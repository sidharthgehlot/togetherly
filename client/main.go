package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	webview "github.com/jchv/go-webview2"
	"github.com/gorilla/websocket"
	"golang.org/x/sys/windows"
)

const (
	vlcBase       = "http://localhost:8080/requests/status.json"
	vlcPass       = "togetherly"
	pollInterval  = 500 * time.Millisecond
	seekThreshold = 3.0
	serverURL     = "wss://togetherly-eqpo.onrender.com/ws"

	// Win32
	wmClose      = 0x0010
	wmTrayIcon   = 0x8001 // WM_APP + 1
	wmLButtonUp  = 0x0202
	gwlpWndProc  = ^uintptr(3) // -4
	swHide       = 0
	swRestore    = 9
	nimAdd       = 0
	nimDelete    = 2
	nifMessage   = 1
	nifIcon      = 2
	nifTip       = 4
	idiApp       = uintptr(32512)
)

// ── Win32 lazy procs ──────────────────────────────────────────────────────────

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")

	setWinLong   = user32.NewProc("SetWindowLongPtrW")
	getWinLong   = user32.NewProc("GetWindowLongPtrW")
	callWinProc  = user32.NewProc("CallWindowProcW")
	showWin      = user32.NewProc("ShowWindow")
	setForeground = user32.NewProc("SetForegroundWindow")
	loadIcon     = user32.NewProc("LoadIconW")
	shellNotify  = shell32.NewProc("Shell_NotifyIconW")
)

// notifyIconData maps to NOTIFYICONDATAW
type notifyIconData struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	_                [4]byte // GUID padding
}

// ── Global state ──────────────────────────────────────────────────────────────

var (
	gView       webview.WebView
	gHWND       uintptr
	origWndProc uintptr

	ignoreMu    sync.Mutex
	ignoreUntil time.Time
)

// ── UI helpers ────────────────────────────────────────────────────────────────

func evalUI(js string) {
	if gView != nil {
		gView.Dispatch(func() { gView.Eval(js) })
	}
}

func jsStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// ── Loop prevention ───────────────────────────────────────────────────────────

func isIgnoring() bool {
	ignoreMu.Lock()
	defer ignoreMu.Unlock()
	return time.Now().Before(ignoreUntil)
}

func setIgnore() {
	ignoreMu.Lock()
	defer ignoreMu.Unlock()
	ignoreUntil = time.Now().Add(1500 * time.Millisecond)
}

// ── VLC ───────────────────────────────────────────────────────────────────────

type vlcStatus struct {
	Time  int    `json:"time"`
	State string `json:"state"`
}

type syncEvent struct {
	Type string  `json:"type"`
	Time float64 `json:"time"`
}

func getVLCStatus() (*vlcStatus, error) {
	client := &http.Client{Timeout: 1 * time.Second}
	req, _ := http.NewRequest("GET", vlcBase, nil)
	req.SetBasicAuth("", vlcPass)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var s vlcStatus
	return &s, json.Unmarshal(body, &s)
}

func vlcCommand(cmd string) {
	client := &http.Client{Timeout: 1 * time.Second}
	req, _ := http.NewRequest("GET", vlcBase+"?"+cmd, nil)
	req.SetBasicAuth("", vlcPass)
	client.Do(req)
}

func applyEvent(e syncEvent) {
	setIgnore()
	switch e.Type {
	case "seek":
		vlcCommand(fmt.Sprintf("command=seek&val=%d", int(e.Time)))
	case "pause":
		vlcCommand(fmt.Sprintf("command=seek&val=%d", int(e.Time)))
		time.Sleep(80 * time.Millisecond)
		if s, err := getVLCStatus(); err == nil && s.State == "playing" {
			vlcCommand("command=pl_pause")
		}
	case "play":
		vlcCommand(fmt.Sprintf("command=seek&val=%d", int(e.Time)))
		time.Sleep(80 * time.Millisecond)
		if s, err := getVLCStatus(); err == nil && s.State != "playing" {
			vlcCommand("command=pl_pause")
		}
	}
	evalUI(fmt.Sprintf("syncEvent('recv',%s,%s)", jsStr(e.Type), jsStr(fmtTime(e.Time))))
}

// ── VLC auto-setup ────────────────────────────────────────────────────────────

func autoConfigureVLC() error {
	path := filepath.Join(os.Getenv("APPDATA"), "vlc", "vlcrc")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			os.MkdirAll(filepath.Dir(path), 0755)
			return os.WriteFile(path, []byte("extraintf=http\nhttp-password=togetherly\n"), 0644)
		}
		return err
	}
	content := string(data)
	extraRe := regexp.MustCompile(`(?m)^extraintf=(.*)$`)
	if extraRe.MatchString(content) {
		content = extraRe.ReplaceAllStringFunc(content, func(match string) string {
			val := strings.TrimPrefix(match, "extraintf=")
			if strings.Contains(val, "http") {
				return match
			}
			if val == "" {
				return "extraintf=http"
			}
			return "extraintf=" + val + ":http"
		})
	} else {
		content += "\nextraintf=http\n"
	}
	passRe := regexp.MustCompile(`(?m)^http-password=.*$`)
	if passRe.MatchString(content) {
		content = passRe.ReplaceAllString(content, "http-password=togetherly")
	} else {
		content += "http-password=togetherly\n"
	}
	return os.WriteFile(path, []byte(content), 0644)
}

func watchVLC() {
	configured := false
	for {
		if _, err := getVLCStatus(); err == nil {
			evalUI("vlcStatus(true, 'VLC connected')")
			time.Sleep(3 * time.Second)
			continue
		}
		if !configured {
			autoConfigureVLC()
			configured = true
		}
		evalUI("vlcStatus(false, 'Waiting for VLC — please open VLC')")
		time.Sleep(2 * time.Second)
	}
}

// ── Sync loop ─────────────────────────────────────────────────────────────────

func connectAndSync(code string, isHost bool) {
	if isHost {
		evalUI(fmt.Sprintf("roomCreated(%s)", jsStr(code)))
	} else {
		evalUI(fmt.Sprintf("roomJoined(%s)", jsStr(code)))
	}

	conn, _, err := websocket.DefaultDialer.Dial(serverURL, nil)
	if err != nil {
		evalUI(fmt.Sprintf("showError(%s)", jsStr("Cannot reach server — check internet")))
		return
	}

	join, _ := json.Marshal(map[string]string{"type": "join", "room": code})
	conn.WriteMessage(websocket.TextMessage, join)

	go func() {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				evalUI("showError('Disconnected from server')")
				return
			}
			var raw map[string]interface{}
			if json.Unmarshal(msg, &raw) != nil {
				continue
			}
			if raw["type"] == "partner_joined" {
				evalUI("partnerJoined()")
				continue
			}
			var e syncEvent
			if json.Unmarshal(msg, &e) == nil && e.Type != "" {
				applyEvent(e)
			}
		}
	}()

	var lastState string
	var lastTime float64
	var lastTick time.Time

	for {
		time.Sleep(pollInterval)
		s, err := getVLCStatus()
		if err != nil {
			continue
		}
		now := time.Now()
		cur := float64(s.Time)

		if lastState == "" {
			lastState, lastTime, lastTick = s.State, cur, now
			continue
		}
		if isIgnoring() {
			lastState, lastTime, lastTick = s.State, cur, now
			continue
		}

		elapsed := now.Sub(lastTick).Seconds()
		expected := lastTime
		if lastState == "playing" {
			expected += elapsed
		}
		drift := math.Abs(cur - expected)

		var event *syncEvent
		switch {
		case s.State != lastState:
			t := "play"
			if s.State == "paused" {
				t = "pause"
			}
			event = &syncEvent{Type: t, Time: cur}
		case drift > seekThreshold:
			event = &syncEvent{Type: "seek", Time: cur}
		}

		if event != nil {
			data, _ := json.Marshal(event)
			conn.WriteMessage(websocket.TextMessage, data)
			evalUI(fmt.Sprintf("syncEvent('sent',%s,%s)", jsStr(event.Type), jsStr(fmtTime(event.Time))))
		}

		lastState, lastTime, lastTick = s.State, cur, now
	}
}

// ── Tray icon ─────────────────────────────────────────────────────────────────

func addTrayIcon(hwnd uintptr) {
	icon, _, _ := loadIcon.Call(0, idiApp)
	var nid notifyIconData
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = hwnd
	nid.UID = 1
	nid.UFlags = nifMessage | nifIcon | nifTip
	nid.UCallbackMessage = wmTrayIcon
	nid.HIcon = icon
	tip := windows.StringToUTF16("togetherly — click to open")
	copy(nid.SzTip[:], tip)
	shellNotify.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
}

func removeTrayIcon(hwnd uintptr) {
	var nid notifyIconData
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = hwnd
	nid.UID = 1
	shellNotify.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
}

// ── Window subclass ───────────────────────────────────────────────────────────

func wndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	switch msg {
	case wmClose:
		// Hide to tray instead of closing
		showWin.Call(hwnd, swHide)
		return 0
	case wmTrayIcon:
		if lParam == wmLButtonUp {
			showWin.Call(hwnd, swRestore)
			setForeground.Call(hwnd)
		}
		return 0
	}
	r, _, _ := callWinProc.Call(origWndProc, hwnd, msg, wParam, lParam)
	return r
}

func hookWindow(hwnd uintptr) {
	addTrayIcon(hwnd)
	cb := syscall.NewCallback(wndProc)
	r, _, _ := getWinLong.Call(hwnd, gwlpWndProc)
	origWndProc = r
	setWinLong.Call(hwnd, gwlpWndProc, cb)
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	runtime.LockOSThread()

	w := webview.New(false)
	defer func() {
		removeTrayIcon(gHWND)
		w.Destroy()
	}()
	gView = w

	w.SetTitle("togetherly")
	w.SetSize(380, 520, webview.HintFixed)
	w.SetHtml(htmlUI)

	w.Bind("go_createRoom", func() {
		code := fmt.Sprintf("%04d", rand.Intn(10000))
		go connectAndSync(code, true)
	})
	w.Bind("go_joinRoom", func(code string) {
		go connectAndSync(code, false)
	})
	w.Bind("go_quit", func() {
		removeTrayIcon(gHWND)
		w.Terminate()
	})

	// Hook window after first event loop tick (window is created by then)
	w.Dispatch(func() {
		gHWND = uintptr(w.Window())
		hookWindow(gHWND)
	})

	go watchVLC()

	w.Run()
}

func fmtTime(secs float64) string {
	s := int(secs)
	return fmt.Sprintf("%d:%02d:%02d", s/3600, (s%3600)/60, s%60)
}

// ── HTML UI ───────────────────────────────────────────────────────────────────

const htmlUI = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<style>
  * { margin:0; padding:0; box-sizing:border-box; }
  html, body { height:100%; }

  body {
    font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif;
    background: #fdf2f8;
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    padding: 32px 28px;
    gap: 0;
    user-select: none;
    -webkit-user-select: none;
  }

  h1 {
    font-size: 34px;
    font-weight: 900;
    background: linear-gradient(135deg, #e91e63, #9c27b0);
    -webkit-background-clip: text;
    -webkit-text-fill-color: transparent;
    background-clip: text;
    letter-spacing: -1.5px;
    margin-bottom: 4px;
  }

  .tagline {
    font-size: 12px;
    color: #ccc;
    margin-bottom: 24px;
    letter-spacing: 0.3px;
  }

  /* VLC pill */
  .pill {
    display: inline-flex;
    align-items: center;
    gap: 7px;
    padding: 6px 14px;
    border-radius: 20px;
    background: #f0f0f0;
    font-size: 12px;
    color: #999;
    margin-bottom: 28px;
    transition: all 0.3s;
  }
  .pill.ok  { background: #e8f5e9; color: #2e7d32; }
  .pill.err { background: #fce4ec; color: #b71c1c; }
  .dot {
    width: 7px; height: 7px; border-radius: 50%; background: #ccc; flex-shrink: 0;
  }
  .ok  .dot { background: #43a047; }
  .err .dot { background: #e53935; animation: blink 1.2s infinite; }
  @keyframes blink { 0%,100%{opacity:1} 50%{opacity:.25} }

  /* Buttons */
  .btn {
    width: 100%; padding: 14px; border-radius: 14px; border: none;
    font-size: 15px; font-weight: 600; cursor: pointer; transition: all .15s;
    margin-bottom: 10px;
  }
  .btn:last-child { margin-bottom: 0; }
  .btn-pink {
    background: linear-gradient(135deg, #f06292, #c2185b);
    color: #fff;
    box-shadow: 0 4px 14px rgba(194,24,91,.22);
  }
  .btn-purple {
    background: linear-gradient(135deg, #ce93d8, #7b1fa2);
    color: #fff;
    box-shadow: 0 4px 14px rgba(123,31,162,.2);
  }
  .btn-pink:hover   { transform: translateY(-1px); box-shadow: 0 6px 20px rgba(194,24,91,.32); }
  .btn-purple:hover { transform: translateY(-1px); box-shadow: 0 6px 20px rgba(123,31,162,.28); }
  .btn-ghost {
    background: none; color: #bbb; font-size: 13px; padding: 8px;
  }
  .btn-ghost:hover { color: #999; }

  /* Code box */
  .code-box {
    background: linear-gradient(135deg, #fce4ec, #f3e5f5);
    border-radius: 18px;
    padding: 20px 16px 16px;
    margin-bottom: 18px;
    width: 100%;
  }
  .code-label { font-size: 11px; text-transform: uppercase; letter-spacing:1.5px; color:#ce93d8; margin-bottom:8px; }
  .code-digits { font-size:54px; font-weight:900; letter-spacing:16px; color:#7b1fa2; line-height:1; }
  .code-hint { font-size:11px; color:#ce93d8; margin-top:8px; }

  /* Input */
  .code-input {
    width: 100%; padding: 14px 8px;
    border: 2.5px solid #f8bbd0; border-radius: 14px;
    font-size: 38px; font-weight: 800; text-align: center;
    letter-spacing: 14px; color: #c2185b; background: #fff9fc;
    outline: none; margin-bottom: 12px; transition: border-color .2s;
  }
  .code-input:focus { border-color: #9c27b0; }
  .code-input::placeholder { color:#f8bbd0; letter-spacing:8px; font-size:30px; }

  /* Status + events */
  .status {
    font-size: 14px; font-weight: 500; color: #e91e63;
    min-height: 20px; margin-bottom: 10px; text-align: center;
  }
  @keyframes pulse { 0%,100%{opacity:1} 50%{opacity:.4} }
  .pulse { animation: pulse 2s ease-in-out infinite; }

  .events { width: 100%; max-height: 64px; overflow: hidden; margin-bottom: 14px; }
  .ev { font-size: 12px; padding: 1px 0; color: #ddd; }
  .ev.sent { color: #f06292; }
  .ev.recv { color: #ba68c8; }

  /* Screens */
  .screen { display:none; width:100%; }
  .screen.on { display:flex; flex-direction:column; align-items:center; width:100%; }
</style>
</head>
<body>

<h1>togetherly</h1>
<p class="tagline">watch together, feel together</p>

<div class="pill" id="pill"><div class="dot"></div><span id="pillTxt">checking VLC...</span></div>

<!-- Home -->
<div class="screen on" id="sHome">
  <button class="btn btn-pink"   onclick="createRoom()">Create room</button>
  <button class="btn btn-purple" onclick="showJoin()">Join room</button>
</div>

<!-- Join -->
<div class="screen" id="sJoin">
  <input class="code-input" id="codeIn" type="text" maxlength="4" placeholder="----" autocomplete="off">
  <button class="btn btn-purple" onclick="joinRoom()">Join &#9825;</button>
  <button class="btn btn-ghost"  onclick="show('sHome')">Back</button>
</div>

<!-- Room -->
<div class="screen" id="sRoom">
  <div class="code-box" id="codeBox" style="display:none">
    <div class="code-label">room code — share with your partner</div>
    <div class="code-digits" id="codeDigits">----</div>
  </div>
  <div class="status pulse" id="roomStatus">Waiting for partner...</div>
  <div class="events" id="events"></div>
  <button class="btn btn-ghost" onclick="window.go_quit()">Quit</button>
</div>

<script>
function show(id) {
  document.querySelectorAll('.screen').forEach(s => s.classList.replace('on','') || s.classList.remove('on'));
  document.getElementById(id).classList.add('on');
}
function showJoin() { show('sJoin'); document.getElementById('codeIn').focus(); }

function createRoom() { window.go_createRoom(); }
function joinRoom() {
  const c = document.getElementById('codeIn').value;
  if (c.length === 4) window.go_joinRoom(c);
}

document.getElementById('codeIn').addEventListener('input', function() {
  this.value = this.value.replace(/\D/g,'').slice(0,4);
  if (this.value.length === 4) joinRoom();
});

// Called from Go
function vlcStatus(ok, msg) {
  const p = document.getElementById('pill');
  p.className = 'pill ' + (ok ? 'ok' : 'err');
  document.getElementById('pillTxt').textContent = msg;
}
function roomCreated(code) {
  document.getElementById('codeDigits').textContent = code;
  document.getElementById('codeBox').style.display = 'block';
  setStatus('Waiting for partner...', true);
  show('sRoom');
}
function roomJoined(code) {
  setStatus('Connecting...', false);
  show('sRoom');
}
function partnerJoined() { setStatus('Partner connected ♥ Syncing...', false); }
function syncEvent(dir, event, time) {
  setStatus(dir==='recv' ? 'Partner '+event+'d ♥' : 'Synced ♥', false);
  const d = document.createElement('div');
  d.className = 'ev ' + dir;
  d.textContent = (dir==='sent' ? '  you' : 'them') + '  ' + event + '  ' + time;
  const log = document.getElementById('events');
  log.prepend(d);
  while (log.children.length > 4) log.lastChild.remove();
}
function showError(msg) { setStatus(msg, false); }

function setStatus(msg, pulse) {
  const el = document.getElementById('roomStatus');
  el.textContent = msg;
  el.className = 'status' + (pulse ? ' pulse' : '');
}
</script>
</body>
</html>`

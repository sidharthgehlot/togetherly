package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/gorilla/websocket"
	webview "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

const (
	vlcBase       = "http://localhost:8080/requests/status.json"
	vlcPass       = "togetherly"
	pollInterval  = 500 * time.Millisecond
	seekThreshold = 3.0
	serverURL     = "wss://togetherly-eqpo.onrender.com/ws"

	// Win32
	wmClose         = 0x0010
	wmTrayIcon      = 0x8001
	wmLButtonUp     = 0x0202
	wmSetIcon       = 0x0080
	gwlpWndProc     = ^uintptr(3)
	swHide          = 0
	swRestore       = 9
	nimAdd          = 0
	nimDelete       = 2
	nifMessage      = 1
	nifIcon         = 2
	nifTip          = 4
	idiApp          = uintptr(32512)
	iconSmall       = 0
	iconBig         = 1
	imageIcon       = 1
	lrLoadFromFile  = 0x00000010
)

// ── Win32 ─────────────────────────────────────────────────────────────────────

var (
	user32  = windows.NewLazySystemDLL("user32.dll")
	shell32 = windows.NewLazySystemDLL("shell32.dll")

	setWinLong    = user32.NewProc("SetWindowLongPtrW")
	getWinLong    = user32.NewProc("GetWindowLongPtrW")
	callWinProc   = user32.NewProc("CallWindowProcW")
	showWin       = user32.NewProc("ShowWindow")
	setForeground = user32.NewProc("SetForegroundWindow")
	loadIcon      = user32.NewProc("LoadIconW")
	loadImage     = user32.NewProc("LoadImageW")
	sendMessage   = user32.NewProc("SendMessageW")
	shellNotify   = shell32.NewProc("Shell_NotifyIconW")
)

// gAppIcon holds the HICON we generate from a heart shape at startup.
var gAppIcon uintptr

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
	_                [4]byte
}

// ── State ─────────────────────────────────────────────────────────────────────

var (
	gView       webview.WebView
	gHWND       uintptr
	origWndProc uintptr

	ignoreMu    sync.Mutex
	ignoreUntil time.Time
)

func evalUI(js string) {
	if gView != nil {
		gView.Dispatch(func() { gView.Eval(js) })
	}
}

func jsStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

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
		evalUI("vlcStatus(false, 'Waiting for VLC')")
		time.Sleep(2 * time.Second)
	}
}

// ── Sync ──────────────────────────────────────────────────────────────────────

func connectAndSync(code string, isHost bool) {
	if isHost {
		evalUI(fmt.Sprintf("roomCreated(%s)", jsStr(code)))
	} else {
		evalUI(fmt.Sprintf("roomJoined(%s)", jsStr(code)))
	}

	conn, _, err := websocket.DefaultDialer.Dial(serverURL, nil)
	if err != nil {
		evalUI(fmt.Sprintf("showError(%s)", jsStr("Cannot reach server")))
		return
	}

	join, _ := json.Marshal(map[string]string{"type": "join", "room": code})
	conn.WriteMessage(websocket.TextMessage, join)

	go func() {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				evalUI("showError('Disconnected')")
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

// ── Heart icon (generated at runtime, no .ico file needed) ───────────────────

// makeHeartIcon writes a heart-shaped .ico file to temp dir and returns the path.
// The shape uses the parametric heart curve: (x²+y²-1)³ - x²y³ < 0.
func makeHeartIcon() (string, error) {
	const sz = 32
	img := image.NewRGBA(image.Rect(0, 0, sz, sz))

	// Center the heart, scale so it fits comfortably with breathing room
	cx, cy := float64(sz)/2-0.5, float64(sz)/2+1
	scale := float64(sz) * 0.40

	for y := 0; y < sz; y++ {
		for x := 0; x < sz; x++ {
			fx := (float64(x) - cx) / scale
			fy := -(float64(y) - cy) / scale
			xx := fx * fx
			v := math.Pow(xx+fy*fy-1, 3) - xx*math.Pow(fy, 3)

			if v < 0 {
				// Pink → purple gradient based on diagonal position
				t := (float64(x) + float64(y)) / float64(2*sz)
				r := uint8(236 + (168-236)*t) // ec → a8
				g := uint8(72 + (85-72)*t)
				b := uint8(153 + (247-153)*t) // 99 → f7
				img.Set(x, y, color.RGBA{r, g, b, 255})
			}
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", err
	}
	pngData := buf.Bytes()

	// Wrap PNG in .ico container (Vista+ supports PNG-encoded icons)
	var ico bytes.Buffer
	binary.Write(&ico, binary.LittleEndian, uint16(0)) // reserved
	binary.Write(&ico, binary.LittleEndian, uint16(1)) // type ICO
	binary.Write(&ico, binary.LittleEndian, uint16(1)) // count
	ico.WriteByte(sz)
	ico.WriteByte(sz)
	ico.WriteByte(0) // colors
	ico.WriteByte(0) // reserved
	binary.Write(&ico, binary.LittleEndian, uint16(1))               // planes
	binary.Write(&ico, binary.LittleEndian, uint16(32))              // bpp
	binary.Write(&ico, binary.LittleEndian, uint32(len(pngData)))    // bytes
	binary.Write(&ico, binary.LittleEndian, uint32(22))              // offset
	ico.Write(pngData)

	icoPath := filepath.Join(appDataDir(), "togetherly.ico")
	if err := os.WriteFile(icoPath, ico.Bytes(), 0644); err != nil {
		return "", err
	}
	return icoPath, nil
}

// appDataDir returns %LOCALAPPDATA%\togetherly, creating it if needed.
func appDataDir() string {
	dir := filepath.Join(os.Getenv("LOCALAPPDATA"), "togetherly")
	os.MkdirAll(dir, 0755)
	return dir
}

// psEscape escapes a string for safe insertion inside a PowerShell single-quoted literal.
func psEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// ensureDesktopShortcut creates a togetherly.lnk on the user's desktop on first run.
// Idempotent — uses a marker file in AppData so it only runs once.
func ensureDesktopShortcut(icoPath string) {
	marker := filepath.Join(appDataDir(), ".shortcut-created")
	if _, err := os.Stat(marker); err == nil {
		return
	}

	exePath, err := os.Executable()
	if err != nil {
		return
	}

	script := fmt.Sprintf(`
$desktop = [Environment]::GetFolderPath('Desktop')
$path = Join-Path $desktop 'togetherly.lnk'
$sh = New-Object -ComObject WScript.Shell
$s = $sh.CreateShortcut($path)
$s.TargetPath = '%s'
$s.IconLocation = '%s,0'
$s.WorkingDirectory = '%s'
$s.Description = 'togetherly — watch together, feel together'
$s.Save()
`, psEscape(exePath), psEscape(icoPath), psEscape(filepath.Dir(exePath)))

	cmd := exec.Command("powershell",
		"-NoProfile", "-NonInteractive",
		"-ExecutionPolicy", "Bypass",
		"-WindowStyle", "Hidden",
		"-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	if err := cmd.Run(); err == nil {
		os.WriteFile(marker, []byte("1"), 0644)
	}
}

// loadIconFromFile loads an .ico file and returns its HICON.
func loadIconFromFile(path string) uintptr {
	pathPtr, _ := windows.UTF16PtrFromString(path)
	icon, _, _ := loadImage.Call(
		0,
		uintptr(unsafe.Pointer(pathPtr)),
		imageIcon,
		32, 32,
		lrLoadFromFile,
	)
	return icon
}

func setWindowIcon(hwnd, icon uintptr) {
	if icon == 0 {
		return
	}
	sendMessage.Call(hwnd, wmSetIcon, iconSmall, icon)
	sendMessage.Call(hwnd, wmSetIcon, iconBig, icon)
}

// ── Tray + window subclass ───────────────────────────────────────────────────

func addTrayIcon(hwnd uintptr) {
	icon := gAppIcon
	if icon == 0 {
		icon, _, _ = loadIcon.Call(0, idiApp)
	}
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

func wndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	switch msg {
	case wmClose:
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
	setWindowIcon(hwnd, gAppIcon)
	addTrayIcon(hwnd)
	cb := syscall.NewCallback(wndProc)
	r, _, _ := getWinLong.Call(hwnd, gwlpWndProc)
	origWndProc = r
	setWinLong.Call(hwnd, gwlpWndProc, cb)
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	runtime.LockOSThread()

	// Generate the heart icon, load it as an HICON, drop a desktop shortcut on first run
	if icoPath, err := makeHeartIcon(); err == nil {
		gAppIcon = loadIconFromFile(icoPath)
		go ensureDesktopShortcut(icoPath)
	}

	// Local HTTP server serving the UI (more reliable than SetHtml)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	uiURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(htmlUI))
	})
	go http.Serve(listener, mux)

	w := webview.New(false)
	defer func() {
		removeTrayIcon(gHWND)
		w.Destroy()
	}()
	gView = w

	w.SetTitle("togetherly")
	w.SetSize(400, 580, webview.HintFixed) // sized for the tallest screen (Room with code)

	// Register bindings BEFORE navigation so they're available on first load
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

	w.Navigate(uiURL)

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
<title>togetherly</title>
<style>
  * { margin:0; padding:0; box-sizing:border-box; -webkit-tap-highlight-color:transparent; }
  html, body { height:100%; overflow:hidden; }

  :root {
    --pink:        #ec4899;
    --pink-dark:   #be185d;
    --purple:      #a855f7;
    --purple-dark: #7e22ce;
    --bg-1:        #fff5f9;
    --bg-2:        #fdf2f8;
    --bg-3:        #fce8f3;
    --text:        #4a1942;
    --text-soft:   #b29db0;
    --shadow-pink:   0 8px 24px rgba(190, 24, 93, 0.18);
    --shadow-purple: 0 8px 24px rgba(126, 34, 206, 0.18);
  }

  body {
    font-family: 'Segoe UI', -apple-system, BlinkMacSystemFont, sans-serif;
    background: radial-gradient(ellipse at 50% 0%, var(--bg-1) 0%, var(--bg-2) 50%, var(--bg-3) 100%);
    color: var(--text);
    display: flex;
    flex-direction: column;
    align-items: center;
    padding: 28px 32px 24px;
    user-select: none;
    -webkit-user-select: none;
  }

  /* Logo */
  .logo {
    width: 56px;
    height: 56px;
    margin-bottom: 10px;
    filter: drop-shadow(0 6px 14px rgba(190, 24, 93, 0.22));
  }

  h1 {
    font-size: 34px;
    font-weight: 800;
    background: linear-gradient(135deg, var(--pink) 0%, var(--purple-dark) 100%);
    -webkit-background-clip: text;
    -webkit-text-fill-color: transparent;
    background-clip: text;
    letter-spacing: -1.2px;
    line-height: 1.2;
    padding-bottom: 2px;
  }

  .tagline {
    font-size: 11px;
    color: var(--text-soft);
    margin-top: 8px;
    letter-spacing: 0.6px;
    margin-bottom: 22px;
    font-weight: 500;
  }

  /* Status pill */
  .pill {
    display: inline-flex;
    align-items: center;
    gap: 8px;
    padding: 7px 14px;
    border-radius: 100px;
    background: rgba(255, 255, 255, 0.7);
    border: 1px solid rgba(0, 0, 0, 0.04);
    font-size: 11px;
    font-weight: 600;
    color: #999;
    margin-bottom: 30px;
    transition: all 0.3s;
    letter-spacing: 0.2px;
  }
  .pill.ok  { background: rgba(34,197,94,0.12);  color: #15803d; border-color: rgba(34,197,94,0.18); }
  .pill.err { background: rgba(239,68,68,0.12);  color: #b91c1c; border-color: rgba(239,68,68,0.18); }

  .dot { width: 6px; height: 6px; border-radius: 50%; background: #ccc; }
  .ok .dot  { background: #22c55e; box-shadow: 0 0 8px rgba(34,197,94,0.5); }
  .err .dot { background: #ef4444; animation: pulse 1.4s infinite; }

  /* Buttons */
  .btn {
    width: 100%;
    padding: 16px 18px;
    border-radius: 16px;
    border: none;
    font-size: 15px;
    font-weight: 600;
    font-family: inherit;
    cursor: pointer;
    transition: all 0.18s cubic-bezier(0.4, 0, 0.2, 1);
    margin-bottom: 11px;
    color: white;
    position: relative;
    overflow: hidden;
    letter-spacing: 0.2px;
  }
  .btn:focus { outline: none; }
  .btn:active { transform: scale(0.98); }

  .btn-pink {
    background: linear-gradient(135deg, #f472b6 0%, #be185d 100%);
    box-shadow: var(--shadow-pink);
  }
  .btn-pink:hover {
    transform: translateY(-1px);
    box-shadow: 0 12px 28px rgba(190, 24, 93, 0.28);
  }

  .btn-purple {
    background: linear-gradient(135deg, #c084fc 0%, #7e22ce 100%);
    box-shadow: var(--shadow-purple);
  }
  .btn-purple:hover {
    transform: translateY(-1px);
    box-shadow: 0 12px 28px rgba(126, 34, 206, 0.28);
  }

  .btn-ghost {
    background: none;
    color: var(--text-soft);
    font-size: 12px;
    font-weight: 500;
    padding: 10px;
    margin-top: 4px;
    box-shadow: none;
  }
  .btn-ghost:hover { color: #888; }

  /* Code card */
  .code-card {
    background: linear-gradient(135deg, #ffffff 0%, #fdf4f9 100%);
    border: 1px solid rgba(190, 24, 93, 0.08);
    border-radius: 20px;
    padding: 20px 18px 18px;
    margin-bottom: 16px;
    width: 100%;
    text-align: center;
    box-shadow: 0 4px 20px rgba(190, 24, 93, 0.06);
  }
  .code-label {
    font-size: 10px;
    text-transform: uppercase;
    letter-spacing: 1.6px;
    color: var(--purple);
    margin-bottom: 12px;
    font-weight: 700;
  }
  .code-digits {
    font-size: 54px;
    font-weight: 900;
    letter-spacing: 14px;
    background: linear-gradient(135deg, var(--pink-dark), var(--purple-dark));
    -webkit-background-clip: text;
    -webkit-text-fill-color: transparent;
    background-clip: text;
    line-height: 1;
    padding-left: 14px; /* compensate letter-spacing */
  }
  .code-hint {
    font-size: 11px;
    color: var(--text-soft);
    margin-top: 10px;
    font-weight: 500;
  }

  /* Input */
  .code-input {
    width: 100%;
    padding: 16px 8px;
    border: 2px solid rgba(190, 24, 93, 0.15);
    border-radius: 16px;
    font-size: 36px;
    font-weight: 800;
    text-align: center;
    letter-spacing: 14px;
    color: var(--pink-dark);
    background: white;
    outline: none;
    margin-bottom: 12px;
    font-family: inherit;
    transition: all 0.2s;
    padding-left: 22px;
  }
  .code-input:focus {
    border-color: var(--purple);
    box-shadow: 0 0 0 4px rgba(168, 85, 247, 0.12);
  }
  .code-input::placeholder {
    color: rgba(248, 187, 208, 0.6);
    letter-spacing: 8px;
    font-size: 30px;
  }

  /* Status text */
  .status-text {
    font-size: 13px;
    font-weight: 600;
    color: var(--pink-dark);
    text-align: center;
    margin: 4px 0 14px;
    min-height: 18px;
    letter-spacing: 0.2px;
  }
  @keyframes pulse { 0%,100%{opacity:1} 50%{opacity:.45} }
  .pulse { animation: pulse 2s infinite; }

  /* Events */
  .events {
    width: 100%;
    max-height: 60px;
    overflow: hidden;
    margin-bottom: 14px;
    text-align: center;
  }
  .ev {
    font-size: 11px;
    color: #d4c5d0;
    padding: 1px 0;
    font-weight: 500;
    letter-spacing: 0.3px;
  }
  .ev.sent { color: var(--pink); }
  .ev.recv { color: var(--purple); }

  /* Screens */
  .screen { display: none; width: 100%; }
  .screen.on { display: flex; flex-direction: column; align-items: center; }
</style>
</head>
<body>

<svg class="logo" viewBox="0 0 100 100" xmlns="http://www.w3.org/2000/svg">
  <defs>
    <linearGradient id="heartGrad" x1="0%" y1="0%" x2="100%" y2="100%">
      <stop offset="0%"   stop-color="#ec4899"/>
      <stop offset="50%"  stop-color="#d946ef"/>
      <stop offset="100%" stop-color="#a855f7"/>
    </linearGradient>
    <linearGradient id="playGrad" x1="0%" y1="0%" x2="0%" y2="100%">
      <stop offset="0%"   stop-color="#ffffff" stop-opacity="1"/>
      <stop offset="100%" stop-color="#ffffff" stop-opacity="0.85"/>
    </linearGradient>
  </defs>
  <path d="M50,86 C22,64 8,42 22,26 C32,15 46,20 50,32 C54,20 68,15 78,26 C92,42 78,64 50,86 Z"
        fill="url(#heartGrad)"/>
  <path d="M42,40 L42,62 L62,51 Z" fill="url(#playGrad)"/>
</svg>

<h1>togetherly</h1>
<p class="tagline">watch together, feel together</p>

<div class="pill" id="pill">
  <div class="dot"></div>
  <span id="pillTxt">checking VLC...</span>
</div>

<div class="screen on" id="sHome">
  <button class="btn btn-pink"   onclick="createRoom()">Create room</button>
  <button class="btn btn-purple" onclick="showJoin()">Join room</button>
</div>

<div class="screen" id="sJoin">
  <input class="code-input" id="codeIn" type="text" maxlength="4" placeholder="----" autocomplete="off" inputmode="numeric">
  <button class="btn btn-purple" onclick="joinRoom()">Join &#9825;</button>
  <button class="btn btn-ghost"  onclick="show('sHome')">Back</button>
</div>

<div class="screen" id="sRoom">
  <div class="code-card" id="codeBox" style="display:none">
    <div class="code-label">your room code</div>
    <div class="code-digits" id="codeDigits">----</div>
    <div class="code-hint">share with your partner</div>
  </div>
  <div class="status-text pulse" id="roomStatus">Waiting for partner...</div>
  <div class="events" id="events"></div>
  <button class="btn btn-ghost" onclick="window.go_quit && window.go_quit()">Quit</button>
</div>

<script>
function show(id) {
  document.querySelectorAll('.screen').forEach(s => s.classList.remove('on'));
  document.getElementById(id).classList.add('on');
}
function showJoin() { show('sJoin'); document.getElementById('codeIn').focus(); }

function createRoom() {
  if (window.go_createRoom) {
    window.go_createRoom();
  }
}

function joinRoom() {
  const c = document.getElementById('codeIn').value;
  if (c.length === 4 && window.go_joinRoom) {
    window.go_joinRoom(c);
  }
}

document.getElementById('codeIn').addEventListener('input', function() {
  this.value = this.value.replace(/\D/g, '').slice(0, 4);
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
  setStatus('Connecting...', true);
  show('sRoom');
}
function partnerJoined() { setStatus('Partner connected ♥ Syncing...', false); }

function syncEvent(dir, event, time) {
  setStatus(dir === 'recv' ? 'Partner ' + event + 'd ♥' : 'Synced ♥', false);
  const d = document.createElement('div');
  d.className = 'ev ' + dir;
  d.textContent = (dir === 'sent' ? '↗ you' : '↙ them') + '  ' + event + '  ' + time;
  const log = document.getElementById('events');
  log.prepend(d);
  while (log.children.length > 4) log.lastChild.remove();
}
function showError(msg) { setStatus(msg, false); }

function setStatus(msg, pulse) {
  const el = document.getElementById('roomStatus');
  el.textContent = msg;
  el.className = 'status-text' + (pulse ? ' pulse' : '');
}
</script>
</body>
</html>`

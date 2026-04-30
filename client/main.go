package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	vlcBase       = "http://localhost:8080/requests/status.json"
	vlcPass       = "togetherly"
	pollInterval  = 500 * time.Millisecond
	seekThreshold = 3.0
	serverURL     = "wss://togetherly-eqpo.onrender.com/ws"
)

// ── UI connection ─────────────────────────────────────────────────────────────

var (
	uiMu   sync.Mutex
	uiConn *websocket.Conn
)

func sendUI(msg map[string]interface{}) {
	uiMu.Lock()
	defer uiMu.Unlock()
	if uiConn != nil {
		data, _ := json.Marshal(msg)
		uiConn.WriteMessage(websocket.TextMessage, data)
	}
}

// ── Loop prevention ───────────────────────────────────────────────────────────

var (
	ignoreMu    sync.Mutex
	ignoreUntil time.Time
)

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
	sendUI(map[string]interface{}{
		"type": "sync_event", "direction": "received",
		"event": e.Type, "time": fmtTime(e.Time),
	})
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

// watchVLC continuously checks VLC and updates the UI dot
func watchVLC() {
	configured := false
	for {
		if _, err := getVLCStatus(); err == nil {
			sendUI(map[string]interface{}{"type": "vlc_status", "connected": true})
			time.Sleep(3 * time.Second)
			continue
		}
		if !configured {
			autoConfigureVLC()
			configured = true
			sendUI(map[string]interface{}{
				"type": "vlc_status", "connected": false,
				"message": "VLC ready — please restart VLC, then it'll connect automatically",
			})
		} else {
			sendUI(map[string]interface{}{
				"type": "vlc_status", "connected": false,
				"message": "Waiting for VLC...",
			})
		}
		time.Sleep(2 * time.Second)
	}
}

// ── Sync loop ─────────────────────────────────────────────────────────────────

func runSync(renderConn *websocket.Conn) {
	go func() {
		for {
			_, msg, err := renderConn.ReadMessage()
			if err != nil {
				sendUI(map[string]interface{}{"type": "server_disconnected"})
				return
			}
			var raw map[string]interface{}
			if json.Unmarshal(msg, &raw) != nil {
				continue
			}
			if raw["type"] == "partner_joined" {
				sendUI(map[string]interface{}{"type": "partner_joined"})
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
			renderConn.WriteMessage(websocket.TextMessage, data)
			sendUI(map[string]interface{}{
				"type": "sync_event", "direction": "sent",
				"event": event.Type, "time": fmtTime(event.Time),
			})
		}

		lastState, lastTime, lastTick = s.State, cur, now
	}
}

// ── HTTP + UI WebSocket ───────────────────────────────────────────────────────

var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

func handleUI(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	uiMu.Lock()
	uiConn = conn
	uiMu.Unlock()

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			uiMu.Lock()
			uiConn = nil
			uiMu.Unlock()
			return
		}

		var cmd map[string]string
		if json.Unmarshal(msg, &cmd) != nil {
			continue
		}

		switch cmd["type"] {
		case "create_room":
			code := fmt.Sprintf("%04d", rand.Intn(10000))
			go connectToServer(code, true)
		case "join_room":
			go connectToServer(cmd["code"], false)
		}
	}
}

func connectToServer(code string, isHost bool) {
	if isHost {
		sendUI(map[string]interface{}{"type": "room_created", "code": code})
	} else {
		sendUI(map[string]interface{}{"type": "room_joined", "code": code})
	}

	conn, _, err := websocket.DefaultDialer.Dial(serverURL, nil)
	if err != nil {
		sendUI(map[string]interface{}{
			"type": "error", "message": "Cannot reach server — check your internet",
		})
		return
	}

	join, _ := json.Marshal(map[string]string{"type": "join", "room": code})
	conn.WriteMessage(websocket.TextMessage, join)
	runSync(conn)
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	port := listener.Addr().(*net.TCPAddr).Port
	url := fmt.Sprintf("http://127.0.0.1:%d", port)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(htmlUI))
	})
	mux.HandleFunc("/ui", handleUI)

	go http.Serve(listener, mux)
	go watchVLC()

	exec.Command("cmd", "/c", "start", url).Start()

	fmt.Printf("togetherly running at %s\nPress Ctrl+C to quit.\n", url)
	select {}
}

func fmtTime(secs float64) string {
	s := int(secs)
	return fmt.Sprintf("%d:%02d:%02d", s/3600, (s%3600)/60, s%60)
}

// ── UI ────────────────────────────────────────────────────────────────────────

const htmlUI = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>togetherly</title>
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }

  body {
    font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif;
    background: #fdf2f8;
    min-height: 100vh;
    display: flex;
    align-items: center;
    justify-content: center;
  }

  .card {
    background: #fff;
    border-radius: 28px;
    padding: 40px 36px;
    width: 360px;
    box-shadow: 0 12px 48px rgba(194,24,91,0.12);
    text-align: center;
  }

  h1 {
    font-size: 30px;
    font-weight: 800;
    background: linear-gradient(135deg, #e91e63, #9c27b0);
    -webkit-background-clip: text;
    -webkit-text-fill-color: transparent;
    background-clip: text;
    letter-spacing: -1px;
  }

  .tagline {
    font-size: 13px;
    color: #bbb;
    margin-top: 4px;
    margin-bottom: 28px;
  }

  /* VLC status pill */
  .vlc-pill {
    display: inline-flex;
    align-items: center;
    gap: 7px;
    background: #f5f5f5;
    border-radius: 20px;
    padding: 6px 14px;
    font-size: 12px;
    color: #777;
    margin-bottom: 28px;
    transition: all 0.3s;
  }
  .vlc-pill.ok { background: #e8f5e9; color: #388e3c; }
  .vlc-pill.err { background: #fce4ec; color: #c62828; }

  .dot {
    width: 7px; height: 7px;
    border-radius: 50%;
    background: #ccc;
    flex-shrink: 0;
  }
  .ok .dot { background: #4caf50; }
  .err .dot { background: #e53935; animation: blink 1.4s infinite; }

  @keyframes blink { 0%,100%{opacity:1} 50%{opacity:0.3} }

  /* Buttons */
  .btn {
    width: 100%;
    padding: 15px;
    border-radius: 16px;
    border: none;
    font-size: 15px;
    font-weight: 600;
    cursor: pointer;
    transition: all 0.18s;
    margin-bottom: 10px;
  }
  .btn:last-child { margin-bottom: 0; }

  .btn-primary {
    background: linear-gradient(135deg, #f06292, #ab47bc);
    color: #fff;
    box-shadow: 0 4px 16px rgba(194,24,91,0.2);
  }
  .btn-primary:hover:not(:disabled) {
    transform: translateY(-2px);
    box-shadow: 0 8px 24px rgba(194,24,91,0.3);
  }
  .btn-primary:disabled { opacity: 0.45; cursor: default; }

  .btn-ghost {
    background: transparent;
    color: #aaa;
    font-size: 13px;
    padding: 10px;
  }
  .btn-ghost:hover { color: #888; }

  /* Code display */
  .code-box {
    background: linear-gradient(135deg, #fce4ec, #f3e5f5);
    border-radius: 20px;
    padding: 24px 20px 18px;
    margin: 4px 0 20px;
  }
  .code-label {
    font-size: 11px;
    text-transform: uppercase;
    letter-spacing: 1.5px;
    color: #ce93d8;
    margin-bottom: 10px;
  }
  .code-digits {
    font-size: 52px;
    font-weight: 900;
    letter-spacing: 14px;
    color: #8e24aa;
    line-height: 1;
  }
  .code-hint {
    font-size: 12px;
    color: #ce93d8;
    margin-top: 10px;
  }

  /* Code input */
  .code-input {
    width: 100%;
    padding: 16px 10px;
    border: 2.5px solid #f8bbd0;
    border-radius: 16px;
    font-size: 40px;
    font-weight: 800;
    text-align: center;
    letter-spacing: 14px;
    color: #c2185b;
    background: #fff9fc;
    outline: none;
    margin-bottom: 14px;
    transition: border-color 0.2s;
  }
  .code-input:focus { border-color: #ab47bc; }
  .code-input::placeholder { color: #f8bbd0; letter-spacing: 8px; font-size: 32px; }

  /* Sync status */
  .sync-status {
    font-size: 14px;
    color: #e91e63;
    font-weight: 500;
    min-height: 22px;
    margin-bottom: 12px;
  }

  /* Event log */
  .events {
    max-height: 72px;
    overflow: hidden;
    margin-bottom: 16px;
  }
  .ev {
    font-size: 12px;
    padding: 2px 0;
    color: #ccc;
    transition: color 0.3s;
  }
  .ev.sent  { color: #f06292; }
  .ev.recv  { color: #ba68c8; }

  /* Sections */
  .screen { display: none; }
  .screen.active { display: block; }

  /* Pulse for waiting */
  @keyframes pulse { 0%,100%{opacity:1} 50%{opacity:0.45} }
  .pulse { animation: pulse 2s ease-in-out infinite; }
</style>
</head>
<body>
<div class="card">
  <h1>togetherly</h1>
  <p class="tagline">watch together, feel together</p>

  <div class="vlc-pill" id="vlcPill">
    <div class="dot"></div>
    <span id="vlcText">checking VLC...</span>
  </div>

  <!-- Home -->
  <div class="screen active" id="sHome">
    <button class="btn btn-primary" id="btnCreate" onclick="createRoom()">Create room</button>
    <button class="btn btn-primary" style="background:linear-gradient(135deg,#ba68c8,#7b1fa2);margin-top:2px" onclick="showJoin()">Join room</button>
  </div>

  <!-- Join -->
  <div class="screen" id="sJoin">
    <input class="code-input" id="codeInput" type="text" maxlength="4" placeholder="----" autocomplete="off">
    <button class="btn btn-primary" style="background:linear-gradient(135deg,#ba68c8,#7b1fa2)" onclick="joinRoom()">Join &#9825;</button>
    <button class="btn btn-ghost" onclick="show('sHome')">Back</button>
  </div>

  <!-- Room -->
  <div class="screen" id="sRoom">
    <div class="code-box" id="codeBox" style="display:none">
      <div class="code-label">room code</div>
      <div class="code-digits" id="codeDigits">----</div>
      <div class="code-hint">share this with your partner</div>
    </div>
    <div class="sync-status pulse" id="syncStatus">Waiting for partner...</div>
    <div class="events" id="events"></div>
    <button class="btn btn-ghost" onclick="location.reload()">Leave room</button>
  </div>
</div>

<script>
const ws = new WebSocket('ws://' + location.host + '/ui');

ws.onmessage = ({data}) => {
  const m = JSON.parse(data);

  if (m.type === 'vlc_status') {
    const pill = document.getElementById('vlcPill');
    const txt  = document.getElementById('vlcText');
    if (m.connected) {
      pill.className = 'vlc-pill ok';
      txt.textContent = 'VLC connected';
    } else {
      pill.className = 'vlc-pill err';
      txt.textContent = m.message || 'VLC not found';
    }
  }

  if (m.type === 'room_created') {
    document.getElementById('codeDigits').textContent = m.code;
    document.getElementById('codeBox').style.display = 'block';
    document.getElementById('syncStatus').textContent = 'Waiting for partner...';
    document.getElementById('syncStatus').classList.add('pulse');
    show('sRoom');
  }

  if (m.type === 'room_joined') {
    document.getElementById('syncStatus').textContent = 'Connecting...';
    show('sRoom');
  }

  if (m.type === 'partner_joined') {
    document.getElementById('syncStatus').textContent = 'Partner connected ♥ Syncing...';
    document.getElementById('syncStatus').classList.remove('pulse');
  }

  if (m.type === 'sync_event') {
    document.getElementById('syncStatus').classList.remove('pulse');
    document.getElementById('syncStatus').textContent =
      m.direction === 'received' ? 'Partner ' + m.event + 'd ♥' : 'Synced ♥';

    const div = document.createElement('div');
    div.className = 'ev ' + (m.direction === 'sent' ? 'sent' : 'recv');
    div.textContent = (m.direction === 'sent' ? '  you' : 'them') + '  ' + m.event + '  ' + m.time;
    const log = document.getElementById('events');
    log.prepend(div);
    while (log.children.length > 4) log.lastChild.remove();
  }

  if (m.type === 'error') {
    document.getElementById('syncStatus').textContent = m.message;
    document.getElementById('syncStatus').classList.remove('pulse');
  }
};

function show(id) {
  document.querySelectorAll('.screen').forEach(s => s.classList.remove('active'));
  document.getElementById(id).classList.add('active');
}

function showJoin() {
  show('sJoin');
  document.getElementById('codeInput').focus();
}

function createRoom() { ws.send(JSON.stringify({type: 'create_room'})); }

function joinRoom() {
  const code = document.getElementById('codeInput').value;
  if (code.length === 4) ws.send(JSON.stringify({type: 'join_room', code}));
}

// Sanitize input + auto-submit on 4 digits
document.getElementById('codeInput').addEventListener('input', function() {
  this.value = this.value.replace(/\D/g, '').slice(0, 4);
  if (this.value.length === 4) joinRoom();
});
</script>
</body>
</html>`

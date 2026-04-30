package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	vlcBase       = "http://localhost:8080/requests/status.json"
	vlcPass       = "togetherly" // auto-written into vlcrc on first run
	pollInterval  = 500 * time.Millisecond
	seekThreshold = 3.0

	// Replace this after deploying to Render
	serverURL = "wss://togetherly.onrender.com/ws"
)

// ── VLC types ────────────────────────────────────────────────────────────────

type vlcStatus struct {
	Time  int    `json:"time"`
	State string `json:"state"` // "playing", "paused", "stopped"
}

type syncEvent struct {
	Type string  `json:"type"` // "play", "pause", "seek"
	Time float64 `json:"time"`
}

// ── Loop-prevention ──────────────────────────────────────────────────────────

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

// ── VLC HTTP API ─────────────────────────────────────────────────────────────

func getStatus() (*vlcStatus, error) {
	req, _ := http.NewRequest("GET", vlcBase, nil)
	req.SetBasicAuth("", vlcPass)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var s vlcStatus
	return &s, json.Unmarshal(body, &s)
}

func sendCommand(cmd string) {
	req, _ := http.NewRequest("GET", vlcBase+"?"+cmd, nil)
	req.SetBasicAuth("", vlcPass)
	http.DefaultClient.Do(req)
}

func applyEvent(e syncEvent) {
	setIgnore()
	switch e.Type {
	case "seek":
		sendCommand(fmt.Sprintf("command=seek&val=%d", int(e.Time)))
	case "pause":
		sendCommand(fmt.Sprintf("command=seek&val=%d", int(e.Time)))
		time.Sleep(80 * time.Millisecond)
		if s, err := getStatus(); err == nil && s.State == "playing" {
			sendCommand("command=pl_pause")
		}
	case "play":
		sendCommand(fmt.Sprintf("command=seek&val=%d", int(e.Time)))
		time.Sleep(80 * time.Millisecond)
		if s, err := getStatus(); err == nil && s.State != "playing" {
			sendCommand("command=pl_pause")
		}
	}
}

// ── VLC auto-setup ───────────────────────────────────────────────────────────

func vlcrcPath() string {
	return filepath.Join(os.Getenv("APPDATA"), "vlc", "vlcrc")
}

func autoConfigureVLC() error {
	path := vlcrcPath()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// VLC not yet opened once — create a minimal config
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				return err
			}
			return os.WriteFile(path, []byte("extraintf=http\nhttp-password=togetherly\n"), 0644)
		}
		return err
	}

	content := string(data)

	// Enable HTTP interface (may already have other interfaces like "rc")
	extraRe := regexp.MustCompile(`(?m)^extraintf=(.*)$`)
	if extraRe.MatchString(content) {
		content = extraRe.ReplaceAllStringFunc(content, func(match string) string {
			val := strings.TrimPrefix(match, "extraintf=")
			if strings.Contains(val, "http") {
				return match // already enabled
			}
			if val == "" {
				return "extraintf=http"
			}
			return "extraintf=" + val + ":http"
		})
	} else {
		content += "\nextraintf=http\n"
	}

	// Set password to "togetherly"
	passRe := regexp.MustCompile(`(?m)^http-password=.*$`)
	if passRe.MatchString(content) {
		content = passRe.ReplaceAllString(content, "http-password=togetherly")
	} else {
		content += "http-password=togetherly\n"
	}

	return os.WriteFile(path, []byte(content), 0644)
}

// ensureVLC checks if VLC HTTP API is reachable. If not, auto-configures and
// asks the user to restart VLC.
func ensureVLC(reader *bufio.Reader) bool {
	if _, err := getStatus(); err == nil {
		return true
	}

	fmt.Println("Setting up VLC sync interface automatically...")
	if err := autoConfigureVLC(); err != nil {
		fmt.Println("Could not auto-configure VLC:", err)
		fmt.Println("Please enable it manually: Tools > Preferences > All > Interface > Main interfaces > Web")
		fmt.Println("Set the password to: togetherly")
		return false
	}

	fmt.Println("Done! Please restart VLC, then press Enter...")
	reader.ReadString('\n')

	// Give VLC a moment to start
	time.Sleep(500 * time.Millisecond)

	if _, err := getStatus(); err != nil {
		fmt.Println("Still can't reach VLC. Make sure VLC is open and try again.")
		return false
	}

	fmt.Println("VLC connected!")
	return true
}

// ── Room code ────────────────────────────────────────────────────────────────

func generateCode() string {
	return fmt.Sprintf("%04d", rand.Intn(10000))
}

// ── Main ─────────────────────────────────────────────────────────────────────

func main() {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("togetherly")
	fmt.Println("----------")
	fmt.Println()

	if !ensureVLC(reader) {
		fmt.Print("\nPress Enter to exit.")
		reader.ReadString('\n')
		return
	}

	fmt.Println("[1] Create room  (generates a code for your partner)")
	fmt.Println("[2] Join room    (enter your partner's code)")
	fmt.Print("\n> ")
	choice, _ := reader.ReadString('\n')
	choice = strings.TrimSpace(choice)

	var roomCode string
	switch choice {
	case "1":
		roomCode = generateCode()
		fmt.Printf("\nYour room code: %s\n", roomCode)
		fmt.Println("Share this with your partner, then wait here.")
	case "2":
		fmt.Print("Enter room code: ")
		roomCode, _ = reader.ReadString('\n')
		roomCode = strings.TrimSpace(roomCode)
	default:
		fmt.Println("Invalid choice.")
		return
	}

	fmt.Println()

	conn, _, err := websocket.DefaultDialer.Dial(serverURL, nil)
	if err != nil {
		fmt.Println("Could not connect to server:", err)
		fmt.Println("Make sure the Render server is deployed and the URL in main.go is correct.")
		fmt.Print("Press Enter to exit.")
		reader.ReadString('\n')
		return
	}
	defer conn.Close()

	join, _ := json.Marshal(map[string]string{"type": "join", "room": roomCode})
	conn.WriteMessage(websocket.TextMessage, join)
	fmt.Printf("Connected! Room: %s — syncing with VLC...\n\n", roomCode)

	// Receive incoming sync events from partner
	go func() {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				fmt.Println("\nDisconnected from server.")
				os.Exit(1)
			}
			var e syncEvent
			if json.Unmarshal(msg, &e) == nil {
				fmt.Printf("  <- %s @ %s\n", e.Type, fmtTime(e.Time))
				applyEvent(e)
			}
		}
	}()

	// Poll VLC and broadcast any state changes
	var lastState string
	var lastTime float64
	var lastTick time.Time

	for {
		time.Sleep(pollInterval)

		s, err := getStatus()
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
			fmt.Printf("  -> %s @ %s\n", event.Type, fmtTime(event.Time))
		}

		lastState, lastTime, lastTick = s.State, cur, now
	}
}

func fmtTime(secs float64) string {
	s := int(secs)
	return fmt.Sprintf("%d:%02d:%02d", s/3600, (s%3600)/60, s%60)
}

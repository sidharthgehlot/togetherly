# togetherly

**Watch movies together in VLC from different places.** togetherly is a tiny Windows app for couples and long-distance partners who want synchronized VLC playback without setting up Discord streams, screen sharing, or complicated media servers.

Create a room, share a 4-digit code, open the same video in VLC, and togetherly keeps play, pause, and seek actions in sync for both people.

## Why togetherly?

- Built for couples watching movies, shows, anime, or videos together remotely
- Works with VLC Media Player on Windows
- Syncs play, pause, and timeline seeking between two people
- Simple 4-digit room codes
- Lightweight Windows EXE
- Runs quietly in the system tray
- Right-click tray menu with Quit
- Automatic update support through GitHub Releases
- Manual "Check for updates" button
- Small relay server for room-based sync

## VLC watch party app

togetherly is designed for people searching for:

- VLC watch party app
- watch movies together with VLC
- long-distance couple movie night app
- synchronized VLC playback
- remote movie sync for couples
- VLC sync over internet
- private watch together app

## How It Works

1. Both people open the same video file in VLC.
2. One person creates a room in togetherly.
3. The other person joins with the 4-digit code.
4. Play, pause, and seek actions sync both ways.

togetherly configures VLC's HTTP interface when needed, using the local VLC web API on your own computer. The server only relays sync events between people in the same room.

## Download

Download the latest Windows build from GitHub Releases:

[Download togetherly.exe](https://github.com/sidharthgehlot/togetherly/releases/latest/download/togetherly.exe)

## Requirements

- Windows
- VLC Media Player
- Both people should have the same video available locally
- Internet connection for the room relay

## Updating

togetherly checks for updates every 3 days and also includes a manual "Check for updates" button. New versions are published through GitHub Releases.

## Roadmap

- Easier setup for first-time VLC users
- Optional room names
- Better reconnect handling
- Friendlier update prompts
- More detailed sync health checks

## Development

The project has two parts:

- `client`: Windows desktop app written in Go using WebView2
- `server`: Node.js WebSocket relay for room sync

Build the Windows app:

```powershell
cd client
go build -ldflags="-s -w -X main.appVersion=0.2.1" -o togetherly.exe .
```

Run the relay server:

```powershell
cd server
npm install
npm start
```

## Privacy

togetherly does not stream your video. It only sends small playback events such as play, pause, seek, and timestamp through the relay server.

## Keywords

VLC watch party, watch together app, couples movie night, long distance relationship app, synchronized VLC playback, VLC sync, remote movie night, Windows watch party, private movie sync, togetherly.

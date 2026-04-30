const http = require('http');
const WebSocket = require('ws');

const server = http.createServer();
const wss = new WebSocket.Server({ server });

// roomId -> Set of connected clients
const rooms = new Map();

wss.on('connection', (ws) => {
  let roomId = null;

  ws.on('message', (data) => {
    try {
      const msg = JSON.parse(data);

      if (msg.type === 'join') {
        roomId = msg.room;
        if (!rooms.has(roomId)) rooms.set(roomId, new Set());
        rooms.get(roomId).add(ws);
        console.log(`joined room "${roomId}" (${rooms.get(roomId).size} connected)`);
        return;
      }

      // Relay event to everyone else in the room
      if (roomId && rooms.has(roomId)) {
        for (const client of rooms.get(roomId)) {
          if (client !== ws && client.readyState === WebSocket.OPEN) {
            client.send(data);
          }
        }
      }
    } catch (_) {}
  });

  ws.on('close', () => {
    if (roomId && rooms.has(roomId)) {
      rooms.get(roomId).delete(ws);
      if (rooms.get(roomId).size === 0) rooms.delete(roomId);
    }
  });
});

const port = process.env.PORT || 3000;
server.listen(port, () => console.log(`togetherly relay on :${port}`));

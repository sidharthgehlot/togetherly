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
        const requestedRoom = msg.room;
        if (!rooms.has(requestedRoom)) rooms.set(requestedRoom, new Set());
        const room = rooms.get(requestedRoom);

        if (room.size >= 2 && !room.has(ws)) {
          ws.send(JSON.stringify({ type: 'room_full' }));
          return;
        }

        roomId = requestedRoom;
        room.add(ws);
        console.log(`joined room "${roomId}" (${room.size} connected)`);

        if (room.size > 1) {
          for (const client of room) {
            if (client.readyState === WebSocket.OPEN) {
              client.send(JSON.stringify({ type: 'partner_joined' }));
            }
          }
        }
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
      const room = rooms.get(roomId);
      room.delete(ws);
      if (room.size === 0) {
        rooms.delete(roomId);
        return;
      }

      for (const client of room) {
        if (client.readyState === WebSocket.OPEN) {
          client.send(JSON.stringify({ type: 'partner_left' }));
        }
      }
    }
  });
});

const port = process.env.PORT || 3000;
server.listen(port, () => console.log(`togetherly relay on :${port}`));

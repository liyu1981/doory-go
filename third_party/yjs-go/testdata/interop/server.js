#!/usr/bin/env node
// Minimal y-websocket server for interop testing.
// Usage: PORT=<n> node server.js
// Writes "READY\n" to stdout when listening.
// Clients connect to ws://localhost:<PORT>/<roomName>

const http = require('http');
const { WebSocketServer } = require('ws');
const Y = require('yjs');

const port = parseInt(process.env.PORT || '4444', 10);
const docs = new Map();

function getDoc(roomName) {
  if (!docs.has(roomName)) {
    docs.set(roomName, new Y.Doc());
  }
  return docs.get(roomName);
}

const server = http.createServer();
const wss = new WebSocketServer({ server });

// y-protocols message types
const messageYjsSyncStep1 = 0;
const messageYjsSyncStep2 = 1;
const messageYjsUpdate = 2;

// lib0 encoding helpers
function writeVarUint(encoder, num) {
  while (num > 0x7f) {
    encoder.push((num & 0x7f) | 0x80);
    num >>>= 7;
  }
  encoder.push(num & 0x7f);
}

function readVarUint(data, pos) {
  let result = 0, shift = 0;
  while (true) {
    const b = data[pos++];
    result |= (b & 0x7f) << shift;
    shift += 7;
    if ((b & 0x80) === 0) break;
  }
  return { value: result >>> 0, pos };
}

function writeVarBytes(encoder, bytes) {
  writeVarUint(encoder, bytes.length);
  for (const b of bytes) encoder.push(b);
}

function readVarBytes(data, pos) {
  const { value: len, pos: p } = readVarUint(data, pos);
  return { value: data.slice(p, p + len), pos: p + len };
}

function encodeMsg(type, payload) {
  const enc = [];
  writeVarUint(enc, type);
  writeVarBytes(enc, payload);
  return Buffer.from(enc);
}

function decodeMsg(data) {
  const arr = new Uint8Array(data);
  const { value: type, pos: p } = readVarUint(arr, 0);
  const { value: payload } = readVarBytes(arr, p);
  return { type, payload };
}

wss.on('connection', (ws, req) => {
  const roomName = (req.url || '/default').replace(/^\//, '') || 'default';
  const doc = getDoc(roomName);
  const clients = new Set(wss.clients);

  // Send server's step1.
  const sv = Y.encodeStateVector(doc);
  ws.send(encodeMsg(messageYjsSyncStep1, sv));

  ws.on('message', (rawData) => {
    const { type, payload } = decodeMsg(rawData);
    if (type === messageYjsSyncStep1) {
      // Reply with step2.
      const update = Y.encodeStateAsUpdate(doc, payload);
      ws.send(encodeMsg(messageYjsSyncStep2, update));
    } else if (type === messageYjsSyncStep2) {
      if (payload.length > 0) {
        Y.applyUpdate(doc, payload);
      }
    } else if (type === messageYjsUpdate) {
      Y.applyUpdate(doc, payload);
      // Broadcast to others.
      const broadcast = encodeMsg(messageYjsUpdate, payload);
      wss.clients.forEach(client => {
        if (client !== ws && client.readyState === 1) {
          client.send(broadcast);
        }
      });
    }
  });
});

server.listen(port, '127.0.0.1', () => {
  process.stdout.write('READY\n');
});

#!/usr/bin/env node

/**
 * BBClaw test client
 * - Connects to a local OpenClaw gateway via WebSocket
 * - Sends node.register handshake
 * - Logs incoming JSON and binary messages
 */

const WebSocket = require('ws');

const GATEWAY_WS = process.env.BBCLAW_GATEWAY_WS || 'ws://127.0.0.1:18789';
const DEVICE_ID = process.env.BBCLAW_DEVICE_ID || 'bbclaw-test-001';
const DEVICE_NAME = process.env.BBCLAW_DEVICE_NAME || 'BBClaw Test Client';

const ws = new WebSocket(GATEWAY_WS);

function sendJsonRpc(method, params, id = undefined) {
  const payload = {
    jsonrpc: '2.0',
    method,
    params,
  };

  if (id !== undefined) payload.id = id;

  ws.send(JSON.stringify(payload));
}

let sessionId = 'pending';

ws.on('open', () => {
  console.log(`[bbclaw-test] connected: ${GATEWAY_WS}`);

  sendJsonRpc(
    'node.register',
    {
      nodeId: DEVICE_ID,
      name: DEVICE_NAME,
      type: 'accessory',
      capabilities: ['audio_input', 'audio_output', 'text_output', 'vibration', 'terminal_access'],
      version: '1.0.0-test',
      auth_token: process.env.BBCLAW_PAIRING_TOKEN || 'dev-token',
    },
    'reg-1'
  );

  // Optional heartbeat for long-running local tests.
  setInterval(() => {
    if (ws.readyState !== WebSocket.OPEN) return;
    sendJsonRpc('node.heartbeat', {
      sessionId,
      ts: new Date().toISOString(),
      status: {
        battery: 0.85,
        rssi: -45,
        is_ptt_active: false,
      },
    });
  }, 15000);
});

ws.on('message', (data, isBinary) => {
  if (isBinary) {
    console.log(`[bbclaw-test] binary message (${data.length} bytes)`);
    return;
  }

  const text = data.toString('utf8');
  try {
    const msg = JSON.parse(text);
    console.log('[bbclaw-test] json:', JSON.stringify(msg, null, 2));
    if (msg?.id === 'reg-1' && msg?.result?.sessionId) {
      sessionId = msg.result.sessionId;
      console.log(`[bbclaw-test] sessionId=${sessionId}`);
      ws.send(Buffer.from('bbclaw-uplink-audio-smoke', 'utf8'));
    }
  } catch {
    console.log('[bbclaw-test] text:', text);
  }
});

ws.on('close', (code, reason) => {
  console.log(`[bbclaw-test] disconnected code=${code} reason=${reason.toString()}`);
});

ws.on('error', (err) => {
  console.error('[bbclaw-test] error:', err.message);
});

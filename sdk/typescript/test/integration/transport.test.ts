// The platform WebSocket against a scripted relay: clients accept relay messages of 32768 bytes and more (SPEC.md
// §3.1). Within today's limits a relay sends nothing this large, so the relay is a few lines of RFC 6455 here.
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { createServer, type IncomingMessage } from "node:http";
import type { Duplex } from "node:stream";
import { test } from "node:test";
import { WatchParty } from "../../src/party.ts";
import { Relays } from "../../src/relays.ts";
import { freePort, stateWhere } from "./harness.ts";

const CODE = "K7MQ2X";
const participant = { peerId: "p1", name: "Ann", client: "Test/1", host: true, connected: true, player: "idle", match: null, offsetMs: 0 };
const room = {
  rev: 1, code: CODE, epoch: 0, phase: "idle", positionMs: 0, atMs: 0, rate: 1, control: "host", locked: false,
  waitingOn: [], participants: [participant],
};
const sockets: Duplex[] = [];

/** `msg` with an unknown member added (receivers ignore it) so that its JSON text is `bytes` long. */
function padded(msg: Record<string, unknown>, bytes: number): string {
  const text = JSON.stringify({ ...msg, pad: "" });
  return JSON.stringify({ ...msg, pad: "x".repeat(bytes - text.length) });
}

/** One unmasked text frame from the server, with the 16- or 64-bit length its size needs. */
function frame(text: string): Buffer {
  const payload = Buffer.from(text);
  const n = payload.length;
  let head: Buffer;
  if (n < 126) head = Buffer.from([0x81, n]);
  else if (n < 65_536) head = Buffer.from([0x81, 126, n >> 8, n & 0xff]);
  else {
    head = Buffer.alloc(10);
    head[0] = 0x81;
    head[1] = 127;
    head.writeBigUInt64BE(BigInt(n), 2);
  }
  return Buffer.concat([head, payload]);
}

/** The client's masked text frames at the start of `buf`, and the bytes after the last whole frame. */
function readFrames(buf: Buffer): [string[], Buffer] {
  const texts: string[] = [];
  for (;;) {
    if (buf.length < 2) break;
    let len = buf[1]! & 0x7f;
    let at = 2;
    if (len === 126) {
      if (buf.length < 4) break;
      len = buf.readUInt16BE(2);
      at = 4;
    } else if (len === 127) {
      if (buf.length < 10) break;
      len = Number(buf.readBigUInt64BE(2));
      at = 10;
    }
    if (buf.length < at + 4 + len) break;
    const mask = buf.subarray(at, at + 4);
    const data = Buffer.from(buf.subarray(at + 4, at + 4 + len)).map((b, i) => b ^ mask[i % 4]!);
    if ((buf[0]! & 0x0f) === 1) texts.push(data.toString());
    buf = buf.subarray(at + 4 + len);
  }
  return [texts, buf];
}

/** Answers pings, and answers `join` with a 40000-byte `joined` followed by a 70000-byte `room`. */
function relay(req: IncomingMessage, socket: Duplex): void {
  sockets.push(socket);
  const accept = createHash("sha1")
    .update(`${req.headers["sec-websocket-key"]}258EAFA5-E914-47DA-95CA-C5AB0DC85B11`)
    .digest("base64");
  socket.write(`HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ${accept}\r\n\r\n`);
  let rest: Buffer = Buffer.alloc(0);
  socket.on("data", (chunk: Buffer) => {
    const [texts, left] = readFrames(Buffer.concat([rest, chunk]));
    rest = left;
    for (const text of texts) {
      const msg = JSON.parse(text) as Record<string, unknown>;
      if (msg["type"] === "ping") socket.write(frame(JSON.stringify({ type: "pong", id: msg["id"], serverTime: 1000 })));
      if (msg["type"] !== "join") continue;
      const joined = { type: "joined", code: CODE, you: { peerId: "p1", token: "t".repeat(43) }, room, media: null, serverTime: 1000 };
      socket.write(frame(padded(joined, 40_000)));
      socket.write(frame(padded({ type: "room", ...room, rev: 2 }, 70_000)));
    }
  });
  socket.on("error", () => {});
}

test("relay messages of 32768 bytes and more are accepted: a 40000-byte joined and a 70000-byte room", async () => {
  const port = await freePort();
  const server = createServer();
  server.on("upgrade", relay);
  const listening = Promise.withResolvers<void>();
  server.listen(port, "127.0.0.1", () => listening.resolve());
  await listening.promise;
  const party = new WatchParty({
    relays: Relays.at(`http://127.0.0.1:${port}`),
    client: "Test/1",
    name: "Ann",
    resolver: { resolve: () => [] },
  });
  try {
    await party.join(CODE);
    assert.equal(party.state.connection, "connected");
    await stateWhere(party, "the 70000-byte room", (s) => s.room?.rev === 2);
  } finally {
    party.close();
    for (const s of sockets) s.destroy();
    server.close();
  }
});

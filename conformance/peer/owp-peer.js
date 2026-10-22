#!/usr/bin/env node
// owp-peer: a scripted Open Watch Party participant on the TypeScript SDK, for driving apps in tests. It imports the
// SDK's TypeScript sources directly, so it needs Node's type stripping (22.18+) and no install or build. See README.md.
import { readFileSync } from "node:fs";
import { setTimeout as delay } from "node:timers/promises";

const OK = 0;
const FAILED = 1;
const USAGE = 2;
const PARTY = 3;

const [major = 0, minor = 0] = process.versions.node.split(".").map(Number);
if (!(major >= 24 || (major === 23 && minor >= 6) || (major === 22 && minor >= 18))) {
  process.stderr.write(`owp-peer: needs Node 22.18 or later to run the SDK's TypeScript sources; this is ${process.version}\n`);
  process.exit(USAGE);
}
const { WatchParty, Relays, checkMedia } = await import("../../sdk/typescript/src/index.ts");
const { version } = JSON.parse(readFileSync(new URL("./package.json", import.meta.url), "utf8"));

const HELP = `usage: owp-peer [options] create [steps…]
       owp-peer [options] join CODE [steps…]

options:
  --relay URL|public  relay base URL (http://127.0.0.1:18200) or the public relays (default: public)
  --name NAME         display name (default: Peer)
  --client APP/VER    client name (default: owp-peer/${version})
  --file MS|none      the file this peer finds for shared media: its duration, or none (default: the media's duration)
  --timeout S         how long wait, accept, decline and end may take (default: 30)
  --json              print events as JSON lines
  --create            with join: make the room, with this peer as host, when no room has the code

steps:
  media JSON|@FILE [MS]   host: share a descriptor, starting at MS; plays a simulated file of its duration
  file MS|none            change the file this peer finds; with none of its files chosen, search again; with one
                          chosen, play the new file at once (matched or mismatched, no prompt), or keep it for none
  accept | decline        answer the mismatch prompt once it appears
  attach | detach         attach or detach the simulated player (attached by default)
  play | pause | seek MS | rate R | offset MS
  stall MS                the simulated player buffers for MS (the script goes on)
  control host|everyone | lock on|off | transfer WHO | kick WHO | clear | end | leave
  sleep MS
  wait COND…              until every condition holds, or fail after --timeout
  expect COND…            every condition holds now, or fail
  stay                    stay in the room until it ends or SIGINT

conditions: phase=playing participants=2 waitingOn=0 position=61000~500 rate=1.5 control=everyone locked=true
  host=WHO connection=connected ended=kicked WHO.player=ready WHO.match=matched WHO.delta=-120 WHO.offset=0
  WHO.connected=true me.position=61000~500 me.drift=0~350 (WHO is a name, a peer ID or me)

exit: 0 done, 1 a wait or expect failed, 2 usage, 3 the party failed or ended unexpectedly`;

class UsageError extends Error {}

// ---------------------------------------------------------------------------------------------------------------------
// Arguments

const ARITY = {
  media: -1, file: 1, accept: 0, decline: 0, attach: 0, detach: 0, play: 0, pause: 0, seek: 1, rate: 1, offset: 1,
  stall: 1, control: 1, lock: 1, transfer: 1, kick: 1, clear: 0, end: 0, leave: 0, sleep: 1, wait: -1, expect: -1,
  stay: 0,
};
const ROOM_KEYS = {
  phase: "text", participants: "number", waitingOn: "number", position: "number", rate: "number", control: "text",
  locked: "text", host: "text", connection: "text", ended: "text",
};
const PEER_KEYS = {
  player: "text", match: "text", delta: "number", offset: "number", connected: "text", position: "number", drift: "number",
};

const isCount = (s) => /^\d+$/.test(s);

function parseArgs(argv) {
  const opts = { relay: "public", name: "Peer", client: `owp-peer/${version}`, file: undefined, timeoutMs: 30_000, json: false, create: false };
  let i = 0;
  const value = (flag, inline) => {
    if (inline !== undefined) return inline;
    if (i + 1 >= argv.length) throw new UsageError(`${flag} needs a value`);
    return argv[++i];
  };
  for (; i < argv.length && argv[i].startsWith("--"); i++) {
    const [flag, inline] = argv[i].includes("=") ? [argv[i].slice(0, argv[i].indexOf("=")), argv[i].slice(argv[i].indexOf("=") + 1)] : [argv[i], undefined];
    switch (flag) {
      case "--relay": opts.relay = value(flag, inline); break;
      case "--name": opts.name = value(flag, inline); break;
      case "--client": opts.client = value(flag, inline); break;
      case "--file": opts.file = parseFile(value(flag, inline)); break;
      case "--timeout": {
        const s = Number(value(flag, inline));
        if (!(s > 0)) throw new UsageError("--timeout needs a number of seconds > 0");
        opts.timeoutMs = s * 1000;
        break;
      }
      case "--json": opts.json = true; break;
      case "--create": opts.create = true; break;
      case "--help": throw new UsageError("");
      default: throw new UsageError(`unknown option ${flag}`);
    }
  }
  const command = argv[i++];
  let code = null;
  if (command === "join") {
    code = argv[i++];
    if (code === undefined) throw new UsageError("join needs a room code");
  } else if (command !== "create") {
    throw new UsageError(command === undefined ? "missing create or join" : `unknown command ${command}`);
  } else if (opts.create) {
    throw new UsageError("--create goes with join");
  }
  const steps = [];
  while (i < argv.length) {
    const name = argv[i++];
    const arity = ARITY[name];
    if (arity === undefined || !Object.hasOwn(ARITY, name)) throw new UsageError(`unknown step ${name}`);
    const args = [];
    if (name === "media") {
      if (i >= argv.length) throw new UsageError("media needs a descriptor");
      args.push(argv[i++]);
      if (i < argv.length && isCount(argv[i])) args.push(argv[i++]);
    } else if (arity === -1) {
      while (i < argv.length && argv[i].includes("=")) args.push(argv[i++]);
      if (args.length === 0) throw new UsageError(`${name} needs at least one condition`);
    } else {
      if (i + arity > argv.length) throw new UsageError(`${name} needs ${arity} argument`);
      args.push(...argv.slice(i, i + arity));
      i += arity;
    }
    steps.push(parseStep(name, args));
  }
  return { opts, command, code, steps };
}

function parseFile(s) {
  if (s === "none") return null;
  if (!isCount(s)) throw new UsageError(`file needs a duration in ms or none, not ${s}`);
  return Number(s);
}

function parseStep(name, args) {
  const step = { name, args };
  const [a] = args;
  const need = (ok, what) => {
    if (!ok) throw new UsageError(`${name} needs ${what}, not ${a}`);
  };
  switch (name) {
    case "media": {
      let media;
      try {
        media = JSON.parse(a.startsWith("@") ? readFileSync(a.slice(1), "utf8") : a);
      } catch (e) {
        throw new UsageError(`media: ${e.message}`);
      }
      const problem = checkMedia(media);
      if (problem !== null) throw new UsageError(`media: ${problem}`);
      step.media = media;
      step.positionMs = args[1] === undefined ? 0 : Number(args[1]);
      break;
    }
    case "file": step.fileMs = parseFile(a); break;
    case "seek": case "stall": case "sleep": need(isCount(a), "a number of ms"); step.ms = Number(a); break;
    case "offset": need(/^-?\d+$/.test(a), "a whole number of ms"); step.ms = Number(a); break;
    case "rate": need(Number.isFinite(Number(a)), "a number"); step.rate = Number(a); break;
    case "control": need(a === "host" || a === "everyone", "host or everyone"); break;
    case "lock": need(a === "on" || a === "off", "on or off"); break;
    case "wait": case "expect": step.conds = args.map(parseCond); break;
  }
  return step;
}

function parseCond(token) {
  const eq = token.indexOf("=");
  const key = token.slice(0, eq);
  const want = token.slice(eq + 1);
  let who = null;
  let field = key;
  let kind = ROOM_KEYS[key];
  if (!Object.hasOwn(ROOM_KEYS, key)) {
    const dot = key.lastIndexOf(".");
    field = key.slice(dot + 1);
    who = key.slice(0, dot);
    kind = Object.hasOwn(PEER_KEYS, field) ? PEER_KEYS[field] : undefined;
    if (dot <= 0 || kind === undefined) throw new UsageError(`unknown condition ${key}`);
    if ((field === "position" || field === "drift") && who !== "me") throw new UsageError(`only me.${field} is known`);
  }
  const cond = { key, who, field, kind, want };
  if (kind === "number") {
    const [v, tol = "0"] = want.split("~");
    cond.value = Number(v);
    cond.tolerance = Number(tol);
    if (v === "" || !Number.isFinite(cond.value) || !(cond.tolerance >= 0)) {
      throw new UsageError(`${key} needs a number, optionally ~tolerance, not ${want}`);
    }
  }
  return cond;
}

// ---------------------------------------------------------------------------------------------------------------------
// Output: one line per event, as words or as JSON

// The fields printed as bare words, in order, after the event type; the rest print as key=value.
const WORDS = {
  created: ["code", "peerId"], joined: ["code", "peerId"], connection: ["connection"], media: ["title"], room: [],
  status: ["peerId", "name"], participant: ["change", "peerId", "name", "client"], action: ["kind"], match: ["match"],
  mismatch: [], player: ["player"], error: ["code"], ended: ["reason"], step: ["step", "args"], fail: ["step"],
};

let json = false;
let holding = true;
const held = [];

function word(v) {
  if (v === null || v === undefined) return "none";
  const s = typeof v === "string" ? v : JSON.stringify(v);
  return /^[^\s"]+$/.test(s) ? s : JSON.stringify(s);
}

function print(event) {
  if (json) {
    process.stdout.write(JSON.stringify(event) + "\n");
    return;
  }
  const words = WORDS[event.type];
  const out = [event.type];
  for (const k of words) out.push(...(Array.isArray(event[k]) ? event[k].map(word) : [word(event[k])]));
  for (const [k, v] of Object.entries(event)) if (k !== "type" && !words.includes(k) && v !== undefined) out.push(`${k}=${word(v)}`);
  process.stdout.write(out.join(" ") + "\n");
}

/** Prints an event; until the party is in its room, holds it so `created`/`joined` comes first. */
function emit(event) {
  if (holding) held.push(event);
  else print(event);
}

function release(first) {
  holding = false;
  if (first !== undefined) print(first);
  for (const e of held.splice(0)) print(e);
}

// ---------------------------------------------------------------------------------------------------------------------
// The simulated player

/**
 * A player whose position runs on the local clock at its rate; commands land at once. `file` is the simulated file it
 * plays, `{durationMs}`, and the ref of the candidate or `setMedia` it was opened for.
 */
class SimPlayer {
  file;
  durationMs;
  paused = true;
  buffering = false;
  firstFrameShown = true;
  rate = 1;
  #anchor = 0;
  #since = performance.now();

  constructor(file) {
    this.file = file;
    this.durationMs = file.durationMs;
  }

  get positionMs() {
    const moved = this.paused || this.buffering ? 0 : (performance.now() - this.#since) * this.rate;
    const at = this.#anchor + moved;
    return Math.floor(this.durationMs === null ? at : Math.min(at, this.durationMs));
  }

  #rebase() {
    this.#anchor = this.positionMs;
    this.#since = performance.now();
  }

  play() {
    this.#rebase();
    this.paused = false;
  }

  pause() {
    this.#rebase();
    this.paused = true;
  }

  seek(positionMs) {
    this.#anchor = positionMs;
    this.#since = performance.now();
  }

  setRate(rate) {
    this.#rebase();
    this.rate = rate;
  }

  stall(on) {
    this.#rebase();
    this.buffering = on;
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// The session

let parsed;
try {
  parsed = parseArgs(process.argv.slice(2));
} catch (e) {
  if (!(e instanceof UsageError)) throw e;
  if (e.message === "") {
    process.stdout.write(`${HELP}\n`);
    process.exit(OK);
  }
  process.stderr.write(`owp-peer: ${e.message} (owp-peer --help shows the usage)\n`);
  process.exit(USAGE);
}
const { opts, command, code, steps } = parsed;
json = opts.json;

const CANDIDATE_ID = "owp-peer";

/** The one file this peer finds: a simulated file of `durationMs`, which is also its ref. */
function candidateOf(durationMs) {
  const file = { durationMs };
  return { id: CANDIDATE_ID, ref: file, durationMs };
}

let party;
const sockets = new Set();
let fileMs = opts.file;
try {
  const relays =
    opts.relay === "public"
      ? Relays.public()
      : Relays.at(opts.relay.replace(/^ws(s?):\/\//, "http$1://").replace(/\/v1\/ws\/?$/, ""));
  party = new WatchParty({
    relays,
    client: opts.client,
    name: opts.name,
    resolver: {
      resolve: (media) => (fileMs === null ? [] : [candidateOf(fileMs ?? media.durationMs)]),
    },
    connect: (url) => {
      const ws = new WebSocket(url);
      sockets.add(ws);
      ws.addEventListener("close", () => sockets.delete(ws));
      return ws;
    },
  });
} catch (e) {
  process.stderr.write(`owp-peer: ${e.message}\n`);
  process.exit(USAGE);
}

let player = null; // the simulated player, once there is a file to play
let choiceFor = null; // the choice `player` was opened for
let autoAttach = true;
let expectEnd = false; // this peer's own leave, end or exit
let staying = false;
let waiting = null; // the conditions of the running wait
let exiting = false;
let current = null; // the running step

const timeoutMs = opts.timeoutMs;

function participant(who) {
  const s = party.state;
  if (who === "me") return s.me;
  return s.room?.participants.find((p) => p.name === who) ?? s.room?.participants.find((p) => p.peerId === who) ?? null;
}

function actual(c) {
  const s = party.state;
  const room = s.room;
  if (c.who === null) {
    switch (c.field) {
      case "phase": return room?.phase ?? null;
      case "participants": return room?.participants.length ?? 0;
      case "waitingOn": return room?.waitingOn.length ?? 0;
      case "position": return party.roomPositionMs();
      case "rate": return room?.rate ?? null;
      case "control": return room?.control ?? null;
      case "locked": return room === null ? null : String(room.locked);
      case "host": return room?.participants.find((p) => p.host) ?? null;
      case "connection": return s.connection;
      case "ended": return s.endReason;
    }
  }
  if (c.field === "position") return player === null ? null : player.positionMs;
  if (c.field === "drift") {
    const room = party.roomPositionMs();
    return player === null || room === null ? null : player.positionMs - (room + party.state.offsetMs);
  }
  const p = participant(c.who);
  if (p === null) return null;
  switch (c.field) {
    case "player": return p.player;
    case "match": return p.match?.state ?? null;
    case "delta": return p.match?.deltaMs ?? null;
    case "offset": return p.offsetMs;
    case "connected": return String(p.connected);
  }
  return null;
}

function holds(c) {
  const v = actual(c);
  if (c.kind === "number") return typeof v === "number" && Math.abs(v - c.value) <= c.tolerance;
  if (c.field === "host") return v === null ? c.want === "none" : c.want === v.name || c.want === v.peerId;
  return (v ?? "none") === c.want;
}

function describe(conds) {
  return conds
    .filter((c) => !holds(c))
    .map((c) => {
      const v = actual(c);
      return `${c.key}=${word(c.field === "host" && v !== null ? v.name : v)}`;
    })
    .join(" ");
}

function fail(reason) {
  emit({ type: "fail", step: current?.name ?? command, reason });
}

/** Runs a party command; a refusal prints as an error and the script goes on. */
function act(fn) {
  try {
    fn();
    return true;
  } catch (e) {
    emit({ type: "error", code: e.code ?? "failed", re: current.name, message: e.message });
    return false;
  }
}

function peerIdOf(who) {
  return participant(who)?.peerId ?? who;
}

/**
 * Opens the simulated file of each new choice and attaches it with that file's ref, as an app does after opening
 * `state.choice`. A player that already has the choice's file open, as the host's does after `media`, isn't reopened:
 * the SDK binds it by its ref.
 */
function follow(s) {
  if (!autoAttach || s.choice === null || s.choice === choiceFor) return;
  choiceFor = s.choice;
  if (player?.file === s.choice.ref) return;
  const p = (player = new SimPlayer(s.choice.ref));
  queueMicrotask(() => {
    if (player === p && autoAttach && !exiting) party.attachPlayer(p, p.file);
  });
}

async function until(ok) {
  const end = performance.now() + timeoutMs;
  for (;;) {
    if (ok()) return true;
    if (exiting || performance.now() >= end) return false;
    await delay(50);
  }
}

async function finish(exitCode) {
  if (exiting) return;
  exiting = true;
  expectEnd = true;
  const released = party.close();
  const closed = [...sockets].map((ws) => new Promise((resolve) => ws.addEventListener("close", resolve)));
  await Promise.race([Promise.all([released, ...closed]), delay(1000)]);
  await new Promise((resolve) => process.stdout.write("", resolve));
  process.exit(exitCode);
}

const statusKey = (p) => JSON.stringify([p.host, p.connected, p.player, p.match, p.offsetMs]);

function report(prev, s) {
  // Ending resets every field; the `ended` event says it once.
  if (s.connection === "ended") return;
  if (s.connection !== prev.connection && (s.connection === "reconnecting" || prev.connection === "reconnecting")) {
    emit({ type: "connection", connection: s.connection });
  }
  if (s.media !== prev.media && JSON.stringify(s.media) !== JSON.stringify(prev.media)) {
    emit(s.media === null ? { type: "media", title: null } : { type: "media", title: s.media.title, kind: s.media.kind, duration: s.media.durationMs });
  }
  const room = s.room;
  if (room !== null && room.rev !== prev.room?.rev) {
    emit({
      type: "room",
      rev: room.rev,
      epoch: room.epoch,
      phase: room.phase,
      position: room.positionMs,
      rate: room.rate,
      control: room.control,
      locked: room.locked,
      host: room.participants.find((p) => p.host)?.name ?? null,
      participants: room.participants.length,
      waitingOn: room.waitingOn.length,
    });
    for (const p of room.participants) {
      const was = prev.room?.participants.find((q) => q.peerId === p.peerId);
      if (was !== undefined && statusKey(was) === statusKey(p)) continue;
      emit({
        type: "status",
        peerId: p.peerId,
        name: p.name,
        host: p.host,
        connected: p.connected,
        player: p.player,
        match: p.match?.state ?? null,
        delta: p.match?.deltaMs,
        offset: p.offsetMs,
      });
    }
  }
  if (s.match?.state !== prev.match?.state || s.match?.deltaMs !== prev.match?.deltaMs) {
    emit({ type: "match", match: s.match?.state ?? null, delta: s.match?.deltaMs });
  }
  if (s.mismatch !== null && s.mismatch !== prev.mismatch) emit({ type: "mismatch", delta: s.mismatch.deltaMs });
  if (s.player !== prev.player) emit({ type: "player", player: s.player });
}

let prev = party.state;
party.on("state", (s) => {
  report(prev, s);
  prev = s;
  follow(s);
});

party.on("event", (e) => {
  switch (e.type) {
    case "action":
      emit({ type: "action", kind: e.kind, by: e.by?.name ?? e.byPeerId ?? "relay" });
      break;
    case "participantJoined":
    case "participantLeft":
      emit({
        type: "participant",
        change: e.type === "participantJoined" ? "joined" : "left",
        peerId: e.participant.peerId,
        name: e.participant.name,
        client: e.participant.client,
      });
      break;
    case "error":
      emit({ type: "error", code: e.error.code, re: e.error.re ?? undefined, message: e.error.message });
      break;
    case "resolveFailed":
      emit({ type: "error", code: "resolve_failed", message: String(e.error?.message ?? e.error) });
      break;
    case "ended":
      emit({ type: "ended", reason: e.reason, error: e.error?.code });
      if (expectEnd) break;
      if (staying) void finish(e.reason === "lost" ? PARTY : OK);
      else if (waiting === null || !waiting.every(holds)) {
        fail(`the party ended: ${e.reason}`);
        void finish(PARTY);
      }
      break;
  }
});

for (const signal of ["SIGINT", "SIGTERM"]) {
  process.on(signal, () => {
    if (!staying) fail("interrupted");
    void finish(staying ? OK : FAILED);
  });
}

async function run(step) {
  const [a] = step.args;
  switch (step.name) {
    case "media": {
      const file = { durationMs: step.media.durationMs };
      choiceFor = party.state.choice;
      if (!act(() => party.setMedia(step.media, file, step.positionMs))) return true;
      player = new SimPlayer(file);
      if (autoAttach) party.attachPlayer(player, file);
      return true;
    }
    case "file": {
      fileMs = step.fileMs;
      const found = party.state.choice?.id === CANDIDATE_ID;
      act(() => party.resolveAgain());
      // With a file of this peer's resolver chosen, searching again only refreshes the candidates (SDK.md §9.1): the
      // user then opens the new file.
      if (found && fileMs !== null) {
        try {
          await party.choose(candidateOf(fileMs));
        } catch (e) {
          emit({ type: "error", code: e.code ?? "failed", re: "file", message: e.message });
        }
      }
      return true;
    }
    case "accept":
    case "decline":
      if (!(await until(() => party.state.mismatch !== null))) {
        fail("no mismatch prompt appeared");
        return false;
      }
      act(() => (step.name === "accept" ? party.acceptMismatch() : party.declineMismatch()));
      return true;
    case "attach":
      autoAttach = true;
      follow(party.state);
      if (player !== null) act(() => party.attachPlayer(player, player.file));
      return true;
    case "detach":
      autoAttach = false;
      act(() => party.detachPlayer());
      return true;
    case "play": act(() => party.play()); return true;
    case "pause": act(() => party.pause()); return true;
    case "seek": act(() => party.seek(step.ms)); return true;
    case "rate": act(() => party.setRate(step.rate)); return true;
    case "offset": act(() => party.setOffset(step.ms)); return true;
    case "stall": {
      const p = player;
      if (p === null) {
        emit({ type: "error", code: "no_player", re: "stall", message: "there is no file to play yet" });
        return true;
      }
      p.stall(true);
      act(() => party.playerChanged());
      setTimeout(() => {
        p.stall(false);
        if (!exiting && player === p) act(() => party.playerChanged());
      }, step.ms);
      return true;
    }
    case "control": act(() => party.setControl(a)); return true;
    case "lock": act(() => party.lock(a === "on")); return true;
    case "transfer": act(() => party.transferHost(peerIdOf(a))); return true;
    case "kick": act(() => party.kick(peerIdOf(a))); return true;
    case "clear": act(() => party.clearMedia()); return true;
    case "end":
      expectEnd = true;
      if (!act(() => party.end())) {
        expectEnd = false;
        return true;
      }
      if (!(await until(() => party.state.connection === "ended"))) {
        fail("the relay did not end the room");
        return false;
      }
      return true;
    case "leave":
      expectEnd = true;
      act(() => party.leave());
      return true;
    case "sleep":
      await delay(step.ms);
      return true;
    case "wait":
      waiting = step.conds;
      try {
        if (await until(() => step.conds.every(holds))) return true;
      } finally {
        waiting = null;
      }
      if (!exiting) fail(describe(step.conds));
      return false;
    case "expect":
      if (step.conds.every(holds)) return true;
      fail(describe(step.conds));
      return false;
    case "stay":
      if (party.state.connection !== "connected" && party.state.connection !== "reconnecting") return true;
      staying = true;
      await new Promise(() => {});
      return true;
  }
  throw new Error(`unhandled step ${step.name}`);
}

try {
  if (command === "create") {
    const roomCode = await party.create();
    release({ type: "created", code: roomCode, peerId: party.state.peerId });
  } else {
    await party.join(code, { create: opts.create });
    release({ type: "joined", code: party.state.room.code, peerId: party.state.peerId });
  }
} catch (e) {
  release();
  if (!exiting) {
    emit({ type: "error", code: e.code ?? "failed", re: command, message: e.message });
    fail(e.message);
    await finish(PARTY);
  }
}

for (const step of steps) {
  if (exiting) break;
  current = step;
  emit({ type: "step", step: step.name, args: step.args });
  if (!(await run(step))) {
    await finish(FAILED);
    break;
  }
}
await finish(OK);

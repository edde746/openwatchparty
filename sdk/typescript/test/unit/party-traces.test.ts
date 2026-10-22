// Runs every trace of conformance/vectors/party.json (SDK.md §17): a real WatchParty against a scripted relay, scripted
// adapters and a scripted resolver, in virtual time. conformance/vectors/README.md (the party section) defines the
// format; this file is its TypeScript driver.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { describe, test } from "node:test";
import { setImmediate as turn } from "node:timers/promises";
import { PartyError } from "../../src/error.ts";
import { WatchParty } from "../../src/party.ts";
import { Relays } from "../../src/relays.ts";
import type { Candidate, Media, PartyEvent, PlayerAdapter, Resolver } from "../../src/types.ts";
import { VECTORS } from "../paths.ts";

type Json = any;

interface Trace {
  name: string;
  input: { setup: Json; steps: Json[] };
  expect: { outputs: Json[] };
}

interface Timer {
  at: number;
  seq: number;
  run: () => void;
}

/**
 * Virtual local time with its own timers, installed as the global setTimeout/clearTimeout the SDK uses (SDK.md §16.8).
 * `settle` runs the party until it is quiet without moving time; `advance` moves time timer by timer, settling after
 * each.
 */
class VirtualTime {
  now: number;
  #seq = 0;
  readonly #timers = new Map<number, Timer>();
  readonly #saved = { setTimeout: globalThis.setTimeout, clearTimeout: globalThis.clearTimeout };

  constructor(startMs: number) {
    this.now = startMs;
  }

  schedule(delayMs: number, run: () => void): number {
    const seq = ++this.#seq;
    this.#timers.set(seq, { at: this.now + Math.max(0, Math.floor(delayMs) || 0), seq, run });
    return seq;
  }

  install(): void {
    const set = (fn: (...args: unknown[]) => void, ms?: number, ...args: unknown[]) => this.schedule(ms ?? 0, () => fn(...args));
    globalThis.setTimeout = set as unknown as typeof setTimeout;
    globalThis.clearTimeout = ((id: unknown) => {
      if (typeof id === "number") this.#timers.delete(id);
    }) as typeof clearTimeout;
  }

  uninstall(): void {
    globalThis.setTimeout = this.#saved.setTimeout;
    globalThis.clearTimeout = this.#saved.clearTimeout;
  }

  /** The earliest timer due at or before `limit`; equal times in the order they were scheduled. */
  #next(limit: number): Timer | null {
    let next: Timer | null = null;
    for (const t of this.#timers.values()) {
      if (t.at <= limit && (next === null || t.at < next.at || (t.at === next.at && t.seq < next.seq))) next = t;
    }
    return next;
  }

  /** Lets every continuation run (setImmediate isn't replaced), then each timer due now, until nothing is left. */
  async settle(): Promise<void> {
    for (;;) {
      await turn();
      const timer = this.#next(this.now);
      if (timer === null) return;
      this.#timers.delete(timer.seq);
      timer.run();
    }
  }

  async advance(ms: number): Promise<void> {
    const end = this.now + ms;
    for (let t = this.#next(end); t !== null; t = this.#next(end)) {
      this.now = t.at;
      await this.settle();
    }
    this.now = end;
    await this.settle();
  }
}

/** Everything the party did in response to one step, per channel. */
interface Outputs {
  send: Json[];
  command: Json[];
  event: Json[];
  resolver: Json[];
  net: Json[];
  done: Json[];
}

const CHANNELS = ["send", "command", "event", "resolver", "net", "done"] as const;

function noOutputs(): Outputs {
  return { send: [], command: [], event: [], resolver: [], net: [], done: [] };
}

/** A relay connection: pings are answered by the driver, everything else the party sends is an output. */
class FakeSocket {
  readyState = 0;
  onopen: ((ev: unknown) => void) | null = null;
  onmessage: ((ev: { data: string }) => void) | null = null;
  onclose: ((ev: { code: number }) => void) | null = null;
  onerror: ((ev: unknown) => void) | null = null;
  readonly url: string;
  readonly #run: Run;

  constructor(url: string, run: Run) {
    this.url = url;
    this.#run = run;
  }

  send(text: string): void {
    const msg = JSON.parse(text);
    if (msg.type === "ping") this.#run.pong(this, msg.id);
    else this.#run.out.send.push({ on: this.url, msg });
  }

  /** The party closing the connection. Only an open connection reports it: one still opening just goes away. */
  close(code?: number): void {
    if (this.readyState === 1) this.#run.out.net.push({ close: this.url, code: code ?? null });
    this.readyState = 3;
  }

  deliver(text: string): void {
    if (this.readyState === 1) this.onmessage?.({ data: text });
  }
}

/** A scripted adapter. Its position moves at the rate last set while it plays; commands take effect when they finish. */
class FakePlayer {
  readonly id: string;
  readonly #run: Run;
  #position = 0;
  #since: number;
  #rate = 1;
  paused = true;
  buffering = false;
  durationMs: number | null = null;
  firstFrameShown = true;
  canSetRate = false;
  seekAccuracyMs = 0;
  playThrows = false;
  /** The next command stays running until a finish step. */
  holdNext = false;
  held: { apply: () => void; resolve: () => void; reject: (e: Error) => void } | null = null;

  constructor(id: string, run: Run) {
    this.id = id;
    this.#run = run;
    this.#since = run.time.now;
  }

  get positionMs(): number {
    if (this.paused || this.buffering) return this.#position;
    return this.#position + Math.floor((this.#run.time.now - this.#since) * this.#rate);
  }

  /** Changes the readable state from now on. */
  set(fields: Json): void {
    this.#position = this.positionMs;
    this.#since = this.#run.time.now;
    for (const [k, v] of Object.entries(fields)) {
      if (k === "positionMs") this.#position = v as number;
      else (this as Json)[k] = v;
    }
  }

  play(): Promise<void> {
    return this.#command({ do: "play" }, () => this.set({ paused: false }), this.playThrows);
  }
  pause(): Promise<void> {
    return this.#command({ do: "pause" }, () => this.set({ paused: true }));
  }
  seek(positionMs: number): Promise<void> {
    return this.#command({ do: "seek", positionMs }, () => this.set({ positionMs }));
  }
  /** Present only while the adapter can set rates (SDK.md §16.8). */
  get setRate(): ((rate: number) => Promise<void>) | undefined {
    if (!this.canSetRate) return undefined;
    return (rate) =>
      this.#command({ do: "setRate", rate: round6(rate) }, () => {
        this.set({});
        this.#rate = rate;
      });
  }

  #command(command: Json, apply: () => void, throws = false): Promise<void> {
    this.#run.out.command.push({ player: this.id, ...command });
    if (this.holdNext) {
      this.holdNext = false;
      const { promise, resolve, reject } = Promise.withResolvers<void>();
      this.held = { apply, resolve, reject };
      return promise;
    }
    if (throws) return Promise.reject(new Error(`${this.id} refused to ${command.do}`));
    apply();
    return Promise.resolve();
  }
}

/** A pending resolver or probe call. */
interface Pending<V> {
  resolve: (value: V) => void;
  reject: (e: Error) => void;
  cancelled: boolean;
}

/** One trace's world: the party, its fakes and the outputs of the current step. */
class Run {
  readonly time: VirtualTime;
  readonly setup: Json;
  readonly party: WatchParty<string>;
  out = noOutputs();
  readonly sockets: FakeSocket[] = [];
  readonly players = new Map<string, FakePlayer>();
  readonly relays: Relays;
  /** How the next connection opens: per URL, and for any URL under "". */
  readonly #nextOpen = new Map<string, "fail" | "hang">();
  /** The round trip of pings sent from now on, per URL; null never answers. */
  readonly #rtt = new Map<string, number | null>();
  #rttAll: number | null;
  #nextGet: Json = null;
  #draws: number[];
  drawsMissing = false;
  resolving: (Pending<Candidate<string>[]> & { title: string }) | null = null;
  readonly probes = new Map<string, Pending<number | null>>();
  readonly calls = new Set<string>();

  constructor(setup: Json) {
    this.setup = setup;
    this.time = new VirtualTime(setup.startMs);
    this.#draws = [...setup.random];
    this.#rttAll = setup.rttMs;
    const resolver: Resolver<string> = { resolve: (media, signal) => this.#resolve(media, signal) };
    this.relays = setup.base === null ? Relays.public({ httpGet: (url) => this.#get(url) }) : Relays.at(setup.base);
    this.party = new WatchParty<string>({
      relays: this.relays,
      client: setup.client,
      name: setup.name,
      ...(setup.resolver ? { resolver } : {}),
      nowMs: () => this.time.now,
      random: () => {
        const u = this.#draws.shift();
        if (u === undefined) this.drawsMissing = true;
        return u ?? 0;
      },
      connect: (url) => this.#connect(url),
    });
    this.party.on("event", (e) => this.out.event.push(projectEvent(e)));
  }

  get drawsLeft(): number {
    return this.#draws.length;
  }

  #connect(url: string): WebSocket {
    const socket = new FakeSocket(url, this);
    this.sockets.push(socket);
    this.out.net.push({ open: url });
    const key = this.#nextOpen.has(url) ? url : "";
    const how = this.#nextOpen.get(key) ?? "open";
    this.#nextOpen.delete(key);
    if (how === "open") {
      this.time.schedule(0, () => {
        if (socket.readyState !== 0) return;
        socket.readyState = 1;
        socket.onopen?.({});
      });
    } else if (how === "fail") {
      this.time.schedule(0, () => {
        if (socket.readyState !== 0) return;
        socket.readyState = 3;
        socket.onerror?.({});
        socket.onclose?.({ code: 1006 });
      });
    }
    return socket as unknown as WebSocket;
  }

  /** A GET of the public list: answered with setup.list unless a nextGet step scripted this one. */
  #get(url: string): Promise<string> {
    this.out.net.push({ get: url });
    const how = this.#nextGet ?? this.setup.list;
    this.#nextGet = null;
    if (how === "fail") return Promise.reject(new Error(`GET ${url} failed`));
    if (how === "hang") return new Promise<string>(() => {});
    return Promise.resolve(JSON.stringify(how));
  }

  /** Answers a ping its URL's round trip later, with relay time = send time + rtt / 2 + relayOffsetMs. */
  pong(socket: FakeSocket, id: unknown): void {
    const rtt = this.#rtt.has(socket.url) ? this.#rtt.get(socket.url)! : this.#rttAll;
    if (rtt === null) return;
    const serverTime = this.time.now + Math.floor(rtt / 2) + this.setup.relayOffsetMs;
    this.time.schedule(rtt, () => socket.deliver(JSON.stringify({ type: "pong", id, serverTime })));
  }

  /** The connection opened last that is still open, to `on` when given. */
  socket(on?: string): FakeSocket {
    const open = this.sockets.filter((s) => s.readyState === 1 && (on === undefined || s.url === on));
    assert.ok(open.length > 0, `no open connection${on === undefined ? "" : ` to ${on}`}`);
    return open[open.length - 1]!;
  }

  player(id: string): FakePlayer {
    const p = this.players.get(id);
    assert.ok(p, `unknown player ${id}`);
    return p;
  }

  #resolve(media: Media, signal: AbortSignal): Promise<Candidate<string>[]> {
    assert.equal(this.resolving, null, "a second resolve call while one is pending");
    this.out.resolver.push({ resolve: media.title });
    const { promise, resolve, reject } = Promise.withResolvers<Candidate<string>[]>();
    const pending = { resolve, reject, cancelled: false, title: media.title };
    this.resolving = pending;
    signal.addEventListener("abort", () => {
      pending.cancelled = true;
      if (this.resolving === pending) this.resolving = null;
      this.out.resolver.push({ resolveCancelled: media.title });
    });
    return promise;
  }

  candidate(c: Json): Candidate<string> {
    const { probe, ref, ...rest } = c;
    const candidate: Candidate<string> = { ...rest, ref: ref ?? c.id };
    if (probe === true) candidate.probe = (signal) => this.#probe(c.id, signal);
    return candidate;
  }

  #probe(id: string, signal: AbortSignal): Promise<number | null> {
    assert.ok(!this.probes.has(id), `probe ${id} called twice at once`);
    this.out.resolver.push({ probe: id });
    const { promise, resolve, reject } = Promise.withResolvers<number | null>();
    const pending = { resolve, reject, cancelled: false };
    this.probes.set(id, pending);
    signal.addEventListener("abort", () => {
      pending.cancelled = true;
      this.probes.delete(id);
      this.out.resolver.push({ probeCancelled: id });
    });
    return promise;
  }

  /** Runs one step and settles. */
  async step(s: Json): Promise<void> {
    if ("call" in s) this.#call(s);
    else if ("recv" in s) this.socket(s.on).deliver(JSON.stringify(s.recv));
    else if ("recvText" in s) this.socket(s.on).deliver(s.recvText);
    else if ("drop" in s) {
      const socket = this.socket(s.on);
      socket.readyState = 3;
      socket.onclose?.({ code: s.drop ?? 1005 });
    } else if ("nextOpen" in s) this.#nextOpen.set(s.on ?? "", s.nextOpen);
    else if ("rtt" in s) {
      if (s.on === undefined) {
        this.#rttAll = s.rtt;
        this.#rtt.clear();
      } else this.#rtt.set(s.on, s.rtt);
    } else if ("nextGet" in s) this.#nextGet = s.nextGet;
    else if ("advance" in s) {
      await this.time.advance(s.advance);
      return;
    } else if ("player" in s) {
      const { player: id, ...fields } = s;
      let p = this.players.get(id);
      if (p === undefined) {
        p = new FakePlayer(id, this);
        this.players.set(id, p);
      }
      p.set(fields);
    } else if ("hold" in s) this.player(s.hold).holdNext = true;
    else if ("finish" in s) {
      const p = this.player(s.finish);
      const held = p.held;
      assert.ok(held, `${s.finish} has no running command to finish`);
      p.held = null;
      if (s.throws === true) held.reject(new Error(`${s.finish} failed`));
      else {
        held.apply();
        held.resolve();
      }
    } else if ("answer" in s || "answerThrows" in s) {
      const pending = this.resolving;
      assert.ok(pending, "no resolve call to answer");
      this.resolving = null;
      if ("answer" in s) pending.resolve(s.answer.map((c: Json) => this.candidate(c)));
      else pending.reject(new Error("the resolver failed"));
    } else if ("probeAnswer" in s || "probeThrows" in s) {
      const id: string = s.probeAnswer ?? s.probeThrows;
      const pending = this.probes.get(id);
      assert.ok(pending, `no running probe of ${id}`);
      this.probes.delete(id);
      if ("probeAnswer" in s) pending.resolve(s.durationMs);
      else pending.reject(new Error(`the probe of ${id} failed`));
    } else assert.fail(`unknown step ${JSON.stringify(s)}`);
    await this.time.settle();
  }

  #call(s: Json): void {
    const party = this.party;
    const id: string | undefined = s.id;
    if (id !== undefined) this.calls.add(id);
    const ok = (value?: unknown) => {
      if (id !== undefined) this.out.done.push(value === undefined ? { id } : { id, value });
    };
    const failed = (e: unknown) => {
      assert.ok(e instanceof PartyError, `${s.call} threw ${String(e)}`);
      assert.ok(id !== undefined, `${s.call} without an id threw ${e.code}`);
      this.out.done.push({ id, error: e.code, re: e.re, message: e.message });
    };
    const later = (p: Promise<unknown>, value: (v: unknown) => unknown = () => undefined) =>
      p.then((v) => ok(value(v)), failed);
    try {
      switch (s.call) {
        case "create":
          return void later(party.create(), (code) => code);
        case "join":
          return void later(s.create === undefined ? party.join(s.code) : party.join(s.code, { create: s.create }));
        case "choose":
          return void later(party.choose(this.candidate(s.candidate)), (v) => v);
        case "detachPlayer":
          return void later(party.detachPlayer(s.player === undefined ? undefined : this.player(s.player)));
        case "close":
          return void later(party.close());
        case "refresh":
          return void later(this.relays.refresh(), (v) => v);
        case "attachPlayer":
          party.attachPlayer(this.player(s.player) as unknown as PlayerAdapter, s.ref);
          break;
        case "setMedia":
          if (s.positionMs === undefined) party.setMedia(s.media, s.ref);
          else party.setMedia(s.media, s.ref, s.positionMs);
          break;
        case "seek":
          party.seek(s.ms);
          break;
        case "setRate":
          party.setRate(s.rate);
          break;
        case "setOffset":
          party.setOffset(s.ms);
          break;
        case "setControl":
          party.setControl(s.mode);
          break;
        case "lock":
          party.lock(s.locked);
          break;
        case "kick":
          party.kick(s.peerId);
          break;
        case "transferHost":
          party.transferHost(s.peerId);
          break;
        case "leave":
        case "end":
        case "clearMedia":
        case "play":
        case "pause":
        case "acceptMismatch":
        case "declineMismatch":
        case "resolveAgain":
        case "playerChanged":
          party[s.call as "play"]();
          break;
        default:
          assert.fail(`unknown call ${s.call}`);
      }
      ok();
    } catch (e) {
      failed(e);
    }
  }

  /**
   * This step's outputs as `want` writes them: a sent message with its connection only where `want` pins it, an
   * error's `re` and `message` only where `want` has them, and the state fields `want` names.
   */
  take(want: Json): Json {
    const got: Json = {};
    for (const ch of CHANNELS) {
      if (this.out[ch].length === 0) continue;
      got[ch] = this.out[ch].map((e, i) => {
        // A done entry matches the expected one of its call; the others by position.
        const w = ch === "done" ? want.done?.find((d: Json) => d.id === e.id) : want[ch]?.[i];
        if (ch === "send") return w !== undefined && "on" in w && !("type" in w) ? e : e.msg;
        if (ch !== "done" || !("error" in e)) return e;
        const { re, message, ...done } = e;
        return { ...done, ...(w !== undefined && "re" in w ? { re } : {}), ...(w !== undefined && "message" in w ? { message } : {}) };
      });
    }
    const fields = Object.keys(want.state ?? {});
    if (fields.length > 0) got.state = projectState(this.party, fields);
    this.out = noOutputs();
    return got;
  }
}

function round6(r: number): number {
  return Math.round(r * 1e6) / 1e6;
}

function projectMatch(m: { state: string; deltaMs?: number | null } | null): Json {
  if (m === null) return null;
  return m.deltaMs === undefined || m.deltaMs === null ? { state: m.state } : { state: m.state, deltaMs: m.deltaMs };
}

/** The state fields as the README's projection writes them. */
function projectState(party: WatchParty<string>, fields: string[]): Json {
  const state = party.state;
  const all: Record<string, () => unknown> = {
    connection: () => state.connection,
    peerId: () => state.peerId,
    room: () => state.room?.rev ?? null,
    media: () => state.media?.title ?? null,
    match: () => projectMatch(state.match),
    choice: () => (state.choice === null ? null : { id: state.choice.id, ref: state.choice.ref }),
    mismatch: () => (state.mismatch === null ? null : { id: state.mismatch.candidate.id, deltaMs: state.mismatch.deltaMs }),
    candidates: () => state.candidates.map((r) => ({ id: r.candidate.id, deltaMs: r.deltaMs })),
    offsetMs: () => state.offsetMs,
    player: () => state.player,
    endReason: () => state.endReason,
    isHost: () => state.isHost,
    canControl: () => state.canControl,
    roomPositionMs: () => party.roomPositionMs(),
  };
  return Object.fromEntries(fields.map((f) => [f, all[f]!()]));
}

function projectEvent(e: PartyEvent): Json {
  switch (e.type) {
    case "action":
      return { type: e.type, kind: e.kind, byPeerId: e.byPeerId, by: e.by?.peerId ?? null, byMe: e.byMe, room: e.room.rev };
    case "participantJoined":
    case "participantLeft":
      return { type: e.type, peerId: e.participant.peerId };
    case "error":
      return { type: e.type, code: e.error.code, re: e.error.re };
    case "resolveFailed":
      return { type: e.type, timeout: e.error instanceof DOMException && e.error.name === "TimeoutError" };
    case "playBlocked":
      return { type: e.type };
    case "ended":
      return { type: e.type, reason: e.reason, error: e.error?.code ?? null };
  }
}

/** The expected outputs of a step with every rate rounded as the driver records it. */
function expected(outputs: Json): Json {
  const want = structuredClone(outputs);
  for (const c of want.command ?? []) if (c.do === "setRate") c.rate = round6(c.rate);
  return want;
}

/**
 * Sorts entries stably by `key`: `net` is pinned per URL only, so sorting by URL keeps each URL's order and drops the
 * rest; `done` isn't ordered at all, and each call reports once, so sorting by id drops its order.
 */
function sortedBy(entries: Json[] | undefined, key: (e: Json) => string): Json[] | undefined {
  if (entries === undefined) return undefined;
  return [...entries].sort((a, b) => (key(a) < key(b) ? -1 : key(a) > key(b) ? 1 : 0));
}

const netUrl = (e: Json): string => e.get ?? e.open ?? e.close;
const doneId = (e: Json): string => e.id;

async function runTrace(trace: Trace): Promise<void> {
  const run = new Run(trace.input.setup);
  run.time.install();
  try {
    await run.time.settle();
    const { steps } = trace.input;
    for (const [i, step] of steps.entries()) {
      await run.step(step);
      const want = expected(trace.expect.outputs[i]);
      const got = run.take(want);
      for (const o of [got, want]) {
        if (o.net !== undefined) o.net = sortedBy(o.net, netUrl);
        if (o.done !== undefined) o.done = sortedBy(o.done, doneId);
      }
      assert.deepStrictEqual(got, want, `step ${i} ${JSON.stringify(step)} at local ${run.time.now}`);
      assert.ok(!run.drawsMissing, `step ${i}: the party drew more random values than setup.random holds`);
    }
    assert.equal(run.drawsLeft, 0, "setup.random holds draws the party never took");
    assert.equal(run.resolving, null, "a resolve call is still unanswered");
    assert.deepEqual([...run.probes.keys()], [], "a probe is still unanswered");
    for (const [id, p] of run.players) assert.equal(p.held, null, `${id} still runs a held command`);
    const answered = new Set(trace.expect.outputs.flatMap((o: Json) => (o.done ?? []).map((d: Json) => d.id)));
    for (const id of run.calls) assert.ok(answered.has(id), `call ${id} never settled`);
  } finally {
    void run.party.close();
    await run.time.settle();
    run.time.uninstall();
  }
}

const table = JSON.parse(readFileSync(`${VECTORS}party.json`, "utf8")) as { table: string; cases: Trace[] };

describe("vectors: party", () => {
  for (const trace of table.cases) test(trace.name, () => runTrace(trace));
});

// What the shared party traces (party-traces.test.ts) leave to this SDK, against a scripted relay in virtual time: the
// language-specific rules of SDK.md §17, attachVideo, and the parts of shared rules no trace pins.
import assert from "node:assert/strict";
import { afterEach, beforeEach, mock, test } from "node:test";
import { setImmediate as turn } from "node:timers/promises";
import { parseMedia } from "../../src/media.ts";
import { WatchParty } from "../../src/party.ts";
import { Relays } from "../../src/relays.ts";
import type { Candidate, Media, PartyEvent, PlayerAdapter, Resolver } from "../../src/types.ts";
import { attachVideo, videoAdapter } from "../../src/video.ts";

// A resume token is 43 characters of base64url (SPEC.md §11.1).
const TOKEN = "secret-token".padEnd(43, "x");

beforeEach(() => mock.timers.enable({ apis: ["setTimeout", "Date"], now: 0 }));
afterEach(() => mock.timers.reset());

/** Lets every pending promise and microtask run (setImmediate is not mocked). */
const settle = () => turn();

/** Advances virtual time and lets the work it starts run. */
async function advance(ms: number): Promise<void> {
  mock.timers.tick(ms);
  await settle();
}

/** A socket the test drives by hand. Pings are answered at once with the virtual time as relay time. */
class FakeSocket {
  readyState = 0;
  readonly sent: Record<string, unknown>[] = [];
  /** Every text sent, pings included, as written. */
  readonly raw: string[] = [];
  closedWith: number | null = null;
  onopen: ((ev: unknown) => void) | null = null;
  onmessage: ((ev: { data: string }) => void) | null = null;
  onclose: ((ev: { code: number }) => void) | null = null;
  onerror: ((ev: unknown) => void) | null = null;

  open(): void {
    this.readyState = 1;
    this.onopen?.({});
  }
  send(text: string): void {
    this.raw.push(text);
    const msg = JSON.parse(text) as Record<string, unknown>;
    if (msg["type"] !== "ping") {
      this.sent.push(msg);
      return;
    }
    queueMicrotask(() => this.deliver({ type: "pong", id: msg["id"], serverTime: Date.now() }));
  }
  close(code?: number): void {
    this.readyState = 3;
    this.closedWith = code ?? 1005;
  }
  deliver(msg: Record<string, unknown>): void {
    if (this.readyState === 1) this.onmessage?.({ data: JSON.stringify(msg) });
  }
  drop(code = 1006): void {
    this.readyState = 3;
    this.onclose?.({ code });
  }
  types(): unknown[] {
    return this.sent.map((m) => m["type"]);
  }
}

function joinedMessage(code = "K7MQ2X") {
  return {
    type: "joined",
    code,
    you: { peerId: "p1", token: TOKEN },
    room: {
      rev: 1, code, epoch: 0, phase: "idle", positionMs: 0, atMs: 0, rate: 1, control: "host", locked: false,
      waitingOn: [],
      participants: [{ peerId: "p1", name: "Ann", client: "Test/1", host: true, connected: true, player: "idle", match: null, offsetMs: 0 }],
    },
    media: null,
    serverTime: 0,
  };
}

interface Setup {
  autoOpen?: boolean;
  /** Finds nothing when absent. */
  resolver?: Resolver<string>;
  /** Local time; the virtual time when absent. */
  nowMs?: () => number;
}

function setup({ autoOpen = true, resolver, nowMs }: Setup = {}) {
  const sockets: FakeSocket[] = [];
  const relays = Relays.at("http://relay.test");
  const party = new WatchParty<string>({
    relays,
    client: "Test/1",
    name: "Ann",
    resolver: resolver ?? { resolve: () => [] },
    nowMs: nowMs ?? (() => Date.now()),
    // u = 0: resume attempt n waits min(8000, 500 × 2^(n−1)) / 2 ms.
    random: () => 0,
    connect: () => {
      const s = new FakeSocket();
      sockets.push(s);
      if (autoOpen) queueMicrotask(() => s.open());
      return s as unknown as WebSocket;
    },
  });
  const events: PartyEvent[] = [];
  party.on("event", (e) => events.push(e));
  return { party, sockets, events };
}

async function joinedParty(options: Setup = {}) {
  const env = setup(options);
  const joining = env.party.join("K7M-Q2X");
  await settle();
  env.sockets[0]!.deliver(joinedMessage());
  await joining;
  return env;
}

test("the constructor refuses a client that isn't <app>/<version> and a bad name", () => {
  const relays = Relays.public();
  const resolver = { resolve: () => [] };
  for (const client of ["Example", "/1.0", "Example/", ""]) {
    assert.throws(() => new WatchParty({ relays, client, name: "Ann", resolver }), { code: "invalid_message" });
  }
  assert.throws(() => new WatchParty({ relays, client: "A/1", name: "a\u0007b", resolver }), { code: "invalid_message" });
  assert.throws(() => new WatchParty({ relays, client: "A/1", name: "x".repeat(33), resolver }), { code: "invalid_message" });
});

test("leave() while the socket is still opening closes it with 1000, and a late open sends nothing", async () => {
  const { party, sockets } = setup({ autoOpen: false });
  const joining = party.join("K7MQ2X");
  await settle();
  party.leave();
  await assert.rejects(joining, { code: "cancelled" });
  assert.equal(sockets[0]!.closedWith, 1000);
  sockets[0]!.open();
  await settle();
  assert.deepEqual(sockets[0]!.types(), []);
});

test("after close(), create and join reject with a plain Error, not a PartyError", async () => {
  const { party, sockets } = setup();
  party.close();
  await assert.rejects(party.create(), { name: "Error", message: "this WatchParty is closed" });
  await assert.rejects(party.join("K7MQ2X"), { name: "Error", message: "this WatchParty is closed" });
  assert.equal(sockets.length, 0);
});

test("join refuses a create that isn't a boolean before it connects", async () => {
  const { party, sockets } = setup();
  await assert.rejects(party.join("K7MQ2X", { create: "yes" as unknown as boolean }), { code: "invalid_message" });
  assert.equal(sockets.length, 0);
  assert.equal(party.state.connection, "idle");
});

test("setControl throws not_in_room while reconnecting", async () => {
  const { party, sockets } = await joinedParty();
  sockets[0]!.drop();
  assert.equal(party.state.connection, "reconnecting");
  assert.throws(() => party.setControl("everyone"), { code: "not_in_room" });
  party.leave();
});

test("unpaired surrogates are refused before sending", () => {
  const relays = Relays.public();
  const resolver = { resolve: () => [] };
  assert.throws(() => new WatchParty({ relays, client: "A/1", name: "Ann\uD800", resolver }), { code: "invalid_message" });
  assert.throws(() => new WatchParty({ relays, client: "A/1\uDC00", name: "Ann", resolver }), { code: "invalid_message" });
  assert.doesNotThrow(() => new WatchParty({ relays, client: "A/1", name: "\uD83D\uDE00", resolver }));
});

test("a new epoch whose media ext is nested too deep to walk ends the party as lost without throwing out of onmessage", async () => {
  const deep = `${"[".repeat(100_000)}${"]".repeat(100_000)}`;
  const media = `{"kind":"movie","title":"Heat","durationMs":600000,"ext":{"app.example":${deep}}}`;
  const room = JSON.stringify({ type: "room", ...joinedMessage().room, rev: 2, epoch: 1, phase: "paused" });
  const { sockets, events } = await joinedParty();
  assert.doesNotThrow(() => sockets[0]!.onmessage!({ data: `${room.slice(0, -1)},"media":${media}}` }));
  assert.equal(sockets[0]!.types().at(-1), "leave");
  assert.equal(sockets[0]!.closedWith, 1000);
  const ended = events.at(-1);
  assert.ok(ended?.type === "ended" && ended.reason === "lost" && ended.error?.code === "invalid_message");
});

test("the Error event for a relay's refusal carries its message", async () => {
  const { party, sockets, events } = await joinedParty();
  sockets[0]!.deliver({ type: "error", code: "forbidden", re: "pause", message: "not a controller" });
  const error = events.at(-1);
  assert.ok(error?.type === "error" && error.error.code === "forbidden" && error.error.message === "not a controller");
  party.leave();
});

// ---------------------------------------------------------------------------------------------------------------
// Media, the player and commands

const MOVIE: Media = { kind: "movie", title: "Heat", year: 1995, ids: { imdb: "tt0113277" }, durationMs: 60_000 };

/** A player that records its commands and counts reads of its position. */
class FakePlayer implements PlayerAdapter {
  durationMs: number | null = 60_000;
  seekAccuracyMs: number | undefined = undefined;
  paused = true;
  buffering = false;
  firstFrameShown = true;
  /** While set, `play` rejects with it and the player stays paused. */
  refuse: Error | null = null;
  reads = 0;
  readonly calls: string[] = [];
  #position = 0;

  get positionMs(): number {
    this.reads++;
    return this.#position;
  }
  set positionMs(ms: number) {
    this.#position = ms;
  }

  play(): Promise<void> | void {
    // A refusing player stays paused, as a browser does without a user gesture.
    if (this.refuse !== null) {
      this.calls.push("play");
      return Promise.reject(this.refuse);
    }
    this.paused = false;
    return this.#record("play");
  }
  pause(): Promise<void> | void {
    this.paused = true;
    return this.#record("pause");
  }
  seek(ms: number): Promise<void> | void {
    this.positionMs = ms;
    return this.#record(`seek ${ms}`);
  }
  setRate(rate: number): Promise<void> | void {
    return this.#record(`rate ${rate}`);
  }
  #record(call: string): void {
    this.calls.push(call);
  }
}

/** A joined host whose room has MOVIE, paused at 0 in epoch 1. */
async function partyWithMedia(options: Setup = {}) {
  const env = await joinedParty(options);
  env.sockets[0]!.deliver({ type: "room", ...joinedMessage().room, rev: 2, epoch: 1, phase: "paused", media: MOVIE });
  await settle();
  return env;
}

test("a probe that answers after a later choose cancelled it changes nothing", async () => {
  const { party } = await partyWithMedia();
  // The probe ignores its signal and answers anyway.
  const answerA = Promise.withResolvers<number>();
  const a = party.choose({ id: "A", ref: "A", probe: () => answerA.promise });
  await settle();
  assert.equal(await party.choose({ id: "B", ref: "B", durationMs: 60_000 }), true);
  answerA.resolve(60_000);
  assert.equal(await a, false);
  await settle();
  assert.equal(party.state.choice?.id, "B");
  assert.deepEqual(party.state.match, { state: "matched", deltaMs: 0 });
  party.leave();
});

test("a probe answer that isn't an integer makes choose false and changes nothing", async () => {
  const { party } = await partyWithMedia();
  assert.equal(await party.choose({ id: "A", ref: "A", durationMs: 60_500 }), true);
  assert.equal(await party.choose({ id: "D", ref: "D", probe: async () => 60_000.5 }), false);
  assert.equal(party.state.choice?.id, "A");
  assert.deepEqual(party.state.match, { state: "matched", deltaMs: 500 });
  party.leave();
});

test("the choice is the app's own candidate object, and resolveAgain keeps that object", async () => {
  const a = { id: "A", ref: "A", durationMs: 60_000 };
  const answers = [[a], [{ id: "A", ref: "A2", durationMs: 60_000 }]];
  const { party } = await partyWithMedia({ resolver: { resolve: () => answers.shift()! } });
  assert.equal(party.state.choice, a);
  party.resolveAgain();
  await settle();
  assert.equal(party.state.choice, a);
  const b = { id: "B", ref: "B", durationMs: 60_000 };
  assert.equal(await party.choose(b), true);
  assert.equal(party.state.choice, b);
  party.leave();
});

test("resolveFailed for a resolver past its timeout carries a DOMException named TimeoutError", async () => {
  const resolver: Resolver<string> = { resolve: () => Promise.withResolvers<Candidate<string>[]>().promise };
  const { party, sockets, events } = await joinedParty({ resolver });
  sockets[0]!.deliver({ type: "room", ...joinedMessage().room, rev: 2, epoch: 1, phase: "paused", media: MOVIE });
  await advance(30_000);
  const failed = events.find((e) => e.type === "resolveFailed");
  assert.ok(failed?.type === "resolveFailed" && failed.error instanceof DOMException && failed.error.name === "TimeoutError");
  party.leave();
});

/** The choice for MOVIE once this party shared it as "Heat.mkv". */
const OWN = { id: "", ref: "Heat.mkv", durationMs: 60_000 };

/** A paused room of Ann (p1, this party) and Bob (p2); `host` is the host's peer ID. */
function roomOfTwo(rev: number, epoch: number, host = "p1") {
  const room = joinedMessage().room;
  const ann = room.participants[0]!;
  const participants = [{ ...ann, host: host === "p1" }, { ...ann, peerId: "p2", name: "Bob", host: host === "p2" }];
  return { type: "room", ...room, rev, epoch, phase: "paused", participants };
}

/** Host Ann shares MOVIE as "Heat.mkv" in a room with Bob, and the relay starts epoch 1 with it. */
async function sharedMovie() {
  const env = await joinedParty();
  const socket = env.sockets[0]!;
  socket.deliver(roomOfTwo(2, 0));
  env.party.setMedia(MOVIE, "Heat.mkv");
  assert.deepEqual(socket.sent.find((m) => m["type"] === "setMedia"), { type: "setMedia", media: MOVIE, positionMs: 0 });
  socket.deliver({ ...roomOfTwo(3, 1), media: MOVIE, action: { kind: "media", by: "p1" } });
  await settle();
  return { ...env, socket };
}

test("the party's own choice carries the descriptor's durationMs", async () => {
  const { party } = await sharedMovie();
  assert.deepEqual(party.state.choice, OWN);
  party.leave();
});

test("a play timer longer than 2147483647 ms neither fires early nor re-arms in a loop", async () => {
  const { party, sockets } = await partyWithMedia();
  await party.choose({ id: "A", ref: "A", durationMs: 60_000 });
  const player = new FakePlayer();
  party.attachPlayer(player, "A");
  // A group start 3e9 ms from now: the play timer is longer than setTimeout takes.
  const room = { ...joinedMessage().room, rev: 3, epoch: 1, phase: "playing", atMs: Date.now() + 3e9 };
  sockets[0]!.deliver({ type: "room", ...room });
  await settle();
  const reads = player.reads;
  // No tick is due within 100 ms. A timer that fires early (setTimeout runs a longer delay after 1 ms) re-arms and
  // steps every millisecond.
  for (let ms = 0; ms < 100; ms++) await advance(1);
  assert.equal(player.reads, reads);
  await advance(1000);
  assert.ok(!player.calls.includes("play"));
  party.leave();
});

test("an integer input or computed wire value out of bounds throws invalid_message before any local effect", async () => {
  const { party, sockets } = await partyWithMedia();
  await party.choose({ id: "A", ref: "A", durationMs: 60_000 });
  const player = new FakePlayer();
  party.attachPlayer(player, "A");
  await settle();
  const commands = () => sockets[0]!.sent.filter((m) => m["type"] !== "status").length;
  const sent = commands();
  const calls = player.calls.length;
  const refused = (what: string, f: () => void) => assert.throws(f, { code: "invalid_message" }, what);
  for (const ms of [604_800_001, -604_800_001, -3e9, 0.5, NaN]) refused(`setOffset(${ms})`, () => party.setOffset(ms));
  assert.equal(party.state.offsetMs, 0);
  for (const ms of [-1, 1.5, 2 ** 53, Infinity]) refused(`seek(${ms})`, () => party.seek(ms));
  for (const ms of [-1, 0.5, 2 ** 53]) refused(`setMedia(…, ${ms})`, () => party.setMedia(MOVIE, "A", ms));
  refused("setMedia of 604800001 ms", () => party.setMedia({ ...MOVIE, durationMs: 604_800_001 }, "A"));
  party.setOffset(-604_800_000);
  // Valid inputs whose wire position, position − offset, is past 2^53 − 1.
  refused("seek(2^53 − 1) at offset −604800000", () => party.seek(Number.MAX_SAFE_INTEGER));
  player.positionMs = Number.MAX_SAFE_INTEGER;
  refused("pause() at 2^53 − 1", () => party.pause());
  player.positionMs = NaN;
  refused("pause() at NaN", () => party.pause());
  assert.equal(commands(), sent);
  assert.deepEqual(player.calls.slice(calls), []);
  party.leave();
});

test("every outgoing int is written without a fraction or exponent, also from arithmetic", async () => {
  // A local clock with fractions: round trips and positions computed from it are still ints on the wire.
  let reads = 0;
  const { party, sockets } = await partyWithMedia({ nowMs: () => Date.now() + (reads++ % 4) / 4 });
  await party.choose({ id: "A", ref: "A", probe: async () => 60_400 });
  const player = new FakePlayer();
  party.attachPlayer(player, "A");
  await advance(300);
  party.setOffset(-250);
  player.positionMs = 1234.75;
  party.pause();
  await advance(300);
  party.seek(5000);
  await advance(300);
  party.setMedia({ ...MOVIE, title: "Ronin", year: 1998 }, "Ronin", 1000);
  await advance(16_000);
  const types = new Set(sockets[0]!.raw.map((t) => JSON.parse(t).type));
  for (const t of ["ping", "status", "pause", "seek", "setMedia"]) assert.ok(types.has(t), `sent ${t}`);
  for (const text of sockets[0]!.raw) {
    for (const [, key, value] of text.matchAll(/"(\w+)":(-?[0-9][0-9.eE+-]*)/g)) {
      if (key !== "rate") assert.match(value!, /^-?(0|[1-9][0-9]*)$/, `${key} in ${text}`);
    }
  }
  assert.ok(sockets[0]!.raw.some((t) => /"rttMs":/.test(t)), "a status carried rttMs");
  party.leave();
});

// ---------------------------------------------------------------------------------------------------------------
// Published state is frozen (SDK.md §16.6)

test("published state is frozen all the way down, and the app's objects are not", async () => {
  const candidate = { id: "A", ref: "A", durationMs: 70_000 };
  const { party, sockets } = await partyWithMedia({ resolver: { resolve: () => [candidate] } });
  const events: PartyEvent[] = [];
  party.on("event", (e) => events.push(e));
  sockets[0]!.deliver({ ...roomOfTwo(3, 1), action: { kind: "pause", by: "p2" } });
  const s = party.state;
  assert.ok(s.mismatch !== null && s.room !== null && s.media !== null && s.match !== null);
  const action = events.find((e) => e.type === "action");
  const writes: [string, () => void][] = [
    ["state", () => ((s as { offsetMs: number }).offsetMs = 1)],
    ["room", () => ((s.room as { phase: string }).phase = "playing")],
    ["participants", () => (s.room!.participants as unknown[]).push(s.me)],
    ["a participant", () => ((s.room!.participants[1] as { name: string }).name = "Eve")],
    ["waitingOn", () => (s.room!.waitingOn as string[]).push("p2")],
    ["media", () => ((s.media as { title: string }).title = "Ronin")],
    ["media.ids", () => ((s.media!.ids as Record<string, string>)["imdb"] = "tt0")],
    ["match", () => ((s.match as { state: string }).state = "matched")],
    ["mismatch", () => ((s.mismatch as { deltaMs: number }).deltaMs = 0)],
    ["candidates", () => (s.candidates as unknown[]).pop()],
    ["a ranked candidate", () => ((s.candidates[0] as { deltaMs: number }).deltaMs = 0)],
    ["an event", () => ((action as { kind: string }).kind = "play")],
    ["the relay list", () => (Relays.public().list as unknown[]).pop()],
    ["a relay", () => ((Relays.public().list[0] as { url: string }).url = "wss://evil.example/v1/ws")],
    ["parsed media", () => ((parseMedia(MOVIE).ids as Record<string, string>)["imdb"] = "tt0")],
  ];
  for (const [what, write] of writes) assert.throws(write, TypeError, what);
  assert.equal(s.mismatch.candidate, candidate);
  assert.ok(!Object.isFrozen(candidate), "the app's candidate stays the app's");
  party.leave();
});

// ---------------------------------------------------------------------------------------------------------------
// Offset (SDK.md §12) and seek accuracy (§11.2)

test("seek sends positionMs 0 when ms − offset is negative", async () => {
  const { party, sockets } = await partyWithMedia();
  party.setOffset(1000);
  party.seek(100);
  assert.deepEqual(sockets[0]!.sent.filter((m) => m["type"] === "seek"), [{ type: "seek", positionMs: 0 }]);
  party.leave();
});

test("an adapter's seekAccuracyMs that isn't an integer ≥ 0 counts as 0", async () => {
  // A playing room with the player 3000 ms ahead: past the 2000 ms seek threshold.
  for (const accuracy of [-1, 1.5, "5000", Infinity]) {
    const { party, sockets } = await partyWithMedia();
    await party.choose({ id: "A", ref: "A", durationMs: 60_000 });
    sockets[0]!.deliver({ type: "room", ...joinedMessage().room, rev: 3, epoch: 1, phase: "playing", atMs: Date.now() });
    const player = new FakePlayer();
    player.paused = false;
    player.positionMs = 3000;
    player.seekAccuracyMs = accuracy as number;
    party.attachPlayer(player, "A");
    await settle();
    assert.ok(player.calls.some((c) => c.startsWith("seek")), `seekAccuracyMs ${String(accuracy)}`);
    party.leave();
  }
});

// ---------------------------------------------------------------------------------------------------------------
// Resume (SDK.md §14)

test("the resume token never appears in the published state", async () => {
  const { party } = await joinedParty();
  assert.ok(!JSON.stringify(party.state).includes(TOKEN));
  party.leave();
});

// ---------------------------------------------------------------------------------------------------------------
// A player that refuses to start (SDK.md §10.2)

test("attaching a refusing adapter with another ref unbinds it and ends blocked; bound again, it is reported again", async () => {
  const { party, sockets, events } = await partyWithMedia();
  await party.choose({ id: "A", ref: "A", durationMs: 60_000 });
  const player = new FakePlayer();
  player.refuse = new DOMException("play() needs a user gesture", "NotAllowedError");
  party.attachPlayer(player, "A");
  sockets[0]!.deliver({ type: "room", ...joinedMessage().room, rev: 3, epoch: 1, phase: "playing", atMs: Date.now() });
  await settle();
  assert.equal(events.filter((e) => e.type === "playBlocked").length, 1);
  party.attachPlayer(player, "B");
  party.attachPlayer(player, "A");
  await settle();
  assert.equal(events.filter((e) => e.type === "playBlocked").length, 2);
  assert.equal(party.state.player, "loading");
  party.leave();
});

// ---------------------------------------------------------------------------------------------------------------
// attachVideo (README, "Player and resolver")

/** The parts of an HTMLMediaElement that attachVideo uses. */
class FakeVideo extends EventTarget {
  currentTime = 0;
  duration = 60;
  paused = true;
  ended = false;
  seeking = false;
  readyState = 4;
  /** Whether `play()` may start the element: without a user gesture a browser refuses, and the element stays paused. */
  allowed = true;
  /** What an allowed `play()` returns: a browser settles it once playback runs. */
  started: Promise<void> = Promise.resolve();
  #rate = 1;
  get playbackRate(): number {
    return this.#rate;
  }
  // A browser fires ratechange after every change, as a task of its own (setImmediate is not mocked).
  set playbackRate(rate: number) {
    if (rate === this.#rate) return;
    this.#rate = rate;
    setImmediate(() => this.dispatchEvent(new Event("ratechange")));
  }
  play(): Promise<void> {
    if (!this.allowed) return Promise.reject(new DOMException("play() needs a user gesture", "NotAllowedError"));
    this.paused = false;
    return this.started;
  }
  pause(): void {
    this.paused = true;
  }
}

test("attachVideo's cleanup detaches its own adapter only, never a newer one, and returns the completion", async () => {
  const { party } = await partyWithMedia();
  await party.choose({ id: "A", ref: "A", durationMs: 60_000 });
  const first = new FakeVideo();
  const cleanup = attachVideo(party, first as unknown as HTMLMediaElement, "A");
  await settle();
  assert.equal(party.state.player, "ready");
  attachVideo(party, new FakeVideo() as unknown as HTMLMediaElement, "A");
  const done = cleanup();
  assert.equal(party.state.player, "ready", "the newer element stays attached");
  assert.ok(done instanceof Promise);
  await done;
  party.leave();
});

test("after the element pauses itself at its end, the first press on its play is the party's play", async () => {
  const { party, sockets } = await partyWithMedia();
  await party.choose({ id: "A", ref: "A", durationMs: 60_000 });
  const video = new FakeVideo();
  attachVideo(party, video as unknown as HTMLMediaElement, "A");
  // The room plays its last 100 ms.
  const playing = { phase: "playing", positionMs: 59_900, atMs: Date.now() };
  sockets[0]!.deliver({ type: "room", ...joinedMessage().room, rev: 3, epoch: 1, ...playing });
  await settle();
  assert.equal(video.paused, false);
  // The element reaches its end and pauses itself: not a press.
  Object.assign(video, { currentTime: 60, paused: true, ended: true });
  video.dispatchEvent(new Event("pause"));
  video.dispatchEvent(new Event("ended"));
  // The room pauses there too; the element is paused already, so the SDK sends it no pause.
  const ended = { kind: "ended", by: null };
  sockets[0]!.deliver({ type: "room", ...joinedMessage().room, rev: 4, epoch: 1, phase: "paused", positionMs: 60_000, action: ended });
  await settle();
  assert.deepEqual(sockets[0]!.types().filter((t) => t === "pause" || t === "play"), []);
  // The user presses play on the element's own controls.
  void video.play();
  video.dispatchEvent(new Event("play"));
  assert.deepEqual(sockets[0]!.types().filter((t) => t === "pause" || t === "play"), ["play"]);
  assert.equal(video.paused, true, "the element waits for the room");
  party.leave();
});

test("videoAdapter's play rejects with the element's refusal, and otherwise resolves once playback is requested", async () => {
  const refusing = new FakeVideo();
  refusing.allowed = false;
  const adapter = videoAdapter(refusing as unknown as HTMLMediaElement);
  await assert.rejects(async () => adapter.play(), { name: "NotAllowedError" });
  assert.equal(refusing.paused, true);
  // Playback waits for data that never comes: play doesn't wait for it.
  const stalled = new FakeVideo();
  stalled.started = Promise.withResolvers<void>().promise;
  let resolved = false;
  void Promise.resolve(videoAdapter(stalled as unknown as HTMLMediaElement).play()).then(() => (resolved = true));
  await settle();
  assert.equal(resolved, true);
  assert.equal(stalled.paused, false);
});

test("attachVideo: an element that refuses to play is loading with playBlocked; the user's own play makes it ready", async () => {
  const { party, sockets, events } = await partyWithMedia();
  await party.choose({ id: "A", ref: "A", durationMs: 60_000 });
  const video = new FakeVideo();
  video.allowed = false;
  attachVideo(party, video as unknown as HTMLMediaElement, "A");
  sockets[0]!.deliver({ type: "room", ...joinedMessage().room, rev: 3, epoch: 1, phase: "playing", atMs: Date.now() });
  await settle();
  assert.equal(video.paused, true);
  assert.equal(party.state.player, "loading");
  const blocked = events.filter((e) => e.type === "playBlocked");
  assert.equal(blocked.length, 1);
  assert.equal((blocked[0]!.error as DOMException).name, "NotAllowedError");
  // The user presses play on the element's own controls, which the browser counts as a gesture.
  video.allowed = true;
  void video.play();
  video.dispatchEvent(new Event("play"));
  video.dispatchEvent(new Event("playing"));
  assert.equal(video.paused, false, "the room plays, so the element goes on");
  assert.equal(party.state.player, "ready");
  assert.deepEqual(sockets[0]!.types().filter((t) => t === "play" || t === "pause"), []);
  party.leave();
});

test("attachVideo never sends back a rate the SDK set on the element", async () => {
  const { party, sockets } = await partyWithMedia();
  await party.choose({ id: "A", ref: "A", durationMs: 60_000 });
  sockets[0]!.deliver({ type: "room", ...joinedMessage().room, rev: 3, epoch: 1, phase: "paused", rate: 1.25 });
  const video = new FakeVideo();
  attachVideo(party, video as unknown as HTMLMediaElement, "A");
  // Room for a chain of ratechange tasks: each would carry a rate back to the room.
  for (let i = 0; i < 40; i++) await settle();
  assert.equal(video.playbackRate, 1.25, "binding sets the room's rate on the element");
  const rates = () => sockets[0]!.sent.filter((m) => m["type"] === "rate");
  assert.deepEqual(rates(), []);
  // The user picks 1.5× in the element's speed menu: the room gets 1.5, once.
  video.playbackRate = 1.5;
  for (let i = 0; i < 40; i++) await settle();
  assert.deepEqual(rates(), [{ type: "rate", rate: 1.5 }]);
  const rate = { kind: "rate", by: "p1" };
  sockets[0]!.deliver({ type: "room", ...joinedMessage().room, rev: 4, epoch: 1, phase: "paused", rate: 1.5, action: rate });
  await advance(3000);
  assert.deepEqual(rates(), [{ type: "rate", rate: 1.5 }]);
  assert.equal(video.playbackRate, 1.5);
  party.leave();
});

test("while the user drags the seek bar, attachVideo sends one seek per 250 ms, the latest, then the final one", async () => {
  const { party, sockets } = await partyWithMedia();
  await party.choose({ id: "A", ref: "A", durationMs: 60_000 });
  const video = new FakeVideo();
  attachVideo(party, video as unknown as HTMLMediaElement, "A");
  await settle();
  const seeks = () => sockets[0]!.sent.filter((m) => m["type"] === "seek").map((m) => m["positionMs"]);
  const drag = (seconds: number) => {
    video.currentTime = seconds;
    video.dispatchEvent(new Event("seeking"));
  };
  drag(10);
  await advance(100);
  drag(12);
  await advance(100);
  drag(14);
  assert.deepEqual(seeks(), [10_000]);
  await advance(50);
  assert.deepEqual(seeks(), [10_000, 14_000], "the latest position, 250 ms after the first");
  await advance(50);
  drag(16);
  await advance(50);
  drag(18);
  // Seeking ends at 350 ms, inside the window that opened at 250 ms.
  video.dispatchEvent(new Event("seeked"));
  await advance(149);
  assert.deepEqual(seeks(), [10_000, 14_000]);
  await advance(1);
  assert.deepEqual(seeks(), [10_000, 14_000, 18_000], "the final position once its 250 ms are up");
  await advance(3000);
  assert.deepEqual(seeks(), [10_000, 14_000, 18_000]);
  party.leave();
});

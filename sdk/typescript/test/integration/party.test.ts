// Parties against a real relay built from relay/ (timers at 0.1). Real time throughout: see harness.ts.
import assert from "node:assert/strict";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { after, before, describe, test } from "node:test";
import { WatchParty } from "../../src/party.ts";
import { Relays } from "../../src/relays.ts";
import type { Media, PartyEvent, Resolver } from "../../src/types.ts";
import { eventually, SimPlayer, sleep, stateWhere, TestRelay } from "./harness.ts";

const DURATION = 600_000;
const MOVIE: Media = { kind: "movie", title: "Heat", year: 1995, ids: { imdb: "tt0113277" }, durationMs: DURATION };
// The SDK's sync tolerance: drift within the 350 ms deadband is left alone (SDK.md §11.2).
const TOLERANCE_MS = 350;

const sameFile: Resolver<string> = { resolve: () => [{ id: "heat", ref: "Heat.1995.mkv", durationMs: DURATION }] };

interface Member {
  party: WatchParty<string>;
  player: SimPlayer;
  sockets: WebSocket[];
  events: PartyEvent[];
}

function member(relays: Relays, name: string): Member {
  const sockets: WebSocket[] = [];
  const events: PartyEvent[] = [];
  const party = new WatchParty<string>({
    relays,
    client: "OWP-Test/1.0",
    name,
    resolver: sameFile,
    connect: (url) => {
      const ws = new WebSocket(url);
      sockets.push(ws);
      return ws;
    },
  });
  party.on("event", (e) => events.push(e));
  return { party, player: new SimPlayer(DURATION), sockets, events };
}

/**
 * A host sharing MOVIE and a guest that matched it, both attached. setMedia goes through the barrier as a play
 * (SPEC.md §7.10), so the room plays once both players are ready.
 */
async function room(relays: Relays) {
  const host = member(relays, "Host");
  const code = await host.party.create();
  host.party.setMedia(MOVIE, "Heat.1995.mkv");
  host.party.attachPlayer(host.player, "Heat.1995.mkv");
  const guest = member(relays, "Guest");
  await guest.party.join(code.toLowerCase().replace(/^(...)/, "$1-"));
  const { choice } = await stateWhere(guest.party, "a choice", (s) => s.choice !== null);
  guest.party.attachPlayer(guest.player, choice!.ref);
  await Promise.all([phase(host, "playing"), phase(guest, "playing")]);
  return { host, guest, code };
}

function phase(m: Member, p: string) {
  return stateWhere(m.party, `phase ${p}`, (s) => s.room?.phase === p);
}

/** Both players within the tolerance of the room position and of each other. */
function inSync(...ms: Member[]): void {
  const room = ms[0]!.party.roomPositionMs()!;
  for (const m of ms) {
    assert.ok(Math.abs(m.player.positionMs - room) <= TOLERANCE_MS, `${m.player.positionMs} vs room ${room}`);
  }
}

function closeAll(...ms: Member[]): void {
  for (const m of ms) m.party.close();
}

describe("against a relay", () => {
  let relay: TestRelay;
  let relays: Relays;
  before(async () => {
    relay = await TestRelay.start();
    relays = Relays.at(relay.base);
  });
  after(() => relay.stop());

  test("create, join with a typed code, and after end, join with create makes the room again as host", async () => {
    const host = member(relays, "Host");
    const code = await host.party.create();
    assert.match(code, /^[23456789ABCDEFGHJKMNPQRSTUVWXYZ]{6}$/);
    const typed = ` ${code.slice(0, 3)} ${code.slice(3)} `.toLowerCase();
    const guest = member(relays, "Guest");
    await guest.party.join(typed);
    const s = await stateWhere(host.party, "two participants", (st) => st.room!.participants.length === 2);
    assert.equal(s.isHost, true);
    assert.equal(guest.party.state.isHost, false);
    assert.equal(guest.party.state.canControl, false);
    assert.equal(guest.party.state.room!.code, code);
    assert.ok(host.events.some((e) => e.type === "participantJoined" && e.participant.name === "Guest"));
    assert.throws(() => guest.party.setControl("everyone"), { code: "forbidden" });

    host.party.end();
    await stateWhere(host.party, "ended", (st) => st.connection === "ended");
    await host.party.join(typed, { create: true });
    const again = host.party.state;
    assert.equal(again.isHost, true);
    assert.equal(again.room!.code, code);
    assert.deepEqual(again.room!.participants.map((p) => p.name), ["Host"]);
    closeAll(host, guest);
  });

  test("setMedia: the guest's resolver matches, both report ready, the room plays and both players follow", async () => {
    const { host, guest } = await room(relays);
    assert.deepEqual(guest.party.state.match, { state: "matched", deltaMs: 0 });
    assert.ok(host.events.some((e) => e.type === "action" && e.kind === "media" && e.byMe));
    await stateWhere(host.party, "both ready", (s) => s.room!.participants.every((p) => p.player === "ready"));
    await eventually(() => assert.ok(!host.player.paused && !guest.player.paused));
    await sleep(1500);
    inSync(host, guest);
    assert.ok(Math.abs(host.player.positionMs - guest.player.positionMs) <= 100, "players start at the same moment");
    assert.ok(host.player.positionMs > 1000);
    closeAll(host, guest);
  });

  test("pause, seek and setRate reach the other player", async () => {
    const { host, guest } = await room(relays);
    await sleep(500);

    host.party.pause();
    await phase(guest, "paused");
    await eventually(() => assert.ok(guest.player.paused));
    const pausedAt = guest.party.state.room!.positionMs;
    assert.ok(Math.abs(host.player.positionMs - pausedAt) <= 50, "the room pauses where the presser stopped");
    await eventually(() => assert.ok(Math.abs(guest.player.positionMs - pausedAt) <= 50));

    host.party.seek(120_000);
    await stateWhere(guest.party, "seeked", (s) => s.room!.positionMs === 120_000);
    await eventually(() => assert.ok(Math.abs(guest.player.positionMs - 120_000) <= 50));
    assert.equal(host.player.positionMs, 120_000);
    assert.ok(guest.events.some((e) => e.type === "action" && e.kind === "seek" && e.by?.name === "Host"));

    host.party.setRate(1.5);
    host.party.play();
    await stateWhere(guest.party, "rate 1.5 playing", (s) => s.room!.rate === 1.5 && s.room!.phase === "playing");
    await eventually(() => assert.ok(Math.abs(guest.player.rate - 1.5) < 0.1 && !guest.player.paused));
    await sleep(1000);
    inSync(host, guest);
    closeAll(host, guest);
  });

  test("a dropped connection is resumed and the player keeps following", async () => {
    const { host, guest } = await room(relays);
    await sleep(300);
    const peerId = guest.party.state.peerId;
    guest.sockets.at(-1)!.close();
    await stateWhere(guest.party, "reconnecting", (s) => s.connection === "reconnecting");
    assert.throws(() => guest.party.pause(), { code: "not_in_room" });
    await stateWhere(guest.party, "connected again", (s) => s.connection === "connected", 15_000);
    assert.equal(guest.party.state.peerId, peerId);
    assert.equal(guest.sockets.length, 2);
    host.party.seek(200_000);
    await eventually(() => assert.ok(Math.abs(guest.player.positionMs - host.player.positionMs) <= TOLERANCE_MS));
    closeAll(host, guest);
  });
});

test("a relay restart with --state-dir: both parties resume into the saved room", async () => {
  const stateDir = mkdtempSync(join(tmpdir(), "owp-sdkts-state-"));
  let relay = await TestRelay.start({ stateDir });
  try {
    const relays = Relays.at(relay.base);
    const { host, guest, code } = await room(relays);
    await sleep(1500);
    const ids = [host.party.state.peerId, guest.party.state.peerId];

    await relay.stop();
    await stateWhere(host.party, "host reconnecting", (s) => s.connection === "reconnecting");
    const stoppedAt = host.player.positionMs;
    relay = await TestRelay.start({ port: relay.port, stateDir });

    for (const m of [host, guest]) await stateWhere(m.party, "connected", (s) => s.connection === "connected", 20_000);
    assert.deepEqual([host.party.state.peerId, guest.party.state.peerId], ids);
    assert.equal(guest.party.state.room!.code, code);
    const restored = guest.party.state.room!;
    assert.equal(restored.phase, "paused", "a restored room starts paused");
    assert.ok(Math.abs(restored.positionMs - stoppedAt) <= 1500, `${restored.positionMs} near ${stoppedAt}`);
    await eventually(() => assert.ok(host.player.paused && guest.player.paused));

    host.party.play();
    await Promise.all([phase(host, "playing"), phase(guest, "playing")]);
    // Players held within 500 ms of a paused room aren't seeked; the nudge then closes the gap (SDK.md §11.2).
    await eventually(() => inSync(host, guest));
    closeAll(host, guest);
  } finally {
    await relay.stop();
  }
});

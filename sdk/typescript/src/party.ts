import type { Clock } from "./clock.ts";
import { normalizeCode, routeCode } from "./code.ts";
import { PartyError } from "./error.ts";
import { Link } from "./link.ts";
import { durationCounts, MATCH_TOLERANCE_MS, rankCandidates, walkCandidates, type ProbeOutcome } from "./matching.ts";
import { mediaEquals, sendableMedia } from "./media.ts";
import {
  MAX_OFFSET_MS,
  mediaMember,
  parseJoined,
  parseRelayError,
  parseRoomMessage,
  type Action,
  type Joined,
} from "./messages.ts";
import { holdPoint, reconcile, seekThreshold } from "./reconcile.ts";
import type { Relays } from "./relays.ts";
import { CONNECT_TIMEOUT_MS, later, type Connect, type Env, type TimerHandle } from "./runtime.ts";
import { checkClient, checkText } from "./text.ts";
import { isRunning, roomPosition } from "./timeline.ts";
import type {
  Candidate,
  Connection,
  Control,
  EndReason,
  Match,
  MatchState,
  Media,
  Mismatch,
  PartyEvent,
  PartyState,
  PlayerAdapter,
  PlayerState,
  Ranked,
  Relay,
  Resolver,
  Room,
} from "./types.ts";

export interface WatchPartyOptions<T> {
  relays: Relays;
  /** The real app's name and version as `<app>/<version>`, such as `Example/1.31.0`. */
  client: string;
  /** The user's display name, 1 to 32 characters long. */
  name: string;
  /**
   * The app's search, which finds the candidates for other participants' media.
   *
   * Without one, that media has no candidates, and `match` is `unavailable`.
   */
  resolver?: Resolver<T>;
  /** Opens sockets. The default is the global `WebSocket`. */
  connect?: Connect;
  /** Local monotonic time in ms, which the SDK rounds down to an integer. The default is `performance.now()`. */
  nowMs?: () => number;
  /** Random numbers in [0, 1). The default is `Math.random`. */
  random?: () => number;
}

interface StatusValues {
  epoch: number;
  player: PlayerState;
  match: Match | null;
  offsetMs: number;
}

/** A `joined` answer plus the messages that arrived after it, before the party took the link over. */
interface Answer {
  joined: Joined;
  buffered: Record<string, unknown>[];
}

interface Job {
  run: (adapter: PlayerAdapter) => Promise<void> | void;
  /** A `restore` (the room's rate back on a released adapter) is never dropped (SDK.md §11.2). */
  kind: "command" | "seek" | "restore";
}

/** One adapter's commands (SDK.md §11.1): they run one at a time, in order, each awaited. */
interface Lane {
  jobs: Job[];
  /** A command of this lane is running. */
  busy: boolean;
  /** The running command is a seek. */
  seeking: boolean;
}

interface Reconnect {
  attempt: number;
  timer: TimerHandle | undefined;
  deadline: TimerHandle;
  link: Link | null;
  abort: AbortController;
}

/** Where and as whom the party is in a room, for resuming after a drop (SDK.md §14). */
interface Session {
  url: string;
  code: string;
  peerId: string;
  token: string;
}

type Outcome<V> = { ok: true; value: V } | { ok: false; error: unknown } | "missed";

const RESOLVE_TIMEOUT_MS = 30_000;
const HEARTBEAT_MS = 15_000;
const STATUS_SPACING_MS = 250;
const TICK_MS = 500;
const HELD_LOADING_MS = 500;
const HOLD_MS = 2000;
const RESUME_MS = 300_000;
const BACKOFF_FIRST_MS = 500;
const BACKOFF_MAX_MS = 8000;
const CANCELLED = new PartyError("cancelled", "cancelled by leave() or close()");
// Sockets whose create, join or resume was cancelled after sending, which close themselves once answered, and sockets
// whose resume was answered while the clock sample is awaited, which the waiting attempt closes.
const lingering = new WeakSet<Link>();
const NO_CANDIDATES: readonly Ranked<never>[] = Object.freeze([]);

/**
 * One participant's session in a room (SDK.md §16.2).
 *
 * The party handles the connection, the room state, matching, status and following the room. The app supplies a
 * resolver, attaches its player and routes the user's controls here.
 */
export class WatchParty<T = unknown> {
  readonly #relays: Relays;
  readonly #client: string;
  readonly #name: string;
  readonly #resolver: Resolver<T> | null;
  readonly #env: Env;
  readonly #random: () => number;
  readonly #stateListeners = new Set<(state: PartyState<T>) => void>();
  readonly #eventListeners = new Set<(event: PartyEvent) => void>();
  #state: PartyState<T>;
  #closed = false;

  // Session.
  #connection: Connection = "idle";
  #endReason: EndReason | null = null;
  #attempt: AbortController | null = null;
  #attemptLinks: Link[] = [];
  #link: Link | null = null;
  #clock: Clock | null = null;
  /** Set by `joined`, cleared at the end. */
  #session: Session | null = null;
  #peerId: string | null = null;
  #room: Room | null = null;
  #media: Media | null = null;
  #epoch = 0;
  /** The descriptor, ref and start position of this party's last `setMedia`. */
  #own: { media: Media; ref: T; positionMs: number } | null = null;
  #isOwnMedia = false;
  #pendingSetMedia = false;
  #reconnect: Reconnect | null = null;

  // Matching.
  #match: Match | null = null;
  #choice: Candidate<T> | null = null;
  #mismatch: Mismatch<T> | null = null;
  #candidates: readonly Ranked<T>[] = NO_CANDIDATES;
  #matching: AbortController | null = null;
  #choosing: AbortController | null = null;
  #offsetMs = 0;

  // Player.
  #adapter: PlayerAdapter | null = null;
  /** The `ref` of the file the attached adapter has open. */
  #adapterRef: T | undefined = undefined;
  #away = false;
  #bound = false;
  /** The bound player refused to start (SDK.md §10.2). */
  #blocked = false;
  #appliedRate = 1;
  #nudging = false;
  #lastSeekAt: number | null = null;
  /** Every adapter with commands queued or running, released ones included. */
  readonly #lanes = new Map<PlayerAdapter, Lane>();
  /** Settles once no released adapter has a command left. */
  #releasedIdle: PromiseWithResolvers<void> | null = null;
  /** A step was requested while the attached adapter's lane was busy. */
  #stepWanted = false;
  #hold: { re: string; timer: TimerHandle } | null = null;
  #playTimer: TimerHandle | undefined;
  #tickTimer: TimerHandle | undefined;
  #player: PlayerState = "idle";

  // Status.
  #sent: StatusValues | null = null;
  #sentAt = -Infinity;
  #statusForce = false;
  #statusTimer: TimerHandle | undefined;
  #heartbeatTimer: TimerHandle | undefined;

  constructor(options: WatchPartyOptions<T>) {
    const nameProblem = checkText(options.name, 32);
    if (nameProblem !== null) throw new PartyError("invalid_message", `name ${nameProblem}`);
    const clientProblem = checkClient(options.client);
    if (clientProblem !== null) throw new PartyError("invalid_message", `client ${clientProblem}: ${options.client}`);
    this.#relays = options.relays;
    this.#client = options.client;
    this.#name = options.name;
    this.#resolver = options.resolver ?? null;
    this.#random = options.random ?? Math.random;
    const nowMs = options.nowMs ?? (() => performance.now());
    this.#env = {
      connect: options.connect ?? ((url) => new WebSocket(url)),
      // Round trips and positions computed from local time go on the wire as ints (SDK.md §2).
      nowMs: () => Math.floor(nowMs()),
    };
    this.#state = this.#build();
  }

  /**
   * Everything the UI shows, as one value.
   *
   * It is a new frozen object on every change.
   */
  get state(): PartyState<T> {
    return this.#state;
  }

  /**
   * Listens for state changes or events.
   *
   * Returns a function that stops listening.
   */
  on(type: "state", listener: (state: PartyState<T>) => void): () => void;
  on(type: "event", listener: (event: PartyEvent) => void): () => void;
  on(type: "state" | "event", listener: ((state: PartyState<T>) => void) | ((event: PartyEvent) => void)): () => void {
    const set = (type === "state" ? this.#stateListeners : this.#eventListeners) as Set<typeof listener>;
    set.add(listener);
    return () => set.delete(listener);
  }

  // ---------------------------------------------------------------------------------------------------------------
  // Create, join, leave

  /**
   * Creates a room and joins it as host (SDK.md §6).
   *
   * Resolves with the room code. With one relay in the list, the room is created there. With several, it is created
   * on the first relay to answer a clock ping.
   *
   * A relay that answers `relay_full` passes the create on to the next untried relay, in list order. When none is
   * left, the call rejects with the last relay's `relay_full`.
   */
  async create(): Promise<string> {
    const signal = this.#begin();
    try {
      const untried = [...this.#relays.list];
      const [first, opened] =
        untried.length === 1 ? [0, await this.#open(untried[0]!.url, signal)] : await this.#race(untried, signal);
      untried.splice(first, 1);
      let link = opened;
      for (;;) {
        try {
          const answer = await this.#hello(link, { type: "create", name: this.#name, client: this.#client }, signal);
          this.#enter(link, answer, false);
          return answer.joined.code;
        } catch (e) {
          const next = untried.shift();
          if (!(e instanceof PartyError && e.code === "relay_full") || next === undefined) throw e;
          link.close(1000);
          link = await this.#open(next.url, signal);
        }
      }
    } catch (e) {
      this.#failAttempt(signal);
      throw e;
    }
  }

  /**
   * Joins a room by its code, as typed or pasted (SDK.md §4, §7.2).
   *
   * With `create` true, the relay makes a room with the code when no room has it, and the party joins it as host.
   * Use it to bring a group back to a code it used before, such as one from a list of recent rooms. A relay that
   * answers `relay_full` ends the call: unlike `create()`, `join` never tries another relay.
   */
  async join(code: string, options?: { create?: boolean }): Promise<void> {
    this.#usable();
    const normalized = normalizeCode(code);
    const create = options?.create === undefined ? false : options.create;
    if (typeof create !== "boolean") throw new PartyError("invalid_message", "create must be a boolean");
    const signal = this.#begin();
    try {
      const list = this.#relays.list;
      let refreshed = false;
      let relay: Relay | null = routeCode(normalized, list);
      if (relay === null) {
        refreshed = true;
        await abortable(this.#relays.refresh(), signal);
        relay = routeCode(normalized, this.#relays.list);
        if (relay === null) throw new PartyError("room_not_found", `no relay owns ${normalized[0]}`);
      }
      let link: Link;
      let answer: Answer;
      try {
        [link, answer] = await this.#joinAt(relay.url, normalized, create, signal);
      } catch (e) {
        if (!(e instanceof PartyError && e.code === "room_not_found") || refreshed) throw e;
        await abortable(this.#relays.refresh(), signal);
        const moved = routeCode(normalized, this.#relays.list);
        if (moved === null || moved.url === relay.url) throw e;
        [link, answer] = await this.#joinAt(moved.url, normalized, create, signal);
      }
      this.#enter(link, answer, false);
    } catch (e) {
      this.#failAttempt(signal);
      throw e;
    }
  }

  /**
   * Leaves the room.
   *
   * The party sends `leave` and ends with `left`. While the party is still connecting, `leave` cancels the attempt
   * instead.
   */
  leave(): void {
    if (this.#attempt !== null) {
      this.#attempt.abort();
      for (const link of this.#attemptLinks) if (!lingering.has(link)) link.close();
      this.#attempt = null;
      this.#attemptLinks = [];
      this.#connection = "idle";
      this.#publish();
      return;
    }
    if (this.#connection === "connected") this.#link?.send({ type: "leave" });
    if (this.#connection === "connected" || this.#connection === "reconnecting") this.#end("left", null);
  }

  /**
   * Leaves the room, if the party is in one, and releases everything, the attached adapter included.
   *
   * The party can't be used again. The returned promise never rejects. It resolves once no released adapter has a
   * command running or queued, so await it before disposing of the player.
   */
  close(): Promise<void> {
    if (!this.#closed) {
      this.leave();
      this.#releaseAdapter();
      this.#closed = true;
      this.#stateListeners.clear();
      this.#eventListeners.clear();
    }
    return this.#releasedSettled();
  }

  // ---------------------------------------------------------------------------------------------------------------
  // Player

  /**
   * Attaches the app's player, together with `ref`, the `ref` of the file the player has open.
   *
   * `ref` is normally `state.choice.ref`. As host, it is the `ref` passed to `setMedia`. Call `attachPlayer` once per
   * file the player opens.
   *
   * The SDK follows the room with this player only while `state.choice.ref === ref` and the current media matched
   * (SDK.md §10.2). A player still holding an earlier choice's file is therefore never commanded. Attaching the same
   * player again replaces its ref and keeps its commands.
   *
   * It works outside a room too. A player attached while idle or after `ended` is followed in the next session. The
   * party's end releases the player, so after `ended`, attach a player that still has its file open again.
   */
  attachPlayer(player: PlayerAdapter, ref: T): void {
    this.#usable();
    if (player !== this.#adapter) {
      this.#releaseAdapter();
      this.#lastSeekAt = null;
    }
    this.#adapter = player;
    this.#adapterRef = ref;
    this.#away = false;
    this.#rebind();
    // A released adapter attached again is no longer released.
    this.#settleReleased();
    this.#changed();
    this.#step();
  }

  /**
   * Tells the SDK the user left the player, so the room doesn't wait for this participant.
   *
   * With `player`, it acts only when that is the attached adapter, and otherwise changes nothing. The returned promise
   * never rejects. It resolves once no released adapter has a command running or queued, so await it before
   * disposing of the player.
   */
  detachPlayer(player?: PlayerAdapter): Promise<void> {
    this.#usable();
    if (player === undefined || player === this.#adapter) {
      this.#releaseAdapter();
      this.#away = true;
      this.#changed();
    }
    return this.#releasedSettled();
  }

  /** Tells the SDK the player's state changed, so it reports and follows without waiting for the next tick. */
  playerChanged(): void {
    this.#usable();
    this.#changed();
    this.#step();
  }

  // ---------------------------------------------------------------------------------------------------------------
  // Matching

  /** Plays the offered file of a mismatch. */
  acceptMismatch(): void {
    this.#usable();
    const offered = this.#mismatch;
    if (offered === null) return;
    this.#choice = offered.candidate;
    this.#match = matchOf("mismatched", offered.deltaMs);
    this.#mismatch = null;
    this.#rebind();
    this.#changed();
    this.#step();
  }

  /** Declines the offered file, so this participant doesn't play the media. */
  declineMismatch(): void {
    this.#usable();
    const offered = this.#mismatch;
    if (offered === null) return;
    this.#match = matchOf("unavailable", offered.deltaMs);
    this.#mismatch = null;
    this.#choice = null;
    this.#rebind();
    this.#changed();
  }

  /**
   * Takes a file the user picked by hand as the new `choice`.
   *
   * The file becomes `choice` even when it has the current choice's id. The call resolves false, and nothing changes,
   * in these cases:
   *
   * - There is no media.
   * - The file has no known duration, and it has no probe or its probe fails.
   * - A later `choose`, or the end of matching, cancels it.
   */
  async choose(candidate: Candidate<T>): Promise<boolean> {
    this.#usable();
    const media = this.#media;
    if (media === null) return false;
    // The latest call wins (SDK.md §9.6): an earlier one still probing is cancelled and returns false.
    this.#choosing?.abort();
    this.#choosing = null;
    // A durationMs that isn't a duration (SDK.md §9.2) counts as absent.
    let duration = candidate.durationMs;
    if (!durationCounts(duration)) {
      const probe = candidate.probe;
      if (probe === undefined) return false;
      const ctl = new AbortController();
      this.#choosing = ctl;
      let out: Outcome<number | null | undefined>;
      try {
        out = await runCancellable((s) => probe(s), ctl.signal, null);
      } catch {
        return false;
      }
      // Superseded after the probe answered, before this continuation ran.
      if (this.#choosing !== ctl) return false;
      this.#choosing = null;
      if (out === "missed" || !out.ok || !durationCounts(out.value)) return false;
      duration = out.value;
    }
    if (this.#media !== media) return false;
    this.#cancelMatching();
    const delta = duration - media.durationMs;
    this.#choice = candidate;
    this.#mismatch = null;
    this.#match = matchOf(Math.abs(delta) <= MATCH_TOLERANCE_MS ? "matched" : "mismatched", delta);
    this.#rebind();
    this.#changed();
    this.#step();
    return true;
  }

  /**
   * Runs the resolver again for the current media.
   *
   * It does nothing without media, or for the party's own media. The offset and what plays stay as they are.
   *
   * - Without a choice, matching starts over (SDK.md §9.1).
   * - With a choice, only `candidates` changes, to the resolver's new list, ranked. Offer that list for `choose`.
   */
  resolveAgain(): void {
    this.#usable();
    const media = this.#media;
    if (media === null || this.#isOwnMedia) return;
    this.#cancelMatching();
    if (this.#choice === null) {
      this.#mismatch = null;
      this.#candidates = NO_CANDIDATES;
      this.#match = matchOf("searching");
      this.#startMatching(media, true);
    } else {
      this.#startMatching(media, false);
    }
    this.#changed();
  }

  // ---------------------------------------------------------------------------------------------------------------
  // Commands (SDK.md §12)

  /** Starts the room once everyone is ready. */
  play(): void {
    this.#member();
    this.#needMedia();
    this.#needControl();
    this.#link!.send({ type: "play" });
  }

  /** Pauses the room where this player stopped. */
  pause(): void {
    this.#member();
    this.#needMedia();
    this.#needControl();
    const msg: Record<string, unknown> = { type: "pause" };
    const adapter = this.#bound ? this.#adapter : null;
    if (adapter !== null) {
      const local = Math.floor(adapter.positionMs) - this.#offsetMs;
      if (!Number.isSafeInteger(local)) {
        throw new PartyError("invalid_message", "the player's position minus the offset is not an int");
      }
      msg["positionMs"] = Math.max(0, local);
      this.#enqueue(adapter, (a) => a.pause());
    }
    this.#link!.send(msg);
    this.#holdFor("pause");
  }

  /**
   * Moves the room to `positionMs` in the app's own file.
   *
   * `positionMs` must be an integer ≥ 0.
   */
  seek(positionMs: number): void {
    this.#member();
    this.#needMedia();
    this.#needControl();
    if (!Number.isSafeInteger(positionMs) || positionMs < 0) {
      throw new PartyError("invalid_message", "positionMs must be an integer ≥ 0");
    }
    const local = positionMs - this.#offsetMs;
    if (!Number.isSafeInteger(local)) throw new PartyError("invalid_message", "positionMs minus the offset is not an int");
    const adapter = this.#adapter;
    const seeks = adapter !== null && this.#bound;
    if (seeks) {
      this.#lastSeekAt = this.#env.nowMs();
      this.#enqueue(adapter, (a) => a.seek(positionMs), "seek");
    }
    this.#link!.send({ type: "seek", positionMs: Math.max(0, local) });
    this.#holdFor("seek");
    // The queued local seek is an SDK seek: the player is loading until it has run (SDK.md §10.2 rule 5, §12).
    if (seeks) this.#changed();
  }

  /** Sets the room's rate, from 0.5 to 2.0. */
  setRate(rate: number): void {
    this.#member();
    this.#needMedia();
    this.#needControl();
    if (!(rate >= 0.5 && rate <= 2)) throw new PartyError("invalid_message", "rate must be between 0.5 and 2.0");
    const adapter = this.#adapter;
    if (adapter !== null && this.#bound && typeof adapter.setRate === "function") {
      this.#appliedRate = rate;
      this.#enqueue(adapter, (a) => a.setRate?.(rate));
    }
    this.#link!.send({ type: "rate", rate });
    this.#holdFor("rate");
  }

  /**
   * Sets this participant's offset.
   *
   * The local position is the room position plus the offset. `offsetMs` must be an integer within ±604800000, which
   * is 7 days.
   */
  setOffset(offsetMs: number): void {
    this.#usable();
    if (!Number.isSafeInteger(offsetMs) || Math.abs(offsetMs) > MAX_OFFSET_MS) {
      throw new PartyError("invalid_message", `offsetMs must be an integer from −${MAX_OFFSET_MS} to ${MAX_OFFSET_MS}`);
    }
    this.#offsetMs = offsetMs;
    this.#changed();
    this.#step();
  }

  /**
   * As host, shares the media this party plays, starting at `positionMs` of its own file.
   *
   * `ref` is the app's value that reopens that file. While the room plays this descriptor, `state.choice` is a
   * candidate with id `""` and this `ref`. A player attached with this `ref` follows the room once the media arrives.
   * After handing host over and leaving the player, the user opens the file again like any guest.
   *
   * The SDK sends its own copy of `media`, and keeps it.
   */
  setMedia(media: Media, ref: T, positionMs = 0): void {
    this.#member();
    this.#needHost();
    const own = sendableMedia(media);
    if (!Number.isSafeInteger(positionMs) || positionMs < 0) {
      throw new PartyError("invalid_message", "positionMs must be an integer ≥ 0");
    }
    this.#own = { media: own, ref, positionMs };
    this.#pendingSetMedia = true;
    this.#rebind();
    this.#link!.send({ type: "setMedia", media: own, positionMs });
    this.#changed();
  }

  /** As host, stops the media. */
  clearMedia(): void {
    this.#member();
    this.#needHost();
    this.#link!.send({ type: "clearMedia" });
  }

  /** As host, sets who may play, pause, seek and set the rate. */
  setControl(mode: Control): void {
    this.#member();
    this.#needHost();
    if (mode !== "host" && mode !== "everyone") throw new PartyError("invalid_message", "mode must be host or everyone");
    this.#link!.send({ type: "setControl", mode });
  }

  /** As host, makes another participant the host. */
  transferHost(peerId: string): void {
    this.#member();
    this.#needHost();
    this.#needParticipant(peerId);
    this.#link!.send({ type: "transferHost", peerId });
  }

  /** As host, removes a participant from the room. */
  kick(peerId: string): void {
    this.#member();
    this.#needHost();
    this.#needParticipant(peerId);
    this.#link!.send({ type: "kick", peerId });
  }

  /**
   * As host, refuses or allows new joins.
   *
   * With `locked` true the room refuses new joins, and with `locked` false it allows them.
   */
  lock(locked: boolean): void {
    this.#member();
    this.#needHost();
    if (typeof locked !== "boolean") throw new PartyError("invalid_message", "locked must be a boolean");
    this.#link!.send({ type: "lock", locked });
  }

  /**
   * As host, ends the room for everyone.
   *
   * The party ends with `ended` once the relay confirms.
   */
  end(): void {
    this.#member();
    this.#needHost();
    this.#link!.send({ type: "end" });
  }

  /**
   * The room's position now, in the reference file.
   *
   * Returns null without media or without a clock sample.
   */
  roomPositionMs(): number | null {
    const now = this.#relayNow();
    if (this.#media === null || this.#room === null || now === null) return null;
    return roomPosition(this.#room, this.#media.durationMs, now);
  }

  // ---------------------------------------------------------------------------------------------------------------
  // Connecting

  #usable(): void {
    if (this.#closed) throw new Error("this WatchParty is closed");
  }

  #begin(): AbortSignal {
    this.#usable();
    if (this.#attempt !== null || this.#connection === "connected" || this.#connection === "reconnecting") {
      throw new PartyError("already_in_room", "the party is in a room or connecting");
    }
    this.#reset();
    this.#endReason = null;
    this.#attempt = new AbortController();
    this.#attemptLinks = [];
    this.#connection = "connecting";
    this.#publish();
    return this.#attempt.signal;
  }

  #failAttempt(signal: AbortSignal): void {
    if (this.#attempt?.signal !== signal) return;
    for (const link of this.#attemptLinks) if (!lingering.has(link)) link.close();
    this.#attempt = null;
    this.#attemptLinks = [];
    this.#connection = "idle";
    this.#publish();
  }

  #newLink(url: string): Link {
    const link = new Link(url, this.#env);
    this.#attemptLinks.push(link);
    return link;
  }

  async #open(url: string, signal: AbortSignal): Promise<Link> {
    const link = this.#newLink(url);
    await abortable(link.opened, signal);
    return link;
  }

  async #joinAt(url: string, code: string, create: boolean, signal: AbortSignal): Promise<[Link, Answer]> {
    const link = await this.#open(url, signal);
    const msg: Record<string, unknown> = { type: "join", code, name: this.#name, client: this.#client };
    if (create) msg["create"] = true;
    try {
      return [link, await this.#hello(link, msg, signal)];
    } catch (e) {
      if (!lingering.has(link)) link.close();
      throw e;
    }
  }

  /**
   * Connects to every relay at once (SDK.md §6). Resolves with the index and connection of the first relay whose
   * connection yields an accepted clock sample, and closes every other one. Rejects with `unreachable` when none does
   * within the connect timeout or every connection fails first.
   */
  #race(list: readonly Relay[], signal: AbortSignal): Promise<[number, Link]> {
    const links = list.map((r) => this.#newLink(r.url));
    const { promise, resolve, reject } = Promise.withResolvers<[number, Link]>();
    const finish = (winner: number, error: PartyError | null) => {
      clearTimeout(timer);
      signal.removeEventListener("abort", cancel);
      links.forEach((link, i) => {
        link.onPong = () => {};
        link.onClose = () => {};
        if (i !== winner) link.close(1000);
      });
      if (error !== null) reject(error);
      else resolve([winner, links[winner]!]);
    };
    const cancel = () => finish(-1, CANCELLED);
    const timer = setTimeout(
      () => finish(-1, new PartyError("unreachable", `no relay answered a ping within ${CONNECT_TIMEOUT_MS} ms`)),
      CONNECT_TIMEOUT_MS,
    );
    signal.addEventListener("abort", cancel, { once: true });
    let failed = 0;
    links.forEach((link, i) => {
      // Pongs come in arrival order; the first accepted sample wins.
      link.onPong = () => {
        if (link.clock.best !== null) finish(i, null);
      };
      link.onClose = () => {
        if (++failed === links.length) finish(-1, new PartyError("unreachable", "every relay connection failed"));
      };
    });
    return promise;
  }

  /**
   * Sends `create`, `join` or `resume` and waits for `joined` or the refusal, within the connect timeout. Cancelled
   * after sending, it rejects at once but keeps the socket until the answer: a late `joined` gets `leave`, so the relay
   * holds nobody for us.
   */
  #hello(link: Link, msg: Record<string, unknown>, signal: AbortSignal): Promise<Answer> {
    const re = msg["type"] as string;
    const { promise, resolve, reject } = Promise.withResolvers<Answer>();
    const buffered: Record<string, unknown>[] = [];
    let settled = false;
    const settle = (result: Answer | PartyError) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      signal.removeEventListener("abort", cancel);
      link.onMessage = (m) => buffered.push(m);
      link.onClose = () => {};
      if (result instanceof PartyError) reject(result);
      else resolve(result);
    };
    const cancel = () => {
      if (settled) return;
      settled = true;
      lingering.add(link);
      link.onMessage = (m) => {
        if (m["type"] === "joined") link.send({ type: "leave" });
        if (m["type"] === "joined" || (m["type"] === "error" && m["re"] === re)) {
          clearTimeout(timer);
          link.close(1000);
        }
      };
      link.onClose = () => clearTimeout(timer);
      reject(CANCELLED);
    };
    const timer = setTimeout(() => {
      if (settled) link.close(1000);
      else settle(new PartyError("unreachable", `no answer to ${re}`));
    }, CONNECT_TIMEOUT_MS);
    link.onClose = () => settle(new PartyError("unreachable", `the relay closed the connection before answering ${re}`));
    link.onMessage = (m) => {
      if (m["type"] === "joined") {
        const joined = parseJoined(m);
        if (joined !== null) {
          settle({ joined, buffered });
          return;
        }
        // A relay fault (SDK.md §8). A resume answered so stays resumable: no leave.
        if (re !== "resume") link.send({ type: "leave" });
        link.close(1000);
        settle(new PartyError("invalid_message", `the relay answered ${re} with a malformed joined`));
      } else if (m["type"] === "error" && m["re"] === re) {
        const error = parseRelayError(m);
        if (error !== null) settle(error);
      }
    };
    if (!link.send(msg)) settle(new PartyError("unreachable", `the connection to ${link.url} is closed`));
    else if (signal.aborted) cancel();
    else signal.addEventListener("abort", cancel, { once: true });
    return promise;
  }

  /** Takes the link over as the party's connection (SDK.md §7.3). */
  #enter(link: Link, answer: Answer, resumed: boolean): void {
    const { joined, buffered } = answer;
    this.#attempt = null;
    this.#attemptLinks = [];
    this.#link = link;
    this.#clock = link.clock;
    link.onMessage = (m) => this.#receive(link, m);
    link.onClose = (code) => this.#dropped(link, code);
    link.onPong = () => {};
    this.#session = { url: link.url, code: joined.code, peerId: joined.peerId, token: joined.token };
    this.#peerId = joined.peerId;
    const before = this.#room;
    this.#room = joined.room;
    let resend: Record<string, unknown> | null = null;
    if (!resumed || joined.room.epoch !== this.#epoch) {
      this.#epoch = joined.room.epoch;
      this.#media = joined.media;
      this.#pendingSetMedia = false;
      this.#newEpoch();
    } else if (this.#pendingSetMedia && this.#own !== null) {
      // The setMedia sent before the drop never reached the room (SDK.md §14): the host sends it again after the
      // status, still pending; anyone else drops it.
      const host = joined.room.participants.some((p) => p.peerId === joined.peerId && p.host);
      if (host) {
        resend = { type: "setMedia", media: this.#own.media, positionMs: this.#own.positionMs };
      } else {
        this.#pendingSetMedia = false;
        this.#rebind();
      }
    }
    this.#connection = "connected";
    this.#sent = null;
    this.#sentAt = -Infinity;
    this.#startTick();
    this.#refreshPlayer();
    this.#publish();
    if (resumed && before !== null) for (const e of participantEvents(before, joined.room)) this.#emit(e);
    this.#sendStatus();
    if (resend !== null) link.send(resend);
    this.#relays.connected();
    for (const m of buffered) this.#receive(link, m);
    this.#step();
  }

  #receive(link: Link, msg: Record<string, unknown>): void {
    if (link !== this.#link) return;
    switch (msg["type"]) {
      case "room": {
        const parsed = parseRoomMessage(msg);
        if (parsed === null || this.#room === null || parsed.room.rev <= this.#room.rev) return;
        let media: Media | null | undefined;
        if (parsed.room.epoch !== this.#epoch) {
          // The snapshot that starts an epoch carries its media; the party can't follow the room without it (SDK.md §8).
          media = mediaMember(msg["media"]);
          if (media === undefined) {
            link.send({ type: "leave" });
            this.#end("lost", new PartyError("invalid_message", "the relay sent a new epoch without valid media"));
            return;
          }
        }
        this.#acceptRoom(parsed.room, parsed.action, media);
        return;
      }
      case "error": {
        const error = parseRelayError(msg);
        if (error !== null) this.#relayError(error);
        return;
      }
      case "closed": {
        const reason = msg["reason"];
        if (typeof reason !== "string" || reason === "") return;
        this.#end(reason === "kicked" ? "kicked" : reason === "expired" ? "expired" : "ended", null);
        return;
      }
    }
  }

  /** An accepted snapshot; `media` is the new epoch's, undefined when the epoch stays. */
  #acceptRoom(room: Room, action: Action | null, media: Media | null | undefined): void {
    const before = this.#room!;
    this.#room = room;
    if (media !== undefined) {
      this.#epoch = room.epoch;
      this.#media = media;
      this.#pendingSetMedia = false;
      this.#newEpoch();
    }
    if (action !== null) this.#releaseHold();
    this.#refreshPlayer();
    this.#publish();
    for (const e of participantEvents(before, room)) this.#emit(e);
    if (action !== null) this.#emit(actionEvent(action, room, this.#peerId));
    this.#queueStatus();
    this.#step();
  }

  #relayError(error: PartyError): void {
    const re = error.re;
    if (this.#hold !== null && re === this.#hold.re) this.#releaseHold();
    if (re === "setMedia" && this.#pendingSetMedia) {
      this.#pendingSetMedia = false;
      this.#rebind();
    }
    this.#changed();
    this.#emit({ type: "error", error });
    this.#step();
  }

  #dropped(link: Link, code: number | null): void {
    if (link !== this.#link) return;
    this.#link = null;
    if (code === 4000) {
      this.#end("replaced", null);
      return;
    }
    this.#connection = "reconnecting";
    this.#stopStatus();
    this.#reconnect = {
      attempt: 0,
      timer: undefined,
      link: null,
      abort: new AbortController(),
      deadline: later(
        () => this.#end("lost", new PartyError("unreachable", `could not resume within ${RESUME_MS} ms`)),
        RESUME_MS,
      ),
    };
    this.#publish();
    this.#scheduleResume();
  }

  #scheduleResume(): void {
    const rc = this.#reconnect;
    if (rc === null) return;
    rc.attempt++;
    const delay = Math.min(BACKOFF_MAX_MS, BACKOFF_FIRST_MS * 2 ** (rc.attempt - 1)) * (0.5 + 0.5 * this.#random());
    // Delays are whole milliseconds (SDK.md §2).
    rc.timer = later(() => void this.#tryResume(rc), Math.floor(delay));
  }

  async #tryResume(rc: Reconnect): Promise<void> {
    const session = this.#session!;
    const link = new Link(session.url, this.#env);
    rc.link = link;
    try {
      await link.opened;
      if (this.#reconnect !== rc) throw CANCELLED;
      // The old snapshot and clock stay in use until the new connection has both (SDK.md §14).
      const answer = await this.#resumeAt(link, session, rc.abort.signal);
      if (this.#reconnect !== rc) throw CANCELLED;
      clearTimeout(rc.deadline);
      this.#reconnect = null;
      this.#enter(link, answer, true);
    } catch (e) {
      if (!lingering.has(link)) link.close(1000);
      if (this.#reconnect !== rc) return;
      rc.link = null;
      if (e instanceof PartyError && (e.code === "resume_failed" || e.code === "client_blocked")) {
        this.#end("lost", e);
        return;
      }
      // Any other refusal (rate_limited, unknown codes) is a failed attempt, retried within the window (SDK.md §14).
      this.#scheduleResume();
    }
  }

  /**
   * Sends `resume` on an open link and waits for `joined` plus a first clock sample, both within the connect timeout of
   * sending (SDK.md §14). Once `joined` has arrived the link is this call's to close: a cancel answers it with `leave`,
   * any other failure closes it and leaves the participant resumable.
   */
  async #resumeAt(link: Link, session: Session, signal: AbortSignal): Promise<Answer> {
    const { code, peerId, token } = session;
    const sentAt = this.#env.nowMs();
    const answer = await this.#hello(link, { type: "resume", code, peerId, token }, signal);
    lingering.add(link);
    try {
      await firstSample(link, signal, sentAt + CONNECT_TIMEOUT_MS - this.#env.nowMs());
    } catch (e) {
      // A resume answered after the party ended or the attempt was cancelled leaves at once (SDK.md §7.2).
      if (e === CANCELLED) link.send({ type: "leave" });
      link.close(1000);
      throw e;
    } finally {
      lingering.delete(link);
    }
    return answer;
  }

  /** Ends the party (SDK.md §7.4). */
  #end(reason: EndReason, error: PartyError | null): void {
    if (this.#connection === "ended" || this.#connection === "idle") return;
    // Releases the adapter while the room's rate is still known; a restore queued earlier still runs (SDK.md §11.2).
    this.#releaseAdapter();
    const rc = this.#reconnect;
    if (rc !== null) {
      this.#reconnect = null;
      clearTimeout(rc.timer);
      clearTimeout(rc.deadline);
      rc.abort.abort();
      if (rc.link !== null && !lingering.has(rc.link)) rc.link.close(1000);
    }
    this.#link?.close(1000);
    this.#link = null;
    this.#reset();
    this.#connection = "ended";
    this.#endReason = reason;
    this.#publish();
    this.#emit({ type: "ended", reason, error });
  }

  /**
   * Forgets the session: timers, matching and every room field. The attached adapter and its ref stay: only the
   * releases of SDK.md §11.2 let it go, the party's end among them, so one attached while idle or after the end is
   * followed in the session the next attempt starts (§7.2).
   */
  #reset(): void {
    clearTimeout(this.#tickTimer);
    clearTimeout(this.#playTimer);
    this.#tickTimer = undefined;
    this.#releaseHold();
    this.#stopStatus();
    this.#cancelMatching();
    this.#clock = null;
    this.#session = null;
    this.#peerId = null;
    this.#room = null;
    this.#media = null;
    this.#epoch = 0;
    this.#own = null;
    this.#isOwnMedia = false;
    this.#pendingSetMedia = false;
    this.#match = null;
    this.#choice = null;
    this.#mismatch = null;
    this.#candidates = NO_CANDIDATES;
    this.#offsetMs = 0;
    this.#away = false;
    this.#bound = false;
    this.#blocked = false;
    this.#nudging = false;
    this.#lastSeekAt = null;
    this.#player = "idle";
  }

  // ---------------------------------------------------------------------------------------------------------------
  // Media and matching (SDK.md §9)

  #newEpoch(): void {
    this.#cancelMatching();
    const media = this.#media;
    const own = media !== null && this.#own !== null && mediaEquals(media, this.#own.media) ? this.#own : null;
    this.#isOwnMedia = own !== null;
    this.#offsetMs = 0;
    this.#choice = null;
    this.#mismatch = null;
    this.#candidates = NO_CANDIDATES;
    if (media === null) {
      this.#match = null;
      this.#unbindAndPause();
    } else if (own !== null) {
      // The party's own file is the choice (SDK.md §9.1): a former host opens it again like any guest. The candidate is
      // the SDK's, so it is frozen; the ref stays the app's.
      this.#choice = Object.freeze({ id: "", ref: own.ref, durationMs: own.media.durationMs });
      this.#match = matchOf("matched", 0);
      this.#unbind();
      this.#rebind();
    } else {
      this.#unbindAndPause();
      this.#match = matchOf("searching");
      this.#startMatching(media, true);
    }
  }

  /**
   * Starts the resolver for `media`, then the walk (SDK.md §9.5), or with `walk` false only the ranking, for
   * `resolveAgain()` with a choice. Without a resolver there are no candidates: a walk ends `unavailable` at once.
   */
  #startMatching(media: Media, walk: boolean): void {
    const resolver = this.#resolver;
    if (resolver === null) {
      this.#candidates = NO_CANDIDATES;
      if (walk) this.#match = matchOf("unavailable");
      return;
    }
    const ctl = new AbortController();
    this.#matching = ctl;
    void this.#runMatching(resolver, media, walk, ctl.signal);
  }

  /** Cancels the walk and any manual probe (SDK.md §9.1, §9.6). */
  #cancelMatching(): void {
    this.#matching?.abort();
    this.#matching = null;
    this.#choosing?.abort();
    this.#choosing = null;
  }

  async #runMatching(resolver: Resolver<T>, media: Media, walk: boolean, signal: AbortSignal): Promise<void> {
    let found: Candidate<T>[] = [];
    try {
      const out = await runCancellable((s) => resolver.resolve(media, s), signal, RESOLVE_TIMEOUT_MS);
      if (out === "missed") {
        const error = new DOMException(`the resolver took over ${RESOLVE_TIMEOUT_MS} ms`, "TimeoutError");
        this.#emit({ type: "resolveFailed", error });
      } else if (!out.ok) this.#emit({ type: "resolveFailed", error: out.error });
      else if (Array.isArray(out.value)) found = out.value;
    } catch {
      return;
    }
    if (signal.aborted) return;
    const ranked = rankCandidates(media, found);
    // Each entry is frozen; its candidate stays the app's.
    this.#candidates = Object.freeze(
      ranked.map((c) => Object.freeze({ candidate: c, deltaMs: durationCounts(c.durationMs) ? c.durationMs - media.durationMs : null })),
    );
    if (!walk) {
      this.#matching = null;
      this.#publish();
      return;
    }
    this.#publish();
    let result;
    try {
      result = await walkCandidates(
        media,
        ranked,
        (c, deadlineMs) => this.#probe(c, deadlineMs, signal),
        (i, deltaMs) => {
          const entry = this.#candidates[i];
          if (signal.aborted || entry === undefined || entry.deltaMs !== null) return;
          this.#candidates = Object.freeze(
            this.#candidates.map((r, j) => (j === i ? Object.freeze({ candidate: r.candidate, deltaMs: Math.round(deltaMs) }) : r)),
          );
          this.#publish();
        },
      );
    } catch {
      return;
    }
    if (signal.aborted) return;
    this.#matching = null;
    const delta = result.deltaMs === null ? 0 : Math.round(result.deltaMs);
    if (result.result === "matched") {
      this.#choice = result.choice;
      this.#match = matchOf("matched", delta);
    } else if (result.result === "mismatch" && result.choice !== null) {
      this.#mismatch = Object.freeze({ candidate: result.choice, deltaMs: delta });
      this.#match = matchOf("searching", delta);
    } else {
      this.#match = matchOf("unavailable");
    }
    this.#rebind();
    this.#changed();
    this.#step();
  }

  /**
   * One probe of the walk. The deadline is inclusive (SDK.md §9.5): an answer when exactly `deadlineMs` has passed is in
   * time, so the cancel comes 1 ms later, and the time taken is measured.
   */
  async #probe(c: Candidate<T>, deadlineMs: number, signal: AbortSignal): Promise<ProbeOutcome> {
    const started = this.#env.nowMs();
    const out = await runCancellable((s) => c.probe!(s), signal, Math.max(0, deadlineMs) + 1);
    const tookMs = this.#env.nowMs() - started;
    if (out === "missed" || tookMs > deadlineMs) return "missed";
    const value = out.ok ? out.value : null;
    return { durationMs: typeof value === "number" ? value : null, tookMs };
  }

  // ---------------------------------------------------------------------------------------------------------------
  // The player (SDK.md §10.2, §11)

  /**
   * Whether the attached adapter plays the current media's file (SDK.md §10.2): it is attached with the choice's
   * `ref`, the current media matched or the user accepted the mismatch, and no `setMedia` of this party is pending.
   */
  #bindable(): boolean {
    const state = this.#match?.state;
    return (
      this.#adapter !== null &&
      !this.#away &&
      this.#media !== null &&
      !this.#pendingSetMedia &&
      this.#choice !== null &&
      this.#choice.ref === this.#adapterRef &&
      (state === "matched" || state === "mismatched")
    );
  }

  /** Re-evaluates binding after any of its inputs changed; binding and unbinding act only on a change. */
  #rebind(): void {
    const bindable = this.#bindable();
    if (bindable === this.#bound) return;
    if (bindable) this.#bind();
    else this.#unbind();
  }

  /** Binds the attached adapter: nudging off, the room's rate applied. The caller runs a step afterwards. */
  #bind(): void {
    const adapter = this.#adapter!;
    this.#bound = true;
    this.#nudging = false;
    const rate = this.#room?.rate ?? 1;
    this.#appliedRate = rate;
    if (typeof adapter.setRate === "function") this.#enqueue(adapter, (a) => a.setRate?.(rate));
  }

  #unbind(): void {
    this.#bound = false;
    this.#blocked = false;
    this.#nudging = false;
    clearTimeout(this.#playTimer);
  }

  #unbindAndPause(): void {
    this.#unbind();
    if (this.#adapter !== null) this.#enqueue(this.#adapter, (a) => a.pause());
  }

  /**
   * Releases the attached adapter (SDK.md §11.2): its commands that haven't started are dropped, a running one
   * finishes, and a nudged one gets the room's rate back once. Nothing else reaches it.
   */
  #releaseAdapter(): void {
    const adapter = this.#adapter;
    if (adapter === null) return;
    const nudging = this.#nudging;
    this.#unbind();
    this.#adapter = null;
    this.#adapterRef = undefined;
    this.#stepWanted = false;
    const lane = this.#lanes.get(adapter);
    if (lane !== undefined) lane.jobs = lane.jobs.filter((j) => j.kind === "restore");
    const rate = this.#room?.rate;
    if (nudging && rate !== undefined && typeof adapter.setRate === "function") {
      this.#enqueue(adapter, (a) => a.setRate?.(rate), "restore");
    }
  }

  /** Resolves once no released adapter has a command running or queued; at once when none has. */
  #releasedSettled(): Promise<void> {
    if (!this.#releasedBusy()) return Promise.resolve();
    this.#releasedIdle ??= Promise.withResolvers<void>();
    return this.#releasedIdle.promise;
  }

  #releasedBusy(): boolean {
    for (const [adapter, lane] of this.#lanes) if (adapter !== this.#adapter && lane.busy) return true;
    return false;
  }

  #settleReleased(): void {
    if (this.#releasedIdle === null || this.#releasedBusy()) return;
    this.#releasedIdle.resolve();
    this.#releasedIdle = null;
  }

  #enqueue(adapter: PlayerAdapter, run: Job["run"], kind: Job["kind"] = "command"): void {
    let lane = this.#lanes.get(adapter);
    if (lane === undefined) {
      lane = { jobs: [], busy: false, seeking: false };
      this.#lanes.set(adapter, lane);
    }
    lane.jobs.push({ run, kind });
    if (!lane.busy) void this.#drain(adapter, lane);
  }

  // Each adapter's commands run one at a time, in order (SDK.md §11.1); lanes of different adapters never wait for each
  // other. No step starts while the attached adapter's lane is busy.
  async #drain(adapter: PlayerAdapter, lane: Lane): Promise<void> {
    lane.busy = true;
    for (let job = lane.jobs.shift(); job !== undefined; job = lane.jobs.shift()) {
      lane.seeking = job.kind === "seek";
      try {
        await job.run(adapter);
      } catch {
        // A failed command is corrected by a later step.
      }
      lane.seeking = false;
    }
    lane.busy = false;
    this.#lanes.delete(adapter);
    if (adapter !== this.#adapter) {
      this.#settleReleased();
      return;
    }
    if (this.#connection === "connected" || this.#connection === "reconnecting") this.#changed();
    if (this.#stepWanted) {
      this.#stepWanted = false;
      this.#step();
    }
  }

  #relayNow(): number | null {
    const best = this.#clock?.best ?? null;
    return best === null ? null : this.#env.nowMs() + best.offsetMs;
  }

  /** One reconciliation step (SDK.md §11.2) when the adapter is bound, the room has media and relay time is known. */
  #step(): void {
    clearTimeout(this.#playTimer);
    const adapter = this.#adapter;
    const room = this.#room;
    const media = this.#media;
    if (!this.#bound || adapter === null || room === null || media === null) return;
    const now = this.#relayNow();
    if (now === null) return;
    if (this.#lanes.get(adapter)?.busy === true) {
      this.#stepWanted = true;
      return;
    }
    if (this.#hold !== null) return;
    const local = this.#env.nowMs();
    const out = reconcile({
      room,
      durationMs: media.durationMs,
      serverNowMs: now,
      offsetMs: this.#offsetMs,
      player: {
        positionMs: Math.floor(adapter.positionMs),
        paused: adapter.paused,
        buffering: adapter.buffering,
        durationMs: adapter.durationMs ?? null,
        canSetRate: typeof adapter.setRate === "function",
        rate: this.#appliedRate,
        seekAccuracyMs: seekAccuracyOf(adapter),
      },
      nudging: this.#nudging,
      sinceSeekMs: this.#lastSeekAt === null ? null : local - this.#lastSeekAt,
    });
    this.#nudging = out.nudging;
    for (const action of out.actions) {
      switch (action.do) {
        case "pause":
          this.#enqueue(adapter, (a) => a.pause());
          break;
        case "setRate":
          this.#appliedRate = action.rate;
          this.#enqueue(adapter, (a) => a.setRate?.(action.rate));
          break;
        case "seek":
          this.#lastSeekAt = local;
          this.#enqueue(adapter, (a) => a.seek(action.positionMs), "seek");
          break;
        case "play":
          this.#enqueue(adapter, (a) => this.#play(a));
          break;
      }
    }
    // A start further away than the longest timer (SDK.md §2) gets that timer; the step it runs works out the rest.
    if (out.playInMs !== null) this.#playTimer = later(() => this.#step(), out.playInMs);
    this.#changed();
  }

  #startTick(): void {
    if (this.#tickTimer !== undefined) return;
    const tick = () => {
      this.#tickTimer = setTimeout(tick, TICK_MS);
      this.#changed();
      this.#step();
    };
    this.#tickTimer = setTimeout(tick, TICK_MS);
  }

  /** The player state of SDK.md §10.2. */
  #computePlayer(): PlayerState {
    const { adapter, room, media } = { adapter: this.#adapter, room: this.#room, media: this.#media };
    if (media === null || this.#match?.state === "unavailable" || this.#away) return "idle";
    if (!this.#bound || adapter === null || room === null) return "loading";
    // A seek the SDK issued is in flight from the moment it is queued.
    const lane = this.#lanes.get(adapter);
    if (!adapter.firstFrameShown || lane?.seeking === true || lane?.jobs.some((j) => j.kind === "seek")) return "loading";
    const now = this.#relayNow();
    // Without relay time the SDK can't tell where a playing room is.
    if (room.phase === "playing" && now === null) return "loading";
    const hold = holdPoint(room, this.#offsetMs, now ?? 0, adapter.durationMs ?? null);
    const heldLimit = seekThreshold(HELD_LOADING_MS, seekAccuracyOf(adapter));
    if (hold !== null && Math.abs(Math.floor(adapter.positionMs) - hold.holdAtMs) > heldLimit) return "loading";
    if (this.#blocked) return "loading";
    if (adapter.buffering) return hold === null && now !== null && isRunning(room, now) ? "buffering" : "loading";
    return "ready";
  }

  #refreshPlayer(): void {
    // Blocked ends once the player reads not paused or the room stops playing (SDK.md §10.2).
    if (this.#blocked && (this.#adapter?.paused !== true || this.#room?.phase !== "playing")) this.#blocked = false;
    this.#player = this.#computePlayer();
  }

  /**
   * A step's play. A play that throws while the bound adapter then reads paused means the player refused to start,
   * such as a browser that wants a user gesture: the player is blocked, reported as loading, and the app hears it once
   * (SDK.md §10.2).
   */
  async #play(adapter: PlayerAdapter): Promise<void> {
    try {
      await adapter.play();
    } catch (error) {
      const refused = adapter === this.#adapter && this.#bound && adapter.paused && this.#room?.phase === "playing";
      if (!refused || this.#blocked) return;
      this.#blocked = true;
      this.#changed();
      this.#emit({ type: "playBlocked", error });
    }
  }

  /** Something the state or status shows changed: recompute, publish, report. */
  #changed(): void {
    this.#refreshPlayer();
    this.#publish();
    this.#queueStatus();
  }

  // ---------------------------------------------------------------------------------------------------------------
  // Status (SDK.md §10.3)

  #values(): StatusValues {
    return { epoch: this.#epoch, player: this.#player, match: this.#match, offsetMs: this.#offsetMs };
  }

  #queueStatus(force = false): void {
    if (this.#connection !== "connected" || this.#link === null) return;
    if (force) this.#statusForce = true;
    const values = this.#values();
    if (!this.#statusForce && this.#sent !== null && !statusDiffers(values, this.#sent)) return;
    // A status for a new epoch goes out at once: the relay waits on it after setMedia.
    const newEpoch = this.#sent !== null && values.epoch !== this.#sent.epoch;
    const wait = this.#sentAt + STATUS_SPACING_MS - this.#env.nowMs();
    if (wait > 0 && !newEpoch) {
      this.#statusTimer ??= later(() => {
        this.#statusTimer = undefined;
        this.#queueStatus();
      }, wait);
      return;
    }
    this.#sendStatus();
  }

  #sendStatus(): void {
    const link = this.#link;
    if (link === null) return;
    clearTimeout(this.#statusTimer);
    this.#statusTimer = undefined;
    this.#statusForce = false;
    const values = this.#values();
    const msg: Record<string, unknown> = { type: "status", ...values };
    const best = link.clock.best;
    if (best !== null) msg["rttMs"] = best.rttMs;
    link.send(msg);
    this.#sent = values;
    this.#sentAt = this.#env.nowMs();
    clearTimeout(this.#heartbeatTimer);
    this.#heartbeatTimer = setTimeout(() => this.#queueStatus(true), HEARTBEAT_MS);
  }

  #stopStatus(): void {
    clearTimeout(this.#statusTimer);
    clearTimeout(this.#heartbeatTimer);
    this.#statusTimer = undefined;
    this.#heartbeatTimer = undefined;
    this.#statusForce = false;
  }

  // ---------------------------------------------------------------------------------------------------------------
  // Command checks and the command hold

  #member(): void {
    this.#usable();
    if (this.#connection !== "connected" || this.#link === null) throw new PartyError("not_in_room", "not in a room");
  }

  #needMedia(): void {
    if (this.#media === null) throw new PartyError("invalid_message", "the room has no media");
  }

  #needControl(): void {
    if (!this.#state.canControl) throw new PartyError("forbidden", "only a controller may do this");
  }

  #needHost(): void {
    if (!this.#state.isHost) throw new PartyError("forbidden", "only the host may do this");
  }

  #needParticipant(peerId: string): void {
    if (!this.#room?.participants.some((p) => p.peerId === peerId)) {
      throw new PartyError("invalid_message", `no participant ${peerId}`);
    }
  }

  #holdFor(re: string): void {
    this.#releaseHold();
    this.#hold = {
      re,
      timer: setTimeout(() => {
        this.#hold = null;
        this.#step();
      }, HOLD_MS),
    };
  }

  #releaseHold(): void {
    if (this.#hold === null) return;
    clearTimeout(this.#hold.timer);
    this.#hold = null;
  }

  // ---------------------------------------------------------------------------------------------------------------
  // State and events

  #build(): PartyState<T> {
    const room = this.#room;
    const me = room?.participants.find((p) => p.peerId === this.#peerId) ?? null;
    const isHost = me?.host === true;
    // Every member is frozen already or is the app's own choice.
    return Object.freeze({
      connection: this.#connection,
      peerId: this.#peerId,
      room,
      media: this.#media,
      match: this.#match,
      choice: this.#choice,
      mismatch: this.#mismatch,
      candidates: this.#candidates,
      offsetMs: this.#offsetMs,
      player: this.#player,
      endReason: this.#endReason,
      me,
      isHost,
      canControl: isHost || (me !== null && room?.control === "everyone"),
    });
  }

  #publish(): void {
    const next = this.#build();
    const prev = this.#state as unknown as Record<string, unknown>;
    const changed = Object.entries(next).some(([k, v]) => prev[k] !== v);
    if (!changed) return;
    this.#state = next;
    for (const listener of [...this.#stateListeners]) call(listener, next);
  }

  #emit(event: PartyEvent): void {
    Object.freeze(event);
    for (const listener of [...this.#eventListeners]) call(listener, event);
  }
}

// A listener that throws doesn't break the party; its error surfaces on its own.
function call<V>(listener: (value: V) => void, value: V): void {
  try {
    listener(value);
  } catch (e) {
    queueMicrotask(() => {
      throw e;
    });
  }
}

/** A frozen `{state, deltaMs}`. */
function matchOf(state: MatchState, deltaMs?: number): Match {
  return Object.freeze(deltaMs === undefined ? { state } : { state, deltaMs });
}

/** The adapter's `seekAccuracyMs`: any value that isn't an integer ≥ 0 counts as 0 (SDK.md §16.4). */
function seekAccuracyOf(adapter: PlayerAdapter): number {
  const a = adapter.seekAccuracyMs;
  return typeof a === "number" && Number.isInteger(a) && a >= 0 ? a : 0;
}

function statusDiffers(a: StatusValues, b: StatusValues): boolean {
  const sameMatch =
    a.match === b.match ||
    (a.match !== null && b.match !== null && a.match.state === b.match.state && a.match.deltaMs === b.match.deltaMs);
  return a.epoch !== b.epoch || a.player !== b.player || a.offsetMs !== b.offsetMs || !sameMatch;
}

function actionEvent(action: Action, room: Room, myPeerId: string | null): PartyEvent {
  return {
    type: "action",
    kind: action.kind,
    byPeerId: action.by,
    by: room.participants.find((p) => p.peerId === action.by) ?? null,
    byMe: action.by !== null && action.by === myPeerId,
    room,
  };
}

function participantEvents(before: Room, after: Room): PartyEvent[] {
  const had = new Set(before.participants.map((p) => p.peerId));
  const has = new Set(after.participants.map((p) => p.peerId));
  return [
    ...after.participants.filter((p) => !had.has(p.peerId)).map((p) => ({ type: "participantJoined" as const, participant: p })),
    ...before.participants.filter((p) => !has.has(p.peerId)).map((p) => ({ type: "participantLeft" as const, participant: p })),
  ];
}

/** Settles like `p`, or rejects with CANCELLED as soon as `signal` aborts. */
function abortable<V>(p: Promise<V>, signal: AbortSignal): Promise<V> {
  if (signal.aborted) return Promise.reject(CANCELLED);
  const { promise, resolve, reject } = Promise.withResolvers<V>();
  const cancel = () => reject(CANCELLED);
  signal.addEventListener("abort", cancel, { once: true });
  p.then(resolve, reject).finally(() => signal.removeEventListener("abort", cancel));
  return promise;
}

/** Waits until the link's clock has a sample (SDK.md §14); fails with `unreachable` after `timeoutMs`. */
function firstSample(link: Link, signal: AbortSignal, timeoutMs: number): Promise<void> {
  if (link.clock.best !== null) return Promise.resolve();
  const { promise, resolve, reject } = Promise.withResolvers<void>();
  const finish = (error: PartyError | null) => {
    clearTimeout(timer);
    signal.removeEventListener("abort", cancel);
    link.onPong = () => {};
    link.onClose = () => {};
    if (error === null) resolve();
    else reject(error);
  };
  const cancel = () => finish(CANCELLED);
  const timer = later(() => finish(new PartyError("unreachable", "no clock sample within the connect timeout of sending resume")), timeoutMs);
  signal.addEventListener("abort", cancel, { once: true });
  link.onPong = () => {
    if (link.clock.best !== null) finish(null);
  };
  link.onClose = () => finish(new PartyError("unreachable", "the relay closed the connection"));
  return promise;
}

/**
 * Runs cancellable work with its own AbortSignal. Resolves with its value or error, or "missed" (after aborting it)
 * when `timeoutMs` passes first; rejects with CANCELLED when `parent` aborts.
 */
function runCancellable<V>(
  work: (signal: AbortSignal) => Promise<V> | V,
  parent: AbortSignal,
  timeoutMs: number | null,
): Promise<Outcome<V>> {
  if (parent.aborted) return Promise.reject(CANCELLED);
  const { promise, resolve, reject } = Promise.withResolvers<Outcome<V>>();
  const child = new AbortController();
  let timer: TimerHandle | undefined;
  let settled = false;
  const settle = (finish: () => void) => {
    if (settled) return;
    settled = true;
    clearTimeout(timer);
    parent.removeEventListener("abort", onAbort);
    finish();
  };
  const onAbort = () =>
    settle(() => {
      child.abort();
      reject(CANCELLED);
    });
  parent.addEventListener("abort", onAbort, { once: true });
  if (timeoutMs !== null) {
    timer = later(
      () =>
        settle(() => {
          child.abort();
          resolve("missed");
        }),
      timeoutMs,
    );
  }
  Promise.resolve()
    .then(() => work(child.signal))
    .then(
      (value) => settle(() => resolve({ ok: true, value })),
      (error: unknown) => settle(() => resolve({ ok: false, error })),
    );
  return promise;
}


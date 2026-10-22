`openwatchparty` keeps players in different apps in sync in one room. Each person in the room plays their own copy of the title, and a relay keeps the copies together.

The SDK implements the client behaviour in [`spec/SDK.md`](../../spec/SDK.md). This page names the rules the SDK follows, and SDK.md explains them in full.

## Install

```sh
npm install openwatchparty
```

## Requirements

The package runs in Node and in browsers from these versions on:

| Runtime | Minimum |
|---|---|
| Node | 22 |
| Chrome / Edge | 119 |
| Firefox | 121 |
| Safari | 17.4 |

The package is ESM only, with no `require` condition, and it has no runtime dependencies. TypeScript consumers need the `dom` lib, or `skipLibCheck`. A global `WebSocket` must exist, even when you pass a custom `connect`.

## Quick start

This example runs in a browser page with a `<video>` element. It joins the room named in the page's `room` query parameter. Without one, it creates a room and shares the video as host. Whenever the SDK chooses a file, the example loads it into the `<video>` and attaches the element with `attachVideo`.

```ts
import { Relays, WatchParty, attachVideo, formatCode, type Resolver } from "openwatchparty";

// One relay list for the whole app, passed to every party.
const relays = Relays.public();

// The app's search: the files the user could play for the room's title, best first.
const resolver: Resolver<string> = {
  async resolve(media, signal) {
    const res = await fetch(`/api/search?title=${encodeURIComponent(media.title)}`, { signal });
    const files: { id: string; url: string; durationMs: number }[] = await res.json();
    // ref: the absolute URL, as video.src reads it back
    return files.map((f) => ({ id: f.id, ref: new URL(f.url, document.baseURI).href, durationMs: f.durationMs }));
  },
};

const party = new WatchParty<string>({ relays, client: "Example/1.0", name: "Ann", resolver });
const video = document.querySelector("video")!;

// Open each choice, one at a time, and attach the <video> with its ref.
let attached: string | null = null; // the ref the <video> is attached with
let detach: (() => Promise<void>) | null = null;
let opening = false;
async function openChoice(): Promise<void> {
  if (opening) return; // the open under way goes on to the newest choice
  opening = true;
  try {
    while (true) {
      const ref = party.state.choice?.ref;
      if (ref === undefined || ref === attached) return; // no choice, or attached: it follows by itself
      if (video.src !== ref) {
        const { promise, resolve } = Promise.withResolvers<void>();
        const loading = new AbortController();
        for (const type of ["loadeddata", "error"]) video.addEventListener(type, () => resolve(), { signal: loading.signal });
        video.src = ref;
        await promise; // loaded, or failed to
        loading.abort(); // removes both listeners
      }
      if (party.state.choice?.ref !== ref) continue; // a newer choice came meanwhile: open that one
      const previous = detach;
      detach = attachVideo(party, video, ref);
      attached = ref;
      void previous?.(); // removes the earlier file's control listeners
    }
  } finally {
    opening = false;
  }
}
party.on("state", () => void openChoice());

party.on("event", (event) => {
  if (event.type !== "ended") return;
  console.log(`Out of the room: ${event.reason}`);
  void detach?.(); // the end released the <video>: its controls stop going to the party
  detach = attached = null;
});

const code = new URLSearchParams(location.search).get("room");
if (code !== null) {
  await party.join(code);
} else {
  console.log(`Room code: ${formatCode(await party.create())}`);
  // Host: share the file the <video> plays.
  party.setMedia(
    { kind: "movie", title: "Example Movie", year: 2024, ids: { tmdb: "12345" }, durationMs: Math.round(video.duration * 1000) },
    video.src,
    Math.floor(video.currentTime * 1000),
  );
}
```

[`examples/web`](../../examples/web/README.md) is a full browser page built on `attachVideo`.

## Relays

A `Relays` object tells your parties which relays exist and which one owns a room code. Create one for the whole app and pass it to every party, as the quick start does.

There are two kinds of list:

- `Relays.public()` uses the public relays. It starts from the copy of [`relays.json`](../../relays.json) compiled into the SDK. Each valid list a refresh reads from `https://openwatch.party/relays.json` replaces it in memory.
- `Relays.at(baseUrl)` uses one [self-hosted relay](../../relay/README.md) for every room. Its `list` is that relay alone, and it fetches nothing.

`Relays.at` needs a lower-case `https://` or `http://` URL with a host, such as `https://relay.example.com`. Any other base throws a `PartyError` with the code `invalid_message`.

| Member | What it does |
|---|---|
| `static public(options?: RelaysOptions): Relays` | Creates a list of the public relays. |
| `static at(baseUrl: string): Relays` | Creates a list of one self-hosted relay. |
| `list: readonly Relay[]` | The relays in use. |
| `relayFor(code: string): Relay \| null` | Finds the relay that owns a room code. |
| `refresh(): Promise<boolean>` | Fetches the public list now, and resolves with whether a valid list came back. |

`refresh()` resolves `false` when no valid list came back, and the list in use stays. On a `Relays.at` list, it does nothing and resolves `true`.

`RelaysOptions` has one member, `httpGet: (url: string, signal: AbortSignal) => Promise<string>`. By default it is `fetch` with `redirect: "error"`. A custom `httpGet` must follow these rules:

- It stops when `signal` aborts.
- It follows no redirects.
- It fails on a non-200 status.

## WatchParty

A `WatchParty` is one participant's session. It connects to a relay, keeps the room's state, matches the room's media to the user's own files and drives your player.

### Creating a party

The constructor, `new WatchParty<T>`, takes a `WatchPartyOptions<T>` object with these options. `T` is the type of your app's file `ref`, the value your app uses to open a file.

| Option | Type | What it is |
|---|---|---|
| `relays` | `Relays` | The app's relay list. Required. |
| `client` | `string` | Your app and its version, as `<app>/<version>`, such as `Example/1.0`. Required. |
| `name` | `string` | The user's display name, 1–32 characters. Required. |
| `resolver` | `Resolver<T>` | Your search for the user's own files. There is none by default. |
| `connect` | `(url: string) => WebSocket` | Opens a socket. Defaults to `new WebSocket(url)`. |
| `nowMs` | `() => number` | The monotonic local clock. Defaults to `performance.now()`. |
| `random` | `() => number` in [0, 1) | Draws random numbers. Defaults to `Math.random`. |

The constructor throws a `PartyError` with the code `invalid_message` for a bad `client` or `name`. Without a resolver, other participants' media has no candidates, so `match` is `unavailable`.

A custom `connect` must return a socket that behaves like a browser `WebSocket`. The SDK uses its `onopen`, `onmessage`, `onclose` and `onerror` handlers, its `readyState`, `send(string)` and `close(code)`.

### State and session

| Member | What it does |
|---|---|
| `state: PartyState<T>` | Everything the UI shows, as one value. |
| `on("state", listener)` | Calls `listener` with every new state, and returns a function that unsubscribes. |
| `on("event", listener)` | Calls `listener` with every event, and returns a function that unsubscribes. |
| `create(): Promise<string>` | Creates a room, joins it as host and resolves with the room code. |
| `join(code: string, options?: { create?: boolean }): Promise<void>` | Joins a room by its code, as the user typed or pasted it. With `{ create: true }`, it makes the room when no room has the code. |
| `leave(): void` | Leaves the room. |
| `close(): Promise<void>` | Leaves the room and releases everything. |
| `roomPositionMs(): number \| null` | The room's position now. |

`leave()` during `create` or `join` cancels the attempt. An ended party may `create` or `join` again, but after `close()` the party can't be used again.

`join(code, { create: true })` brings a group back to a code it used before, such as one from your app's list of recent rooms. When the room exists, the party joins it. When no room has the code, the relay makes one with that code, and the party is its host. A relay that is full rejects the call with `relay_full`, and the SDK doesn't try another relay. A relay that predates `create` rejects it with `room_not_found`.

### Matching and the player

| Member | What it does |
|---|---|
| `acceptMismatch(): void` | Plays the offered file, though its duration differs. |
| `declineMismatch(): void` | Declines the offered file. |
| `choose(candidate: Candidate<T>): Promise<boolean>` | Takes a file the user picked by hand. |
| `resolveAgain(): void` | Searches again for the current media. |
| `attachPlayer(player: PlayerAdapter, ref: T): void` | Lets the SDK drive your player, which has the file of `ref` open. |
| `detachPlayer(player?: PlayerAdapter): Promise<void>` | Stops driving the player when the user leaves it. |
| `playerChanged(): void` | Tells the SDK the player's state changed, so it reads it now. |

`choose` resolves `false` when it can't take the file, including when the room has no media. [Player and resolver](#player-and-resolver) explains how to use the player members.

### Playback

These commands act on the whole room. Send the user's presses through them, not straight to the player.

| Member | What it does |
|---|---|
| `play(): void` | Asks the room to play. |
| `pause(): void` | Pauses the room. |
| `seek(positionMs: number): void` | Moves the room to a position in the app's own file. |
| `setRate(rate: number): void` | Sets the room's rate, from 0.5 to 2.0. |
| `setOffset(offsetMs: number): void` | Sets your local offset from the room's position. |

The offset you set with `setOffset` applies only to your own player. It is an integer of at most 604800000 ms, or 7 days, either way.

### Host controls

Only the host may use these.

| Member | What it does |
|---|---|
| `setMedia(media: Media, ref: T, positionMs?: number): void` | Shares a title, with `ref` for your own file of it. |
| `clearMedia(): void` | Stops the media. |
| `setControl(mode: Control): void` | Sets who may play, pause, seek and set the rate. |
| `transferHost(peerId: string): void` | Makes another participant the host. |
| `kick(peerId: string): void` | Removes a participant from the room. |
| `lock(locked: boolean): void` | Refuses or allows new joins. |
| `end(): void` | Ends the room for everyone. |

### Party state

`party.state` is a new frozen `PartyState` object on every change.

| `PartyState<T>` field | What it holds |
|---|---|
| `connection` | The session status. |
| `peerId` | Your own peer ID. |
| `room` | The last snapshot of the room. |
| `media` | The current descriptor. |
| `match` | How the room's media matched the user's files. |
| `choice` | The file to open. |
| `mismatch` | The file offered while the user decides. |
| `candidates` | The ranked files. |
| `offsetMs` | Your local offset. |
| `player` | The player state reported to the room. |
| `endReason` | Why the party ended. |
| `me` | Your own entry among the participants. |
| `isHost` | Whether you are the host. |
| `canControl` | Whether you may control playback. |

### Events

Events tell you what happens in the party. Each has a `type`:

| `type` | Carries | When it comes |
|---|---|---|
| `action` | `kind`, `byPeerId`, `by`, `byMe`, `room` | A room change carried an action, such as someone pressing play. |
| `participantJoined` | `participant` | Someone joined the room. |
| `participantLeft` | `participant` | Someone left the room. |
| `error` | `error: PartyError` | The relay refused a command. |
| `resolveFailed` | `error` | The resolver failed. |
| `playBlocked` | `error` | The player refused to start. |
| `ended` | `reason`, `error` | The party ended. |

A `resolveFailed` event carries what the resolver threw. After 30 seconds without an answer, it carries a `DOMException` named `TimeoutError` instead.

A later 1.x may add types, so ignore the ones you don't know.

### Media descriptors

A `Media` value is a media descriptor: it names a title, and each participant finds their own copy of it. It holds `kind`, `title` and `durationMs`. It can also hold `year`, `ids`, `series`, `season`, `episode`, `file` and `ext`. The keys of `ext` are reverse-DNS names, such as `com.example`.

## Player and resolver

Your app gives the party two pieces of its own: a player adapter for your player, and a resolver for your library.

### Video and audio elements

For a `<video>` or `<audio>` element, use `attachVideo(party, video, ref)` instead of writing an adapter. Call it once per file, once the element has the file open. It does three things:

1. It wraps the element in an adapter.
2. It calls `party.attachPlayer(adapter, ref)`, with the `ref` of the file the element has open.
3. It routes the element's own controls to the party, and tells the user's presses apart from the SDK's commands.

A rate the SDK set on the element is never sent back to the room. While the user drags the seek bar, `attachVideo` sends at most one `seek` per 250 milliseconds, with the latest position. The position where the drag stops goes out within 250 milliseconds.

`attachVideo` returns a cleanup function, `() => Promise<void>`. The cleanup removes the listeners and calls `party.detachPlayer(adapter)`, so it never detaches a newer player. It returns that call's completion: await it before you dispose of the element. After `close()`, it resolves at once.

`videoAdapter(video)` is the adapter alone, with no control routing. Use it on pages your app doesn't own, such as a browser extension's. The adapters of both functions throw from `play` when the element refuses to start.

### The player adapter

For any other player, implement `PlayerAdapter`. Its reads must be cheap and synchronous.

| Member | What it is |
|---|---|
| `positionMs` | The current position in your own file. |
| `durationMs` | The file's duration, or `null` when unknown. |
| `paused` | Whether the player is paused. |
| `buffering` | Whether the player stalled while trying to play. |
| `firstFrameShown` | Whether the file is open and shows a frame. |
| `play()` | Starts playing, and resolves once the player plays. |
| `pause()` | Pauses, and resolves once the player has paused. |
| `seek(positionMs)` | Seeks, and resolves once the seek has landed. |
| `setRate(rate)` | Optional. Changes the speed. |
| `seekAccuracyMs` | Optional. How far from the target a seek may land, such as your player's keyframe distance. |

A command must finish or throw. Without `setRate`, the SDK keeps your player in step with seeks alone.

When the player refuses to start, `play` throws. The common case is a browser blocking autoplay. The party then reports `loading` and emits a `playBlocked` event carrying what `play` threw. Your app then asks the user to press play on the player itself.

### Attaching and detaching

Call `attachPlayer(player, ref)` each time you open a file. Call it again after the party ends with an `ended` event, because the end releases the player.

As host, you can call `setMedia` and attach in either order. If a `setMedia` is lost to a dropped connection, the SDK sends it again after the resume, as long as the party is still host.

Call `detachPlayer` when the user leaves the player or the app goes to the background. Await it, or `close()`, before you dispose of a player.

When your player's state changes between ticks, call `playerChanged()` so the SDK reads it at once.

While the user is away, the quick start's `openChoice()` must not load a file or attach the element, because attaching would bring the user back. Add a flag that it checks before it loads a file and again before it attaches:

```ts
let away = false;

async function leavePlayer(): Promise<void> {
  away = true;
  await detach?.();
}

function returnToPlayer(): void {
  away = false;
  detach = attached = null;
  void openChoice();
}
```

### The resolver

The resolver finds the user's own copy of the room's title. A `Resolver<T>` has one method, `resolve(media, signal)`, which returns `Candidate<T>[]` best first.

A candidate is `{ id, ref, durationMs?, sizeBytes?, oshash?, name?, probe? }`. Its `probe(signal)` opens the file to learn its duration.

- Deciding which files are the same title is the resolver's job, for every kind of media. The SDK ranks the candidates and never drops one.
- Treat every value in the descriptor as an untrusted search term. Never use one as a path, URL, host or query fragment.

### Helpers

| Helper | What it does |
|---|---|
| `formatCode` | Formats a room code for display, such as `K7M-Q2X`. |
| `sameTitle` | Tells whether two descriptors name the same title. |
| `checkMedia` | Checks a descriptor before you send it, and returns the first broken rule or `null`. |
| `parseMedia` | Decodes a descriptor and applies the checks a receiver makes. |

## Threading

Listeners run synchronously. `on("state", …)` and `on("event", …)` replay nothing, so a listener gets only what happens after it subscribes.

## Errors

The SDK reports failures as a `PartyError`. Show the user your own text for each `code`, because `message` is meant for logs only.

| Field | What it holds |
|---|---|
| `code` | The error code. |
| `message` | Text for logs only. |
| `re` | The type of the refused message. |

The relay's error codes pass through as they are. The SDK adds three of its own: `invalid_code`, `unreachable` and `cancelled`.

Errors reach your app in different ways:

- `create` and `join` reject with a `PartyError`.
- Commands throw it synchronously.
- The relay's refusals of commands arrive as `error` events.
- The loss of the session arrives as an `ended` event.

When no answer comes within 10 seconds, the call fails with `unreachable`. The code `invalid_message` also covers a bad `Relays.at` base, a malformed `joined` and bad `parseMedia` input.

## Build and test

Run these commands from `sdk/typescript` in a checkout:

```sh
npm ci
npm run typecheck
npm run test:unit
npm run test:integration
npm test
npm run build
```

`test:unit` runs the unit, [vector](../../conformance/vectors/README.md) and embedded-list tests, and needs no Go. `test:integration` and `npm test` build `owp-relay` from `relay/` with Go 1.26.

## Links

- [`docs/integration.md`](../../docs/integration.md) is the end-to-end integration guide.
- [`spec/SPEC.md`](../../spec/SPEC.md) is the wire protocol.
- [`conformance/peer`](../../conformance/peer/README.md) is a scripted participant on this SDK, so an app under test has someone else in the room.
- The [Dart](../dart/README.md), [Kotlin](../kotlin/README.md) and [Swift](../swift/README.md) SDKs cover other platforms.

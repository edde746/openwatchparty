# Integration guide

This guide takes your player app from nothing to a working Open Watch Party (OWP) room. It covers the TypeScript, Dart, Kotlin and Swift SDKs, and most steps show the code in all four languages.

Your app writes three parts: a player adapter that lets the SDK drive your player, a resolver that finds the user's own copy of the media others share, and the UI. The rules behind every step are in [the SDK spec](../spec/SDK.md).

Install the SDK as the README for your language describes: [TypeScript](../sdk/typescript/README.md), [Dart](../sdk/dart/README.md), [Kotlin](../sdk/kotlin/README.md) or [Swift](../sdk/swift/README.md). A complete small app is in [the web example](../examples/web/README.md).

Steps 1 to 4 set up every party. Steps 5 and 6 are the host's flow and the guest's flow. Steps 7 to 11 cover controls, leaving, events and errors, relays, and the public relay.

## How a room works

The relay runs the room. It holds the room's phase, position and rate, which participant is the host, and the room's settings. The phase is one of `idle`, `paused`, `waiting` or `playing`.

Each client plays its own copy of the media. Room positions are in the timeline of the host's reference file, which is the file whose duration the shared media descriptor carries.

## 1. Create the party

The party is the SDK object your app talks to. Its constructor takes four things:

- A `Relays` object says which relays to use, as step 10 describes. Create one per app and pass it to every party.
- A true `client` name gives your app's published name and version, as step 11 describes.
- A `name` is the user's display name.
- Your resolver from step 3 matches the media that other participants share.

If `client` or `name` is bad, the constructor throws `invalid_message`.

Subscribe before you call `create` or `join`, because the party replays nothing. Then `create` opens a new room and returns its room code, which `formatCode` formats for display. To enter an existing room, call `join(code)` instead.

TypeScript:

```ts
import { Relays, WatchParty, formatCode } from "openwatchparty";

const relays = Relays.public(); // one per app, passed to every party

const party = new WatchParty<LibraryItem>({
  relays,
  client: "Example/1.0", // your app's published name and version
  name: "Anna", // the user's display name
  resolver: libraryResolver(library), // step 3; `library` is your app's library
});
party.on("state", render); // subscribe before create or join: nothing is replayed
party.on("event", onEvent);

const code = await party.create(); // or: await party.join(typedCode)
showCode(formatCode(code)); // "K7M-Q2X"
```

Dart:

```dart
final relays = Relays.public(); // one per app, passed to every party

final party = WatchParty<LibraryItem>(
  relays: relays,
  client: 'Example/1.0', // your app's published name and version
  name: 'Anna', // the user's display name
  resolver: LibraryResolver(library),
);
party.states.listen(render); // subscribe before create or join: nothing is replayed
party.events.listen(onEvent);

final code = await party.create(); // or: await party.join(typedCode)
showCode(formatCode(code)); // "K7M-Q2X"
```

Kotlin:

```kotlin
val http = HttpClient(OkHttp) { install(WebSockets) } // your Ktor client and engine
val relays = Relays.public(http) // one per app, passed to every party

val party = WatchParty(
    relays = relays,
    client = "Example/1.0", // your app's published name and version
    name = "Anna", // the user's display name
    resolver = LibraryResolver(library),
    scope = mainScope, // single-threaded; on Android a Main scope
)
mainScope.launch { party.state.collect { render(it) } }
mainScope.launch { party.events.collect { onEvent(it) } } // events are not replayed

val code = party.create() // or: party.join(typedCode)
showCode(formatCode(code)) // "K7M-Q2X"
```

Swift:

```swift
import OpenWatchParty

let relays = Relays.public() // one per app, passed to every party

let party = try WatchParty<LibraryItem>(
    relays: relays,
    client: "Example/1.0", // your app's published name and version
    name: "Anna", // the user's display name
    resolver: libraryResolver(library) // step 3; `library` is your app's library
)
// Subscribe before create or join: nothing is replayed. Each call is a stream of its own that gets every change.
let states = party.states()
let events = party.events()
Task { for await state in states { render(state) } }
Task { for await event in events { onEvent(event) } }

let code = try await party.create() // or: try await party.join(typedCode)
try showCode(formatCode(code)) // "K7M-Q2X"
```

### Kotlin and Swift

In Kotlin, the party calls your adapter and resolver on the scope you pass. The sample's scope is single-threaded, which on Android means a Main scope.

In Swift, the party is `@MainActor`, so it calls your adapter and resolver on the main actor. `states()` and `events()` each return a new `AsyncStream` that gets every change. The constructor and `formatCode` throw, so you call them with `try`.

The SDK spec has the details on the [relay list](../spec/SDK.md#3-relay-list) and the [per-language idioms](../spec/SDK.md#168-per-language-idioms).

### Room codes

`join` takes the code as the user typed it. It removes separators and upper-cases the rest, so that it can route the code by its first symbol. If the result isn't 6 symbols from `23456789ABCDEFGHJKMNPQRSTUVWXYZ`, the call fails with `invalid_code` before any network use.

The relay applies the same normalization to the `join` and `resume` codes it receives, as [room codes](../spec/SPEC.md#51-room-codes) describes. This matters only to apps that speak the wire protocol directly.

### Coming back to a code

A room ends when its last participant leaves, when the host ends it, or when it reaches its maximum age. Its code then names no room, and `join` fails with `room_not_found`. To bring a group back to a code it used before, such as one from your app's recent rooms, join with `create`. The call joins the room when it exists. Otherwise it makes a new room with that code, and the user is its host.

```ts
await party.join(code, { create: true });
```

In Kotlin the call is `party.join(code, create = true)`, in Dart `party.join(code, create: true)` and in Swift `try await party.join(code, create: true)`.

Keep these points in mind:

- Whoever arrives first makes the room and is its host. The host can hand the room over with `transferHost`.
- A code carries no ownership. While no room has it, anyone who knows it can make a room with it.
- A relay that predates `create` answers `room_not_found`. Offer to create a new room then.

The rules are in [create and join](../spec/SDK.md#72-create-and-join) and [the relay's `join`](../spec/SPEC.md#72-join).

## 2. Write the player adapter

The player adapter is how the SDK reads your player's state and sends it commands.

Each kind of member has one rule:

- Reads, such as `positionMs` and `paused`, must be cheap and synchronous.
- Commands, such as `play` and `seek`, must either finish or throw.

Two optional members tune how the SDK follows the room:

- `setRate` changes the player's speed. Without it, the SDK only seeks. In Dart, Kotlin and Swift, also return `true` from `canSetRate`.
- `seekAccuracyMs` says how far from its target a seek may land, and defaults to 0. For a player that seeks only to keyframes, use the keyframe distance. For one that can't seek at all, use a very large value.

### When the player refuses to start

When the player refuses to start, `play` must throw. While the player then stays paused, the SDK reports it as `loading` and emits a play-blocked event, which step 9 handles. The event is `playBlocked` in TypeScript, `PlayBlockedEvent` in Dart and Swift, and `PartyEvent.PlayBlocked` in Kotlin.

If `play` swallows the refusal instead, the SDK reports the player as `ready` while it sits paused. The rules are in [player state](../spec/SDK.md#102-player-state).

### The adapter in code

The TypeScript sample also calls `playerChanged` when the player changes. That is optional, but it gets changes to the room sooner.

TypeScript:

```ts
import type { PlayerAdapter } from "openwatchparty";

const adapter: PlayerAdapter = {
  get positionMs() { return player.positionMs; }, // in your own file
  get durationMs() { return player.durationMs; }, // null when unknown
  get paused() { return player.paused; },
  get buffering() { return player.stalled; }, // stalled while trying to play
  get firstFrameShown() { return player.frameShown; },
  play: () => player.play(),
  pause: () => player.pause(),
  seek: (positionMs) => player.seek(positionMs), // resolves once the seek has landed
  setRate: (rate) => player.setSpeed(rate), // optional: without it the SDK only seeks
};
player.onChange(() => party.playerChanged()); // optional: changes reach the room sooner
```

Dart:

```dart
final class ExampleAdapter extends PlayerAdapter {
  ExampleAdapter(this.player);

  final ExamplePlayer player;

  @override
  int get positionMs => player.positionMs; // in your own file
  @override
  int? get durationMs => player.durationMs; // null when unknown
  @override
  bool get paused => player.paused;
  @override
  bool get buffering => player.stalled; // stalled while trying to play
  @override
  bool get firstFrameShown => player.frameShown;
  @override
  bool get canSetRate => true; // optional, with setRate: without it the SDK only seeks

  @override
  Future<void> play() => player.play();
  @override
  Future<void> pause() => player.pause();
  @override
  Future<void> seek(int positionMs) => player.seek(positionMs); // completes once the seek has landed
  @override
  Future<void> setRate(double rate) => player.setSpeed(rate);
}
```

Kotlin:

```kotlin
class ExampleAdapter(private val player: ExamplePlayer) : PlayerAdapter {
    override val positionMs: Long get() = player.positionMs // in your own file
    override val durationMs: Long? get() = player.durationMs // null when unknown
    override val paused: Boolean get() = player.paused
    override val buffering: Boolean get() = player.stalled // stalled while trying to play
    override val firstFrameShown: Boolean get() = player.frameShown
    override val canSetRate: Boolean get() = true // optional, with setRate: without it the SDK only seeks

    override suspend fun play() = player.play()
    override suspend fun pause() = player.pause()
    override suspend fun seek(positionMs: Long) = player.seek(positionMs) // returns once the seek has landed
    override suspend fun setRate(rate: Double) = player.setSpeed(rate)
}
```

Swift:

```swift
@MainActor
final class ExampleAdapter: PlayerAdapter {
    private let player: ExamplePlayer

    init(_ player: ExamplePlayer) { self.player = player }

    var positionMs: Int64 { player.positionMs } // in your own file
    var durationMs: Int64? { player.durationMs } // nil when unknown
    var paused: Bool { player.paused }
    var buffering: Bool { player.stalled } // stalled while trying to play
    var firstFrameShown: Bool { player.frameShown }
    var canSetRate: Bool { true } // optional, with setRate: without it the SDK only seeks

    func play() async throws { try await player.play() } // throws when the player refuses to start
    func pause() async throws { try await player.pause() }
    func seek(to positionMs: Int64) async throws { try await player.seek(positionMs) } // returns once the seek has landed
    func setRate(_ rate: Double) async throws { try await player.setSpeed(rate) }
}
```

### On the web

On the web, `attachVideo(party, video, ref)` wraps an `HTMLMediaElement` and routes the element's controls to the party. Call it once the element has the chosen file open, as in step 4. It returns a detach function, which you await before you dispose of the element.

For pages your app doesn't own, `videoAdapter(video)` makes the adapter alone.

```ts
import { attachVideo } from "openwatchparty";

const video = document.querySelector("video")!;
const detach = attachVideo(party, video, party.state.choice!.ref); // once the element has the chosen file open (step 4)
// later: await detach(); // before disposing of the element
```

### Players controlled over IPC

If your app controls the player over IPC, the adapter still answers reads without waiting:

- Reads return values cached from the player's property-change events. They never wait on the socket.
- Commands await the player's reply. A reply can arrive before its property event, so set the cached `paused` after the reply.
- A seek's reply also arrives before the seek lands. So `seek` waits for the report that it landed, under a timeout.
- A refused command rejects, so `play` throws as described above.
- Call `party.playerChanged()` on any change other than position.

### Browser extensions

A browser extension runs on pages your app doesn't own, so it uses `videoAdapter`:

- Put the adapter on the page's main `<video>` element. The largest one on screen is a workable rule.
- Use a string key for the page's title as the ref, so that refs compare equal across reads.
- On navigation, the page may replace the element. Release the old adapter with `detachPlayer(adapter)`, and attach a new one under the same ref.
- Release the adapter while the same element plays an ad, which the site's markers show. Otherwise the SDK plays whatever the element shows.
- With no resolver, the match stays `unavailable` until the user opens the title and your app calls `choose` with the element's duration. Attach only while the page's key is the chosen ref.
- `videoAdapter` doesn't call `playerChanged()`, so call it from capturing media-event listeners on `document`.

## 3. Write the resolver

The resolver finds the user's own copy of the room's media. The SDK runs it once per new media, passing the room's media descriptor. It returns candidates: the files in your library that are this title, each with its duration or a probe that opens the file to learn it.

### What the SDK expects

- The resolver may also return near misses for the choose list. A near miss has no duration and no probe, so the SDK never chooses it automatically.
- The SDK cancels the resolver when the media changes or the party ends.
- A throw, or a call longer than 30 seconds, counts as no candidates, and the SDK emits `ResolveFailed`.
- When your app finds more files, call `resolveAgain()`. Without a choice it searches again, and with one it only replaces `state.candidates`.

### Deciding what is the same title

Deciding which files are this title is the resolver's job. The SDK only ranks the candidates and compares their durations.

`sameTitle(a, b)` helps you decide. It compares two descriptors by their IDs or their file details:

| Kind | Same title when |
|---|---|
| Movie | The two share a catalogue ID. |
| Episode | The two share an own ID, or share a series ID and have equal season and episode numbers. |
| Video | The two have the same file hash, or the same size and name. |

The episode number match has one exception. If some namespace holds a different own ID on each side, the numbers don't count. An episode with no own ID can still match on numbers.

`sameTitle` never reads titles, so a movie or episode without IDs matches nothing through it alone. The full rule is in [same title](../spec/SDK.md#93-same-title).

### How the samples resolve

The samples work over stand-ins for your library: `LibraryItem`, `Show` and `Library`. They resolve in two steps:

1. Step 1 returns the files that share a catalogue ID with the media or its series, whatever their titles. `sameTitle` checks each one.
2. Step 2 runs only when step 1 found no ID match. From a title search, it takes movies with the same folded title and a year at most one apart, and episodes with the same show, season and episode. It drops any item with a conflicting ID.

The title search always runs, because it also collects the near misses. It looks for an episode by its show's title, never by its own.

Step 2 uses the search results only without an ID match, because ranking prefers durations over resolver order. Next to ID matches, a copy that matches only by title could match first.

A title filed under a different name, without a shared ID, is beyond both steps. For those titles, your app's own search feeds `choose`.

The samples also define three helpers that later steps use:

| Helper | What it does |
|---|---|
| `describe` | Builds the descriptor your app sends as host in step 5. |
| `playable` | Turns a library item into a candidate the SDK can match. |
| `chooseList` | Lists the candidates for the user to choose from in step 6. |

TypeScript:

```ts
import { sameTitle, type Candidate, type Media, type Resolver, type WatchParty } from "openwatchparty";

// Stand-ins for the app's own library.
export interface Show {
  title: string;
  year?: number;
  ids: Record<string, string>; // the catalogue IDs the library knows; often none
}

export interface LibraryItem {
  id: string;
  kind: "movie" | "episode" | "video";
  title: string; // the movie's, episode's or video's title; an episode's may be empty
  year?: number;
  ids: Record<string, string>; // for an episode, its own IDs
  show?: Show; // episodes only
  season?: number;
  episode?: number;
  durationMs?: number; // when the library knows it
  sizeBytes?: number;
  fileName?: string;
}

export interface Library {
  /** Items holding any of these catalogue IDs, as their own or their show's, whatever their titles. */
  findByIds(ids: Readonly<Record<string, string>>, signal: AbortSignal): Promise<LibraryItem[]>;
  /** The library's own text search over movie, show and video titles. */
  search(text: string, signal: AbortSignal): Promise<LibraryItem[]>;
  /** Opens the file, reads its duration and closes it; null when it doesn't open. */
  probeDuration(item: LibraryItem, signal: AbortSignal): Promise<number | null>;
}

const NEAR_MISSES = 10;

/** The descriptor this app sends for `item` as host. `title` is required, so an untitled episode gets "Episode N". */
export function describe(item: LibraryItem, durationMs: number): Media {
  const title =
    item.title.trim() !== "" ? item.title : item.kind === "episode" && item.episode !== undefined ? `Episode ${item.episode}` : (item.fileName ?? "Untitled");
  return {
    kind: item.kind,
    title,
    year: item.year,
    ids: item.ids,
    series: item.show && { title: item.show.title, year: item.show.year, ids: item.show.ids },
    season: item.season,
    episode: item.episode,
    durationMs,
    file: { sizeBytes: item.sizeBytes, name: item.fileName },
  };
}

/** Lower case, with every run of characters other than letters and digits as one space. */
function fold(s: string): string {
  return s.toLowerCase().replace(/[^\p{L}\p{N}]+/gu, " ").trim();
}

function closeYears(a: number | undefined, b: number | undefined): boolean {
  return a === undefined || b === undefined || Math.abs(a - b) <= 1;
}

/** Some namespace holds a different ID on each side: two different titles. */
function conflicts(a: Readonly<Record<string, string>> | undefined, b: Readonly<Record<string, string>>): boolean {
  return Object.entries(a ?? {}).some(([ns, id]) => b[ns] !== undefined && b[ns] !== id);
}

/** Step 2: the same title and year, or the same show, season and episode, with no ID saying otherwise. */
function sameName(media: Media, item: LibraryItem): boolean {
  if (item.kind !== media.kind) return false;
  switch (media.kind) {
    case "movie":
      return fold(item.title) === fold(media.title) && closeYears(media.year, item.year) && !conflicts(media.ids, item.ids);
    case "episode":
      return (
        item.show !== undefined &&
        media.series !== undefined &&
        fold(item.show.title) === fold(media.series.title) &&
        closeYears(media.series.year, item.show.year) &&
        media.season !== undefined &&
        media.season === item.season &&
        media.episode !== undefined &&
        media.episode === item.episode &&
        !conflicts(media.ids, item.ids)
      );
    case "video":
      return (
        fold(item.title) === fold(media.title) ||
        (media.file?.name !== undefined && item.fileName?.toLowerCase() === media.file.name.toLowerCase())
      );
  }
}

/** A candidate the walk may match: its duration, or a probe that learns it. */
export function playable(library: Library, item: LibraryItem): Candidate<LibraryItem> {
  return {
    id: item.id,
    ref: item,
    durationMs: item.durationMs,
    probe: (signal) => library.probeDuration(item, signal),
    sizeBytes: item.sizeBytes,
    name: item.fileName,
  };
}

/** The user's list of files to choose from: the resolver's candidates, each with its difference once known. */
export function chooseList(party: WatchParty<LibraryItem>, library: Library, show: (label: string, onPick: () => void) => void): void {
  for (const { candidate, deltaMs } of party.state.candidates) {
    const item = candidate.ref;
    const name =
      item.title.trim() !== ""
        ? item.season !== undefined && item.episode !== undefined
          ? `${item.title} S${item.season}E${item.episode}`
          : item.title
        : (item.fileName ?? "Untitled");
    const label = deltaMs === null ? name : `${name} (${Math.round(deltaMs / 1000)} s)`;
    // choose takes a playable candidate, rebuilt from the ref: near misses carry no duration and no probe.
    show(label, () => void party.choose(playable(library, item)));
  }
}

export function libraryResolver(library: Library): Resolver<LibraryItem> {
  return {
    async resolve(media, signal) {
      const out = new Map<string, Candidate<LibraryItem>>(); // by item ID, first find wins, in order
      // Step 1: files sharing a catalogue ID with the media, whatever their titles. sameTitle reads no durationMs.
      const byId = [...(await library.findByIds(media.ids ?? {}, signal)), ...(await library.findByIds(media.series?.ids ?? {}, signal))];
      for (const item of byId) {
        if (!out.has(item.id) && sameTitle(media, describe(item, media.durationMs))) out.set(item.id, playable(library, item));
      }
      // Step 2, only without an ID match: an episode is searched by its show's title, never its own. Durations decide.
      const text = media.kind === "episode" ? (media.series?.title ?? media.title) : media.title;
      const found = await library.search(text, signal); // a search term only, never a path or a query fragment
      const byName = out.size === 0 ? found.filter((item) => sameName(media, item)) : [];
      for (const item of byName) if (!out.has(item.id)) out.set(item.id, playable(library, item));
      // Near misses keep the choose list from being empty. Without a duration and a probe the walk never picks them.
      for (const item of found.filter((i) => i.kind === media.kind).slice(0, NEAR_MISSES)) {
        if (!out.has(item.id)) out.set(item.id, { id: item.id, ref: item, sizeBytes: item.sizeBytes, name: item.fileName });
      }
      return [...out.values()];
    },
  };
}
```

Dart:

```dart
import 'dart:async';

import 'package:openwatchparty/openwatchparty.dart';

// Stand-ins for the app's own library.
final class Show {
  const Show({required this.title, this.year, this.ids = const {}});

  final String title;
  final int? year;
  final Map<String, String> ids; // the catalogue IDs the library knows; often none
}

final class LibraryItem {
  const LibraryItem({
    required this.id,
    required this.kind,
    required this.title,
    this.year,
    this.ids = const {},
    this.show,
    this.season,
    this.episode,
    this.durationMs,
    this.sizeBytes,
    this.fileName,
  });

  final String id;
  final MediaKind kind;
  final String title; // the movie's, episode's or video's title; an episode's may be empty
  final int? year;
  final Map<String, String> ids; // for an episode, its own IDs
  final Show? show; // episodes only
  final int? season;
  final int? episode;
  final int? durationMs; // when the library knows it
  final int? sizeBytes;
  final String? fileName;
}

abstract interface class Library {
  /// Items holding any of these catalogue IDs, as their own or their show's, whatever their titles.
  Future<List<LibraryItem>> findByIds(Map<String, String> ids, Cancellation cancel);

  /// The library's own text search over movie, show and video titles.
  Future<List<LibraryItem>> search(String text, Cancellation cancel);

  /// Opens the file, reads its duration and closes it; null when it doesn't open.
  Future<int?> probeDuration(LibraryItem item, Cancellation cancel);
}

const nearMisses = 10;

/// The descriptor this app sends for [item] as host. `title` is required, so an untitled episode gets "Episode N".
Media describe(LibraryItem item, int durationMs) {
  final show = item.show;
  final title = item.title.trim().isNotEmpty
      ? item.title
      : item.kind == MediaKind.episode && item.episode != null
          ? 'Episode ${item.episode}'
          : item.fileName ?? 'Untitled';
  return Media(
    kind: item.kind,
    title: title,
    year: item.year,
    ids: item.ids,
    series: show == null ? null : Series(title: show.title, year: show.year, ids: show.ids),
    season: item.season,
    episode: item.episode,
    durationMs: durationMs,
    file: MediaFile(sizeBytes: item.sizeBytes, name: item.fileName),
  );
}

final _separators = RegExp(r'[^\p{L}\p{N}]+', unicode: true);

/// Lower case, with every run of characters other than letters and digits as one space.
String _fold(String s) => s.toLowerCase().replaceAll(_separators, ' ').trim();

bool _closeYears(int? a, int? b) => a == null || b == null || (a - b).abs() <= 1;

/// Some namespace holds a different ID on each side: two different titles.
bool _conflicts(Map<String, String> a, Map<String, String> b) =>
    a.entries.any((e) => b[e.key] != null && b[e.key] != e.value);

/// Step 2: the same title and year, or the same show, season and episode, with no ID saying otherwise.
bool _sameName(Media media, LibraryItem item) {
  if (item.kind != media.kind) return false;
  switch (media.kind) {
    case MediaKind.movie:
      return _fold(item.title) == _fold(media.title) &&
          _closeYears(media.year, item.year) &&
          !_conflicts(media.ids, item.ids);
    case MediaKind.episode:
      final show = item.show, series = media.series;
      return show != null &&
          series != null &&
          _fold(show.title) == _fold(series.title) &&
          _closeYears(series.year, show.year) &&
          media.season != null &&
          media.season == item.season &&
          media.episode != null &&
          media.episode == item.episode &&
          !_conflicts(media.ids, item.ids);
    case MediaKind.video:
      final name = media.file?.name;
      return _fold(item.title) == _fold(media.title) ||
          (name != null && item.fileName?.toLowerCase() == name.toLowerCase());
  }
}

/// A candidate the walk may match: its duration, or a probe that learns it.
Candidate<LibraryItem> playable(Library library, LibraryItem item) => Candidate(
      id: item.id,
      ref: item,
      durationMs: item.durationMs,
      probe: (cancel) => library.probeDuration(item, cancel),
      sizeBytes: item.sizeBytes,
      name: item.fileName,
    );

final class LibraryResolver implements Resolver<LibraryItem> {
  LibraryResolver(this.library);

  final Library library;

  @override
  Future<List<Candidate<LibraryItem>>> resolve(Media media, Cancellation cancel) async {
    final out = <String, Candidate<LibraryItem>>{}; // by item ID, first find wins, in order
    // Step 1: files sharing a catalogue ID with the media, whatever their titles. sameTitle reads no durationMs.
    final byId = [
      ...await library.findByIds(media.ids, cancel),
      ...await library.findByIds(media.series?.ids ?? const {}, cancel),
    ];
    for (final item in byId) {
      if (!out.containsKey(item.id) && sameTitle(media, describe(item, media.durationMs))) {
        out[item.id] = playable(library, item);
      }
    }
    // Step 2, only without an ID match: an episode is searched by its show's title, never its own. Durations decide.
    final text = media.kind == MediaKind.episode ? media.series?.title ?? media.title : media.title;
    final found = await library.search(text, cancel); // a search term only, never a path or a query fragment
    final byName = out.isEmpty ? found.where((item) => _sameName(media, item)).toList() : const <LibraryItem>[];
    for (final item in byName) {
      out.putIfAbsent(item.id, () => playable(library, item));
    }
    // Near misses keep the choose list from being empty. Without a duration and a probe the walk never picks them.
    for (final item in found.where((i) => i.kind == media.kind).take(nearMisses)) {
      out.putIfAbsent(item.id, () => Candidate(id: item.id, ref: item, sizeBytes: item.sizeBytes, name: item.fileName));
    }
    return out.values.toList();
  }
}

/// The user's list of files to choose from: the resolver's candidates, each with its difference once known.
void chooseList(
  WatchParty<LibraryItem> party,
  Library library,
  void Function(String label, void Function() onPick) show,
) {
  for (final ranked in party.state.candidates) {
    final item = ranked.candidate.ref;
    final delta = ranked.deltaMs;
    final name = item.title.trim().isNotEmpty
        ? (item.season != null && item.episode != null ? '${item.title} S${item.season}E${item.episode}' : item.title)
        : item.fileName ?? 'Untitled';
    final label = delta == null ? name : '$name (${(delta / 1000).round()} s)';
    // choose takes a playable candidate, rebuilt from the ref: near misses carry no duration and no probe.
    show(label, () => unawaited(party.choose(playable(library, item))));
  }
}
```

Kotlin:

```kotlin
package example

import kotlin.math.abs
import kotlin.math.roundToLong
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.launch
import party.openwatch.Candidate
import party.openwatch.Media
import party.openwatch.MediaFile
import party.openwatch.MediaKind
import party.openwatch.Resolver
import party.openwatch.Series
import party.openwatch.WatchParty
import party.openwatch.sameTitle

// Stand-ins for the app's own library.
data class Show(
    val title: String,
    val year: Long? = null,
    val ids: Map<String, String> = emptyMap(), // the catalogue IDs the library knows; often none
)

data class LibraryItem(
    val id: String,
    val kind: MediaKind,
    val title: String, // the movie's, episode's or video's title; an episode's may be empty
    val year: Long? = null,
    val ids: Map<String, String> = emptyMap(), // for an episode, its own IDs
    val show: Show? = null, // episodes only
    val season: Long? = null,
    val episode: Long? = null,
    val durationMs: Long? = null, // when the library knows it
    val sizeBytes: Long? = null,
    val fileName: String? = null,
)

interface Library {
    /** Items holding any of these catalogue IDs, as their own or their show's, whatever their titles. */
    suspend fun findByIds(ids: Map<String, String>): List<LibraryItem>

    /** The library's own text search over movie, show and video titles. */
    suspend fun search(text: String): List<LibraryItem>

    /** Opens the file, reads its duration and closes it; null when it doesn't open. */
    suspend fun probeDuration(item: LibraryItem): Long?
}

const val NEAR_MISSES = 10

/** The descriptor this app sends for [item] as host. `title` is required, so an untitled episode gets "Episode N". */
fun describe(item: LibraryItem, durationMs: Long): Media = Media(
    kind = item.kind,
    title = when {
        item.title.isNotBlank() -> item.title
        item.kind == MediaKind.EPISODE && item.episode != null -> "Episode ${item.episode}"
        else -> item.fileName ?: "Untitled"
    },
    durationMs = durationMs,
    year = item.year,
    ids = item.ids,
    series = item.show?.let { Series(it.title, it.year, it.ids) },
    season = item.season,
    episode = item.episode,
    file = MediaFile(sizeBytes = item.sizeBytes, name = item.fileName),
)

private val separators = Regex("[^\\p{L}\\p{N}]+")

/** Lower case, with every run of characters other than letters and digits as one space. */
private fun fold(s: String) = s.lowercase().replace(separators, " ").trim()

private fun closeYears(a: Long?, b: Long?) = a == null || b == null || abs(a - b) <= 1

/** Some namespace holds a different ID on each side: two different titles. */
private fun conflicts(a: Map<String, String>, b: Map<String, String>) = a.any { (ns, id) -> b[ns] != null && b[ns] != id }

/** Step 2: the same title and year, or the same show, season and episode, with no ID saying otherwise. */
private fun sameName(media: Media, item: LibraryItem): Boolean {
    if (item.kind != media.kind) return false
    return when (media.kind) {
        MediaKind.MOVIE ->
            fold(item.title) == fold(media.title) && closeYears(media.year, item.year) && !conflicts(media.ids, item.ids)
        MediaKind.EPISODE -> {
            val show = item.show
            val series = media.series
            show != null && series != null &&
                fold(show.title) == fold(series.title) && closeYears(series.year, show.year) &&
                media.season != null && media.season == item.season &&
                media.episode != null && media.episode == item.episode &&
                !conflicts(media.ids, item.ids)
        }
        MediaKind.VIDEO ->
            fold(item.title) == fold(media.title) || media.file?.name?.equals(item.fileName, ignoreCase = true) == true
    }
}

/** A candidate the walk may match: its duration, or a probe that learns it. */
fun playable(library: Library, item: LibraryItem): Candidate<LibraryItem> = Candidate(
    id = item.id,
    ref = item,
    durationMs = item.durationMs,
    sizeBytes = item.sizeBytes,
    name = item.fileName,
    probe = { library.probeDuration(item) },
)

class LibraryResolver(private val library: Library) : Resolver<LibraryItem> {
    override suspend fun resolve(media: Media): List<Candidate<LibraryItem>> {
        val out = linkedMapOf<String, Candidate<LibraryItem>>() // by item ID, first find wins, in order
        // Step 1: files sharing a catalogue ID with the media, whatever their titles. sameTitle reads no durationMs.
        val byId = library.findByIds(media.ids) + library.findByIds(media.series?.ids.orEmpty())
        for (item in byId) {
            if (item.id !in out && sameTitle(media, describe(item, media.durationMs))) out[item.id] = playable(library, item)
        }
        // Step 2, only without an ID match: an episode is searched by its show's title, never its own. Durations decide.
        val text = if (media.kind == MediaKind.EPISODE) media.series?.title ?: media.title else media.title
        val found = library.search(text) // a search term only, never a path or a query fragment
        val byName = if (out.isEmpty()) found.filter { sameName(media, it) } else emptyList()
        for (item in byName) out.getOrPut(item.id) { playable(library, item) }
        // Near misses keep the choose list from being empty. Without a duration and a probe the walk never picks them.
        for (item in found.filter { it.kind == media.kind }.take(NEAR_MISSES)) {
            out.getOrPut(item.id) { Candidate(item.id, item, sizeBytes = item.sizeBytes, name = item.fileName) }
        }
        return out.values.toList()
    }
}

/** The user's list of files to choose from: the resolver's candidates, each with its difference once known. */
fun chooseList(
    party: WatchParty<LibraryItem>,
    library: Library,
    scope: CoroutineScope,
    show: (label: String, onPick: () -> Unit) -> Unit,
) {
    for ((candidate, deltaMs) in party.state.value.candidates) {
        val item = candidate.ref
        val name =
            if (item.title.isNotBlank()) {
                if (item.season != null && item.episode != null) {
                    "${item.title} S${item.season}E${item.episode}"
                } else {
                    item.title
                }
            } else {
                item.fileName ?: "Untitled"
            }
        val label = if (deltaMs == null) name else "$name (${(deltaMs / 1000.0).roundToLong()} s)"
        // choose takes a playable candidate, rebuilt from the ref: near misses carry no duration and no probe.
        show(label) { scope.launch { party.choose(playable(library, item)) } }
    }
}
```

Swift:

```swift
import OpenWatchParty

// Stand-ins for the app's own library.
struct Show: Hashable, Sendable {
    var title: String
    var year: Int64? = nil
    var ids: [String: String] = [:] // the catalogue IDs the library knows; often none
}

struct LibraryItem: Hashable, Sendable {
    var id: String
    var kind: MediaKind
    var title: String // the movie's, episode's or video's title; an episode's may be empty
    var year: Int64? = nil
    var ids: [String: String] = [:] // for an episode, its own IDs
    var show: Show? = nil // episodes only
    var season: Int64? = nil
    var episode: Int64? = nil
    var durationMs: Int64? = nil // when the library knows it
    var sizeBytes: Int64? = nil
    var fileName: String? = nil
}

@MainActor
protocol Library: AnyObject, Sendable {
    /// Items holding any of these catalogue IDs, as their own or their show's, whatever their titles.
    func findByIds(_ ids: [String: String]) async throws -> [LibraryItem]

    /// The library's own text search over movie, show and video titles.
    func search(_ text: String) async throws -> [LibraryItem]

    /// Opens the file, reads its duration and closes it; nil when it doesn't open.
    func probeDuration(_ item: LibraryItem) async throws -> Int64?
}

let nearMisses = 10

/// The descriptor this app sends for `item` as host. `title` is required, so an untitled episode gets "Episode N".
func describe(_ item: LibraryItem, durationMs: Int64) -> Media {
    let title: String
    if !item.title.allSatisfy(\.isWhitespace) {
        title = item.title
    } else if item.kind == .episode, let episode = item.episode {
        title = "Episode \(episode)"
    } else {
        title = item.fileName ?? "Untitled"
    }
    return Media(
        kind: item.kind,
        title: title,
        durationMs: durationMs,
        year: item.year,
        ids: item.ids,
        series: item.show.map { Series(title: $0.title, year: $0.year, ids: $0.ids) },
        season: item.season,
        episode: item.episode,
        file: MediaFile(sizeBytes: item.sizeBytes, name: item.fileName)
    )
}

/// Lower case, with every run of characters other than letters and digits as one space.
private func fold(_ s: String) -> String {
    s.lowercased().split(whereSeparator: { !$0.isLetter && !$0.isNumber }).joined(separator: " ")
}

private func closeYears(_ a: Int64?, _ b: Int64?) -> Bool {
    guard let a, let b else { return true }
    return abs(a - b) <= 1
}

/// Some namespace holds a different ID on each side: two different titles.
private func conflicts(_ a: [String: String], _ b: [String: String]) -> Bool {
    a.contains { ns, id in b[ns].map { $0 != id } ?? false }
}

/// Step 2: the same title and year, or the same show, season and episode, with no ID saying otherwise.
private func sameName(_ media: Media, _ item: LibraryItem) -> Bool {
    guard item.kind == media.kind else { return false }
    switch media.kind {
    case .movie:
        return fold(item.title) == fold(media.title) && closeYears(media.year, item.year) && !conflicts(media.ids, item.ids)
    case .episode:
        guard let show = item.show, let series = media.series, let season = media.season, let episode = media.episode else {
            return false
        }
        return fold(show.title) == fold(series.title) && closeYears(series.year, show.year)
            && season == item.season && episode == item.episode && !conflicts(media.ids, item.ids)
    case .video:
        return fold(item.title) == fold(media.title)
            || (media.file?.name).map { $0.lowercased() == item.fileName?.lowercased() } ?? false
    }
}

/// A candidate the walk may match: its duration, or a probe that learns it.
func playable(_ library: any Library, _ item: LibraryItem) -> Candidate<LibraryItem> {
    Candidate(
        id: item.id,
        ref: item,
        durationMs: item.durationMs,
        sizeBytes: item.sizeBytes,
        name: item.fileName,
        probe: { try await library.probeDuration(item) } // the SDK cancels the task when it no longer needs the answer
    )
}

func libraryResolver(_ library: any Library) -> Resolver<LibraryItem> {
    Resolver { media in
        var out: [Candidate<LibraryItem>] = [] // first find wins, in order
        var seen: Set<String> = [] // item IDs in `out`
        // Step 1: files sharing a catalogue ID with the media, whatever their titles. sameTitle reads no durationMs.
        let byId = try await library.findByIds(media.ids) + library.findByIds(media.series?.ids ?? [:])
        for item in byId where !seen.contains(item.id) && sameTitle(media, describe(item, durationMs: media.durationMs)) {
            seen.insert(item.id)
            out.append(playable(library, item))
        }
        // Step 2, only without an ID match: an episode is searched by its show's title, never its own. Durations decide.
        let text = media.kind == .episode ? media.series?.title ?? media.title : media.title
        let found = try await library.search(text) // a search term only, never a path or a query fragment
        let byName = out.isEmpty ? found.filter { sameName(media, $0) } : []
        for item in byName where seen.insert(item.id).inserted {
            out.append(playable(library, item))
        }
        // Near misses keep the choose list from being empty. Without a duration and a probe the walk never picks them.
        for item in found.filter({ $0.kind == media.kind }).prefix(nearMisses) where seen.insert(item.id).inserted {
            out.append(Candidate(id: item.id, ref: item, sizeBytes: item.sizeBytes, name: item.fileName))
        }
        return out
    }
}

/// The user's list of files to choose from: the resolver's candidates, each with its difference once known.
@MainActor
func chooseList(
    _ party: WatchParty<LibraryItem>,
    _ library: any Library,
    show: (_ label: String, _ onPick: @escaping @MainActor () -> Void) -> Void
) {
    for ranked in party.state.candidates {
        let item = ranked.candidate.ref
        let name: String
        if !item.title.allSatisfy(\.isWhitespace) {
            if let season = item.season, let episode = item.episode {
                name = "\(item.title) S\(season)E\(episode)"
            } else {
                name = item.title
            }
        } else {
            name = item.fileName ?? "Untitled"
        }
        let label = ranked.deltaMs.map { "\(name) (\(Int64((Double($0) / 1000).rounded())) s)" } ?? name
        // choose takes a playable candidate, rebuilt from the ref: near misses carry no duration and no probe.
        show(label) { Task { await party.choose(playable(library, item)) } }
    }
}
```

### Treat descriptors as untrusted

A descriptor comes from another participant through the relay, so treat it as untrusted input:

- Use its values as search terms only. Never use them as a path, URL, host, shell or SQL fragment.
- Never open `file.name`.
- Anyone can forge `ext`, so use it only for lookups in your own library.
- Show titles and names as plain text.

The rules are in [descriptors are untrusted](../spec/SDK.md#97-descriptors-are-untrusted).

## 4. Open `state.choice`

When the SDK settles on a file for this user, it publishes it as `state.choice`. Your app then opens that file and attaches the player to it.

Each time `state.choice` changes:

1. If the player is already attached with the choice's ref, stop.
2. Open `choice.ref`, unless the player already has it open.
3. Call `attachPlayer(player, ref)`.

Open one file at a time. If the choice changes while a file is opening, open the newer one instead.

The party's end releases the player, so clear your attached ref on `Ended`. In Kotlin, watch `events` for it, because `state` can skip the `ENDED` connection state.

The samples do this in `openChoice`. It also checks the `away` flag from step 8, so that nothing opens or attaches while the user is away.

TypeScript:

```ts
let attached: LibraryItem | null = null;
let away = false; // step 8
let opening = false;

async function openChoice(): Promise<void> {
  if (opening) return;
  opening = true;
  try {
    while (true) {
      const ref = party.state.choice?.ref;
      if (away || ref === undefined || ref === attached) return;
      if (player.item !== ref) await player.open(ref);
      if (away || party.state.choice?.ref !== ref) continue;
      party.attachPlayer(adapter, ref);
      attached = ref;
    }
  } finally {
    opening = false;
  }
}

party.on("state", () => void openChoice());
party.on("event", (event) => {
  if (event.type === "ended") attached = null;
});
```

Dart:

```dart
LibraryItem? attached;
var away = false; // step 8
var opening = false;

Future<void> openChoice() async {
  if (opening) return;
  opening = true;
  try {
    while (true) {
      final ref = party.state.choice?.ref;
      if (away || ref == null || ref == attached) return;
      if (player.item != ref) await player.open(ref);
      if (away || party.state.choice?.ref != ref) continue;
      party.attachPlayer(adapter, ref);
      attached = ref;
    }
  } finally {
    opening = false;
  }
}

party.states.listen((_) => openChoice());
party.events.listen((event) {
  if (event is EndedEvent) attached = null;
});
```

Kotlin:

```kotlin
var attached: LibraryItem? = null
var away = false // step 8
val opening = Mutex()

suspend fun openChoice() = opening.withLock {
    while (true) {
        val ref = party.state.value.choice?.ref ?: break
        if (away || ref == attached) break
        if (player.item != ref) player.open(ref)
        if (away || party.state.value.choice?.ref != ref) continue
        party.attachPlayer(adapter, ref)
        attached = ref
    }
}

mainScope.launch { party.state.collect { openChoice() } }
mainScope.launch { party.events.collect { if (it is PartyEvent.Ended) attached = null } }
```

Swift:

```swift
var attached: LibraryItem?
var away = false // step 8
var opening = false

@MainActor
func openChoice() async throws {
    if opening { return }
    opening = true
    defer { opening = false }
    while true {
        guard let ref = party.state.choice?.ref, !away, ref != attached else { return }
        if player.item != ref { try await player.open(ref) }
        if away || party.state.choice?.ref != ref { continue }
        party.attachPlayer(adapter, ref: ref)
        attached = ref
    }
}

let choices = party.states()
let endings = party.events()
Task { for await _ in choices { try? await openChoice() } }
Task { for await event in endings where event is EndedEvent { attached = nil } }
```

## 5. Host flow: share media

As host, your app shares the media that everyone watches. It describes the file in a media descriptor and sends it with `setMedia`.

### Sending the media

- Call `setMedia(media, ref)` for the file you will play. The optional start `positionMs` is a position in that file.
- Once the file is open, you can call `setMedia` and `attachPlayer` in either order.
- Both calls take the same `ref`. In TypeScript that means the same object. In Swift it means an equal value, because refs compare with `==`.
- `clearMedia()` stops the media for everyone.

### Building the descriptor

Build the descriptor from your library entry, as `describe` does in step 3. The required fields are `kind`, a non-empty `title` and `durationMs`, plus `series.title` for an episode. Everything else is optional.

Sparse metadata is fine. An untitled episode sends a stand-in title such as `Episode 5`, and you may omit empty `ids`.

Keep the real kind. `video` is only for a file with no catalogue identity, such as a home video or a clip. A movie or episode without IDs stays a `movie` or an `episode`.

ID values are non-empty strings, numbers included, as in `"tmdb": "1396"`. A series' `tmdb` ID is its TV ID, and a movie's is its movie ID. A `plex` ID is a `plex://` GUID.

V1 has no field for absolute episode numbers or alternate titles, so a series' anime-namespace IDs matter more than its numbers.

File hints are optional, and so is an `ext` member under your reverse-DNS name. Nothing in the descriptor may locate or fetch media. The full format is in [the media descriptor](../spec/SPEC.md#10-media-descriptor).

Before you send, check the descriptor with `checkMedia`. It returns null, or nil in Swift, when the relay accepts the descriptor, and otherwise the first rule it breaks.

TypeScript:

```ts
import { checkMedia, type Media } from "openwatchparty";

const media: Media = {
  kind: "episode",
  title: "Pilot",
  durationMs: 3487000,
  ids: { imdb: "tt0959621", tvdb: "349232" },
  series: { title: "Breaking Bad", year: 2008, ids: { imdb: "tt0903747", tmdb: "1396", tvdb: "81189" } },
  season: 1,
  episode: 1,
  ext: { "com.example.player": { item: "4711" } },
};
const problem = checkMedia(media); // null when the relay accepts it, else the first broken rule
if (problem === null) party.setMedia(media, item); // or setMedia(media, item, positionMs)
```

Dart:

```dart
final media = Media(
  kind: MediaKind.episode,
  title: 'Pilot',
  durationMs: 3487000,
  ids: {'imdb': 'tt0959621', 'tvdb': '349232'},
  series: Series(title: 'Breaking Bad', year: 2008, ids: {'imdb': 'tt0903747', 'tmdb': '1396', 'tvdb': '81189'}),
  season: 1,
  episode: 1,
  ext: {'com.example.player': {'item': '4711'}},
);
final problem = checkMedia(media); // null when the relay accepts it, else the first broken rule
if (problem == null) party.setMedia(media, item); // or setMedia(media, item, positionMs: ms)
```

Kotlin:

```kotlin
val media = Media(
    kind = MediaKind.EPISODE,
    title = "Pilot",
    durationMs = 3_487_000,
    ids = mapOf("imdb" to "tt0959621", "tvdb" to "349232"),
    series = Series("Breaking Bad", year = 2008, ids = mapOf("imdb" to "tt0903747", "tmdb" to "1396", "tvdb" to "81189")),
    season = 1,
    episode = 1,
    ext = buildJsonObject { putJsonObject("com.example.player") { put("item", "4711") } },
)
val problem = checkMedia(media) // null when the relay accepts it, else the first broken rule
if (problem == null) party.setMedia(media, ref = item) // positionMs = 0 by default
```

Swift:

```swift
let media = Media(
    kind: .episode,
    title: "Pilot",
    durationMs: 3_487_000,
    ids: ["imdb": "tt0959621", "tvdb": "349232"],
    series: Series(title: "Breaking Bad", year: 2008, ids: ["imdb": "tt0903747", "tmdb": "1396", "tvdb": "81189"]),
    season: 1,
    episode: 1,
    ext: ["com.example.player": ["item": "4711"]]
)
let problem = checkMedia(media) // nil when the relay accepts it, else the first broken rule
if problem == nil { try party.setMedia(media, ref: item) } // positionMs: 0 by default
```

## 6. Guest flow, matching and the offset

A guest calls `join` with the room code. The SDK routes the code to its relay and, when the room has media, searches with your resolver. This step covers the outcomes of that search, what to show the user, and the offset.

### Outcomes

The search ends in one of three outcomes, which [the spec](../spec/SDK.md#96-outcomes) defines:

| Outcome | When it happens | What your app does |
|---|---|---|
| Matched | A file is within 1.5 seconds of the reference duration. | Opens `state.choice`, as in step 4. |
| Mismatch | No file is within 1.5 seconds, but some file opened. | Asks the user about `state.mismatch`, then answers once. |
| Unavailable | No candidate opened, or the party has no resolver. | Lets the user pick a file for `choose`. |

On a mismatch, `state.mismatch` holds the closest file and its `deltaMs`, which is the file's own duration minus the reference duration.

When the outcome is unavailable, `choose` still works, with a candidate from `state.candidates` or from anywhere else.

### Answering a mismatch

Answer a mismatch with one of two calls: `acceptMismatch` plays the offered file, and `declineMismatch` sits out this title. With any outcome, the user can also pick a file from the choose list or from your app's own search, which you pass to `choose`.

```ts
// state.mismatch is set: ask the user, then answer with one of these.
party.acceptMismatch(); // play the offered file
party.declineMismatch(); // sit out this title
chooseList(party, library, showChoice); // step 3: the user's pick calls choose(playable(library, item))
await party.choose(playable(library, found)); // or a file from the app's own search, from anywhere
// On a different cut: local position = room position + offset.
party.setOffset(party.state.offsetMs + 1000);
```

The other languages make the same calls, with a few differences:

- Dart is the same, `chooseList(party, library, show)` included.
- Kotlin reads the state through `.value` and passes its scope to `chooseList(party, library, mainScope, show)`. Its offset call is `party.setOffset(party.state.value.offsetMs + 1000)`.
- Swift puts `try` on the throwing calls and `await` on `choose`, as in `_ = await party.choose(playable(library, found))` and `try party.setOffset(party.state.offsetMs + 1000)`.

### Showing the matched file

A matched episode can still be the wrong one. The SDK walks the ranked candidates and stops at the first one within 1.5 seconds. So under different numbering, another episode of similar length matches silently.

Always show the matched file next to the room's `media`, with its own title, season, episode and file name. Offer the choose list too. Choosing a file keeps the current offset.

### The offset

A different cut of the title can play in sync with an offset. The local position is the room position plus the offset, so a longer intro needs a positive offset. The offset resets on new media.

Offer offset steps of 100 milliseconds and 1 second, a reset, and the mismatch's `deltaMs` as a hint. The rules are in [offset](../spec/SDK.md#13-offset).

## 7. Controls and host tools

Your app's controls send the user's presses to the party, never straight to the player. Who may make each call depends on the user's role:

| Calls | Who may make them | Error otherwise |
|---|---|---|
| `play`, `pause`, `seek`, `setRate` | A participant with `canControl`, while the room has media. | `invalid_message` without media, `forbidden` without control. |
| `setControl`, `transferHost`, `kick`, `lock`, `clearMedia`, `end` | The host, with or without media. | `forbidden` |

Outside a room, all of these calls throw `not_in_room`.

```ts
// The user's own presses go to the party, never straight to the player.
if (party.state.canControl) {
  party.play();
  party.pause();
  party.seek(754000); // in the user's own file
  party.setRate(1.5); // 0.5 to 2.0
}

// Host only.
party.setControl("everyone"); // or "host"
party.transferHost(peerId);
party.kick(peerId);
party.lock(true);
party.clearMedia();
party.end();

party.leave(); // ends this session with "left"
await party.close(); // leaves and releases the party for good; dispose of the player after this
```

In the other languages:

- Dart names the control modes `Control.everyone` and `Control.host`. Kotlin names them `Control.EVERYONE` and `Control.HOST`.
- In Swift, these calls are synchronous and throw, as in `try party.play()`. `leave()` doesn't throw.

### Host changes

Host changes need no app code beyond the UI. `transferHost(peerId)` hands the host role over at once. If the host stays disconnected for 30 seconds, the relay promotes the first connected participant.

## 8. Leaving the player, background and reconnects

This step covers the times the user isn't watching, and how a party ends.

### Detaching the player

Call `detachPlayer()` when the user leaves the player or your app goes to the background. The room then stops waiting for this user.

1. Set `away` first, so that `openChoice` from step 4 doesn't attach the player again.
2. Await the completion that `detachPlayer` returns. It settles once no SDK command runs on the player.
3. Cancel or await any open that is under way.
4. Dispose of the player.

Await the completion of `close()` the same way before you dispose of the player.

An app that doesn't play, such as a remote control, detaches right after joining.

When the user comes back to the player, clear `away` and the attached ref, and call `openChoice` again.

TypeScript:

```ts
// The room stops waiting for this user.
away = true;
await party.detachPlayer(); // settles once no SDK command runs on the player: dispose of it after this

// Back in the player:
away = false;
attached = null;
await openChoice();
```

Dart:

```dart
// The room stops waiting for this user.
away = true;
await party.detachPlayer(); // settles once no SDK command runs on the player: dispose of it after this

// Back in the player:
away = false;
attached = null;
await openChoice();
```

Kotlin:

```kotlin
// The room stops waiting for this user.
away = true
party.detachPlayer().join() // settles once no SDK command runs on the player: dispose of it after this

// Back in the player:
away = false
attached = null
openChoice()
```

Swift:

```swift
// The room stops waiting for this user.
away = true
await party.detachPlayer().value // settles once no SDK command runs on the player: dispose of it after this

// Back in the player:
away = false
attached = nil
try await openChoice()
```

### Reconnects and restarts

When the connection drops, the SDK resumes on its own, with backoff, for up to 5 minutes. While it reconnects, commands throw `not_in_room`.

The SDK keeps nothing across an app restart. After a restart, join the room again with its code.

### How a party ends

A party ends with a reason, and your UI handles each one:

| Reason | What happened |
|---|---|
| `left` | Your app called `leave`. |
| `ended` | The host ended the room. |
| `kicked` | The host kicked this user. |
| `expired` | The room reached its maximum age, which is 24 hours by default. |
| `replaced` | A resume elsewhere took over this connection. |
| `lost` | The party can't go on, because resuming became impossible or the room's new media wasn't valid. |

An ended party may `create` or `join` again. After `close`, the party can't be used at all.

The rules are in [ending](../spec/SDK.md#74-ending) and [reconnect and resume](../spec/SDK.md#14-reconnect-and-resume).

## 9. Events and errors

State drives rendering, and events drive messages. Draw your UI from the state, and show a toast or a prompt for an event.

### Events

- An `action` event says who played, paused, seeked or changed something. An `action` of kind `ended` means the media ended, unlike the `ended` event, which means the party ended.
- A `playBlocked` event, from step 2, carries what `play` threw. Prompt the user to press play on the player itself, and hide the prompt once the player leaves `loading` or the room stops playing.
- Ignore event kinds you don't know, including types that a later SDK 1.x adds.

### Errors

Errors carry a `code`, a `message` and `re`. They are `PartyError` in TypeScript and Swift, and `PartyException` in Dart and Kotlin. Show your own text for each code, and keep `message` for logs.

Plan your own wording for these codes, and use a generic text for the rest:

| Call | Codes to word |
|---|---|
| `create` | `relay_full`, `unreachable` |
| `join` | `room_not_found`, `room_full`, `room_locked`, `invalid_code`, and with `create` also `relay_full` |

The relay's codes are listed in [the protocol's error codes](../spec/SPEC.md#appendix-a-error-codes). The SDK adds three of its own: `invalid_code`, `unreachable` and `cancelled`.

```ts
import { PartyError, type PartyEvent } from "openwatchparty";

try {
  await party.join(typedCode);
} catch (e) {
  if (!(e instanceof PartyError)) throw e;
  showError(e.code); // your own text per code; e.message is for logs
}

function onEvent(event: PartyEvent): void {
  switch (event.type) {
    case "action":
      if (!event.byMe) toast(`${event.by?.name ?? "The room"}: ${event.kind}`);
      break;
    case "playBlocked":
      promptPlay(); // the player refused to start: ask the user to press play on it
      break;
    case "ended":
      showEnded(event.reason);
      break;
    default:
      break; // other events, also types a later SDK 1.x adds
  }
}
```

### In the other languages

- In Dart and Kotlin, the same shape catches `PartyException`.
- Dart's events are `ActionEvent`, `PlayBlockedEvent` and `EndedEvent`. Kotlin's are `PartyEvent.Action`, `PartyEvent.PlayBlocked` and `PartyEvent.Ended`.
- Neither language's event type is exhaustive. A Dart `switch` expression needs a `_` case, and a Kotlin `when` expression needs an `else` branch.
- In Swift, the same shape catches `PartyError`. After `close`, `create` and `join` throw `PartyClosedError`.
- Swift's events are the structs `ActionEvent`, `PlayBlockedEvent` and `EndedEvent`, which conform to the protocol `PartyEvent`. A Swift `switch` over them needs a `default` branch.

## 10. Relays

The `Relays` object you pass to the party decides where rooms live. You can use the public relays or a relay of your own:

- `Relays.public()` starts from the SDK's compiled-in list and refreshes it from `https://openwatch.party/relays.json`, and from nowhere else. A code's first symbol names its relay, which `relays.relayFor(code)` returns.
- `Relays.at(baseUrl)` is one relay for everything. The party creates and joins rooms there and fetches nothing.

Codes don't cross sources: a room created through one can't be joined through the other. The rules are in [relay list](../spec/SDK.md#3-relay-list) and [routing](../spec/SDK.md#42-routing).

```ts
import { Relays } from "openwatchparty";

const publicRelays = Relays.public();
const ownRelay = Relays.at("https://relay.example.com");
```

The other languages construct them like this:

| Language | Public relays | Your own relay |
|---|---|---|
| Dart | `Relays.public()` | `Relays.at('https://relay.example.com')` |
| Kotlin | `Relays.public(http)` | `Relays.at(http, "https://relay.example.com")` |
| Swift | `Relays.public()` | `try Relays.at("https://relay.example.com")` |

### Testing with a local relay

For testing, point `Relays.at` at a local relay. You can start one from the repository root:

```sh
cd relay && go run ./cmd/owp-relay --addr 127.0.0.1:8080 --admin-addr ""
```

The relay is ready when it prints `listening 127.0.0.1:8080`. To run many client instances on one machine, raise its per-IP limits: `--max-connections-per-ip`, `--max-rooms-per-ip` and `--join-burst`. See the [relay README](../relay/README.md) and [`owp-peer`](../conformance/peer/README.md).

## 11. Using the public relay well

Under [the terms](terms.md#who-may-use-the-public-relay), the public relay is free for open-source apps. Other apps need written permission. Every app must stay within [acceptable use](terms.md#acceptable-use).

Identify your app and your user truthfully:

- `client` is `<app>/<version>`, naming the real app. It is 1 to 64 characters long, with at least one character on each side of the first `/`.
- Deny lists and usage counts key on `<app>`, and a denied app gets `client_blocked`.
- `name` is the user's display name, 1 to 32 characters long. It doesn't have to be unique.

The exact rules are in [name and client](../spec/SPEC.md#63-name-and-client).

When you write your own privacy text, [privacy in the protocol](../spec/SPEC.md#15-privacy) and [the privacy policy](privacy.md) state what the relay and other participants see.

## Checklist

Before you ship, check these points:

- Your resolver returns catalogue-ID matches first, and title matches only when no ID matched. It searches episodes by the show's title, and adds near misses to fill the choose list.
- Your app shows the matched file next to the room's media and offers the choose list, so that the user can replace a wrong episode matched on duration.
- Over IPC, reads come from a property-event cache, commands await the player's reply, and a seek waits until it lands.
- In a browser extension, the adapter follows element replacement and is released during ads.

The SDK spec lists [what apps must do](../spec/SDK.md#167-what-apps-must-do). Work through that list before you ship.

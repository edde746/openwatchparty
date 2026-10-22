`OpenWatchParty` is a Swift SDK that keeps players in different apps in sync in one room. Each person in the room plays their own copy of the title, and a relay keeps the copies together.

The SDK implements the client behaviour in [`spec/SDK.md`](../../spec/SDK.md). This page names the rules the SDK follows, and SDK.md explains them in full.

## Install

With SwiftPM, add the package and the product to `Package.swift`:

```swift
.package(url: "https://github.com/edde746/openwatchparty", from: "1.0.0"),
// target dependency:
.product(name: "OpenWatchParty", package: "openwatchparty"),
```

In Xcode, choose File → Add Package Dependencies, enter `https://github.com/edde746/openwatchparty` and add the product `OpenWatchParty`. SwiftPM takes the version from the git tag.

## Requirements

The package runs on these platforms:

| Platform | Minimum |
|---|---|
| iOS | 16 |
| macOS | 13 |
| tvOS | 16 |
| watchOS | 9 |
| visionOS | 1 |
| Linux | Swift 6 toolchain |

It needs Swift tools 6.0 and builds in the Swift 6 language mode. It has no dependencies beyond Foundation, and `FoundationNetworking` on Linux.

## Quick start

This example joins a room by its code, or creates one and shares a title as host. It then attaches the player each time the SDK chooses a file, until the party ends. The resolver here knows one file, where yours searches your app's library.

```swift
import Foundation
import OpenWatchParty

/// The app's player as the party sees it: read on every tick, commanded by the party.
@MainActor
final class VideoPlayer: PlayerAdapter {
    var positionMs: Int64 = 0
    var durationMs: Int64? = 10_200_000
    var paused = true
    var buffering = false
    var firstFrameShown = true

    func play() async throws { paused = false } // throw when the player refuses to start
    func pause() async throws { paused = true }
    func seek(to positionMs: Int64) async throws { self.positionMs = positionMs }
}

@MainActor
func watchTogether(code: String?) async throws {
    let heat = URL(fileURLWithPath: "/Movies/Heat.mkv")
    let party = try WatchParty<URL>(
        relays: .public(),
        client: "Example/1.0",
        name: "Anna",
        resolver: Resolver { media in
            // The files in the app's library that are this title, best first.
            media.title == "Heat" ? [Candidate(id: "heat", ref: heat, durationMs: 10_200_000)] : []
        }
    )
    // Subscribe before create or join: a stream gets what is published from then on, nothing earlier.
    let states = party.states()
    let events = party.events()
    Task {
        for await event in events {
            if event is PlayBlockedEvent { print("Tap play to start the video") }
        }
    }
    if let code {
        try await party.join(code) // "k7m-q2x" works too
    } else {
        let code = try await party.create()
        print("Room code: \(try formatCode(code))")
        let media = Media(kind: .movie, title: "Heat", durationMs: 10_200_000, year: 1995, ids: ["imdb": "tt0113277"])
        try party.setMedia(media, ref: heat)
    }
    let player = VideoPlayer()
    var opened: URL?
    for await state in states {
        let file = state.choice?.ref
        if let file, file != opened {
            opened = file // open `file` in the player, then:
            party.attachPlayer(player, ref: file)
        }
        if state.connection == .ended { break }
    }
    await party.close().value
}
```

## Relays

A `Relays` object tells your parties which relays exist and which one owns a room code. Create one for the whole app and pass it to every party.

There are two kinds of list:

- `Relays.public()` uses the public relays. It starts from the copy of [`relays.json`](../../relays.json) compiled into the SDK.
- `try Relays.at("https://relay.example.com")` uses one [self-hosted relay](../../relay/README.md) for every room.

| Member | What it does |
|---|---|
| `Relays.public(httpGet:clock:)` | Creates a list of the public relays. |
| `Relays.at(_:) throws` | Creates a list of one self-hosted relay. |
| `list` | The relays in use. |
| `relayFor(_:)` | Finds the relay that owns a room code. |
| `refresh() async -> Bool` | Fetches the public list now, and returns whether a valid list came back. |

The GET of the public list defaults to `URLSession`, and you may replace it with `httpGet:`. When `refresh()` returns false because the source gave no valid list, the list in use stays.

`Relays.at` throws `invalid_message` on a bad base URL. Its `refresh()` does nothing and returns true.

## WatchParty

A `WatchParty` is one participant's session. It connects to a relay, keeps the room's state, matches the room's media to the user's own files and drives your player.

### Creating a party

The throwing initializer is `WatchParty<Ref>(relays:client:name:resolver:connect:clock:random:)`. `Ref` is the type of the app's `ref` in candidates, the value your app uses to open a file. It must be `Equatable & Sendable`, and refs compare with `==`.

| Parameter | Type | What it is |
|---|---|---|
| `relays` | `Relays` | The app's relay list. |
| `client` | `String` | Your app and its version, as `<app>/<version>`. |
| `name` | `String` | The user's display name, 1–32 characters. |
| `resolver` | `Resolver<Ref>?` | Your search for the user's own files. Defaults to `nil`. |
| `connect` | `WebSocketConnect?` | Opens the WebSockets. Defaults to `urlSessionConnect`. |
| `clock` | `any PartyClock` | Local time and timers. |
| `random` | `any RandomNumberGenerator` | Drives the spread of reconnects. |

The initializer throws a `PartyError` with the code `invalid_message` for a bad `client` or `name`. Without a resolver, the party never searches, so `match` is `unavailable` at once for other participants' media.

### State and session

| Member | What it does |
|---|---|
| `private(set) var state: PartyState<Ref>` | Everything the UI shows, as one value. |
| `states() -> AsyncStream<PartyState<Ref>>` | A new stream of every state from now on. |
| `events() -> AsyncStream<any PartyEvent>` | A new stream of every event from now on. |
| `create() async throws -> String` | Creates a room, joins it as host and returns its code. |
| `join(_:create:) async throws` | Joins a room by a typed or pasted code, such as `k7m-q2x`. With `create`, makes the room when no room has the code. |
| `leave()` | Leaves the room. |
| `close() -> Task<Void, Never>` | Leaves the room and releases everything. |
| `roomPositionMs() -> Int64?` | The room's position now. |

To bring a group back to a code it used before, such as one from your app's recent rooms, call `join(code, create: true)`. The call joins the room when it exists. Otherwise it makes a new room with that code, and you are its host. A relay that predates `create` answers `room_not_found`, and a full relay answers `relay_full`.

### Matching and the player

| Member | What it does |
|---|---|
| `choose(_:) async -> Bool` | Takes a file the user picked by hand. |
| `acceptMismatch()` | Plays the offered file, though its duration differs. |
| `declineMismatch()` | Declines the offered file. |
| `resolveAgain()` | Searches again for the current media. |
| `attachPlayer(_:ref:)` | Lets the SDK drive your player, which has the file of `ref` open. |
| `detachPlayer(_:) -> Task<Void, Never>` | Stops driving the player when the user leaves it. |
| `playerChanged()` | Tells the SDK the player's state changed, so it reads it now. |

[Player and resolver](#player-and-resolver) explains how to use the player members.

### Playback

These commands act on the whole room. Send the user's presses through them, not straight to the player.

| Member | What it does |
|---|---|
| `play() throws` | Asks the room to play. |
| `pause() throws` | Pauses the room. |
| `seek(to:) throws` | Moves the room to a position in the user's own file. |
| `setRate(_:) throws` | Sets the room's rate, from 0.5 to 2.0. |
| `setOffset(_:) throws` | Sets your local offset from the room's position. |

The offset applies only to your own player. It may be at most 604800000 ms, or 7 days, either way.

### Host controls

Only the host may use these.

| Member | What it does |
|---|---|
| `setMedia(_:ref:positionMs:) throws` | Shares a title, with `ref` for your own file of it. |
| `clearMedia() throws` | Stops the media. |
| `setControl(_:) throws` | Sets who may play, pause, seek and set the rate. |
| `transferHost(_:) throws` | Makes another participant the host. |
| `kick(_:) throws` | Removes a participant from the room. |
| `lock(_:) throws` | Refuses or allows new joins. |
| `end() throws` | Ends the room for everyone. |

### Party state

`state` is a `PartyState`. It changes on events, never on every tick.

| `PartyState` field | What it holds |
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

Events conform to the protocol `PartyEvent`.

| Event | When it comes |
|---|---|
| `ActionEvent` | A room change carried an action, such as someone pressing play. |
| `ParticipantJoinedEvent` | Someone joined the room. |
| `ParticipantLeftEvent` | Someone left the room. |
| `ErrorEvent` | The relay refused a command. |
| `ResolveFailedEvent` | The resolver failed. |
| `PlayBlockedEvent` | The player refused to start. |
| `EndedEvent` | The party ended. |

Because `PartyEvent` is a protocol, a `switch` over an event needs a `default` branch.

### Media descriptors

A `Media` value is a media descriptor: it names a title, and each participant finds their own copy of it. `Media`, `Series` and `MediaFile` are value types, and `Media.ext` is `[String: JSONValue]?`.

## Player and resolver

Your app gives the party two pieces of its own: a player adapter for your player, and a resolver for your library.

### The player adapter

Implement the `@MainActor` protocol `PlayerAdapter` in a class, as the quick start does. Its commands are `async throws`. A protocol extension gives the last three members their defaults.

| Member | What it is |
|---|---|
| `positionMs` | The current position in your own file. |
| `durationMs` | The file's duration, or nil when unknown. |
| `paused` | Whether the player is paused. |
| `buffering` | Whether the player stalled while trying to play. |
| `firstFrameShown` | Whether the file is open and shows a frame. |
| `play()` | Starts playing. |
| `pause()` | Pauses. |
| `seek(to:)` | Seeks, and returns once the seek has landed. |
| `canSetRate` | Whether `setRate(_:)` works. Defaults to false. |
| `seekAccuracyMs` | How far from the target a seek may land, such as your player's keyframe distance. Defaults to 0. |
| `setRate(_:)` | Changes the speed. Does nothing by default. |

Without rate control, the SDK keeps your player in step with seeks alone.

When the player refuses to start, `play` throws. While the player then stays paused, the party emits `PlayBlockedEvent`. Ask the user to press play on the player itself.

### Attaching and detaching

Call `attachPlayer(_:ref:)` each time you open a file.

`close()` and `detachPlayer()` return completions. Await them with `.value`, as in `await party.close().value`, before you dispose of the player.

### The resolver

The resolver finds the user's own copy of the room's title. `Resolver<Ref>` is a struct built from a closure, `@MainActor (Media) async throws -> [Candidate<Ref>]`, which returns candidates best first. A throw counts as no candidates.

`Candidate<Ref>` holds `id`, `ref`, `durationMs`, `sizeBytes`, `oshash`, `name` and an optional `probe`. The probe learns the duration. The SDK cancels the probe's task when it no longer needs the answer, and a failed probe counts as no duration.

### Helpers

| Helper | What it does |
|---|---|
| `formatCode(_:) throws` | Formats a room code for display, such as `K7M-Q2X`. |
| `sameTitle(_:_:)` | Tells whether two descriptors name the same title. |
| `checkMedia(_:)` | Checks a descriptor before you send it, and returns the first broken rule or nil. |
| `parseMedia(_:) throws` | Decodes a descriptor with the checks a receiver makes. |

## Threading

The party is `@MainActor`, and it calls your adapter and resolver on the main actor.

Call `states()` and `events()` before `create` or `join`. Each call returns its own `AsyncStream`, which gets every change or event in order. Nothing is replayed, and both streams finish on `close()`.

Cancelling the task that awaits `create` or `join` cancels the attempt and throws `CancellationError`.

## Errors

The SDK reports failures as a `PartyError`. Show the user your own text for each `code`, with a generic fallback, because `message` is meant for logs only.

| Field | What it holds |
|---|---|
| `code` | A `PartyError.Code` value, such as `.roomNotFound` with the raw value `"room_not_found"`. |
| `message` | Text for logs only. |
| `re` | The type of the refused message. |
| `underlying` | The error underneath, when there is one. |

`PartyError.Code` names these codes. Other codes arrive as raw `Code` values.

| Name | Code |
|---|---|
| `invalidCode` | `invalid_code` |
| `unreachable` | `unreachable` |
| `cancelled` | `cancelled` |
| `notInRoom` | `not_in_room` |
| `alreadyInRoom` | `already_in_room` |
| `forbidden` | `forbidden` |
| `invalidMessage` | `invalid_message` |
| `roomNotFound` | `room_not_found` |
| `roomFull` | `room_full` |
| `roomLocked` | `room_locked` |
| `rateLimited` | `rate_limited` |
| `relayFull` | `relay_full` |
| `resumeFailed` | `resume_failed` |
| `clientBlocked` | `client_blocked` |

Calling `create` or `join` after `close()` throws `PartyClosedError`. It signals a programming error, and it is not a `PartyError`.

`ResolveFailedEvent` carries a `TimeoutError` when the resolver took longer than 30 seconds.

## Build and test

Run these commands at the repository root:

```sh
swift build
swift test
swift test --filter VectorTests
```

`swift test` includes `IntegrationTests`, which runs whole parties against a relay. `OWP_RELAY_BIN` names the relay binary. Without it, the suite builds `relay/` with Go, so Go must be installed.

On iOS and tvOS simulators, `xcodebuild test` runs the suites that need no processes or local server. `IntegrationTests` and `DefaultHTTPGetTests` build on macOS and Linux only.

```sh
xcodebuild test -scheme OpenWatchParty -destination 'platform=iOS Simulator,name=iPhone 17'
xcodebuild test -scheme OpenWatchParty -destination 'platform=tvOS Simulator,name=Apple TV 4K (3rd generation)'
```

watchOS and visionOS build with these commands:

```sh
xcodebuild build -scheme OpenWatchParty -destination 'generic/platform=watchOS Simulator'
xcodebuild build -scheme OpenWatchParty -destination 'generic/platform=visionOS Simulator'
```

CI is `.github/workflows/sdk-swift.yml`, with a macOS job and a Linux job in `swift:6.4`.

## Links

- [`docs/integration.md`](../../docs/integration.md) is the end-to-end integration guide.
- [`spec/SPEC.md`](../../spec/SPEC.md) is the wire protocol.
- The [TypeScript](../typescript/README.md), [Dart](../dart/README.md) and [Kotlin](../kotlin/README.md) SDKs cover other platforms.

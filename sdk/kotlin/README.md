`party.openwatch:openwatchparty` is a Kotlin Multiplatform SDK that keeps players in different apps in sync in one room. Each person in the room plays their own copy of the title, and a relay keeps the copies together.

The SDK implements the client behaviour in [`spec/SDK.md`](../../spec/SDK.md). This page names the rules the SDK follows, and SDK.md explains them in full.

## Install

In a JVM or Android module, add the SDK and a Ktor engine from `mavenCentral()`:

```kotlin
dependencies {
    implementation("party.openwatch:openwatchparty:1.1.0")
    implementation("io.ktor:ktor-client-okhttp:3.6.0")
}
```

In a Kotlin Multiplatform module, depend on the SDK from `commonMain`, and add a Ktor engine for each target:

```kotlin
kotlin {
    sourceSets {
        commonMain.dependencies { implementation("party.openwatch:openwatchparty:1.1.0") }
        jvmMain.dependencies { implementation("io.ktor:ktor-client-okhttp:3.6.0") }
        appleMain.dependencies { implementation("io.ktor:ktor-client-darwin:3.6.0") }
    }
}
```

No Ktor engine is bundled, and the engine you add must support WebSockets:

- On JVM and Android, use OkHttp.
- On Apple platforms, use Darwin.
- Apache5, Jetty and the Android engine don't work.

With no engine argument in common code, Ktor picks the engine from each target's dependencies.

## Requirements

The SDK builds all its targets from `commonMain` code: `jvm`, Android, `iosArm64`, `iosSimulatorArm64`, `macosArm64`, `tvosArm64` and `tvosSimulatorArm64`.

Apple targets produce klibs. A Swift-only app therefore needs its own Kotlin Multiplatform module that depends on the SDK.

JVM and Android bytecode targets 17. Android uses `compileSdk = 37` with `minSdk = 26`.

The SDK is built with these versions:

| Tool or library | Version |
|---|---|
| Kotlin | 2.4.10 |
| AGP | 9.4.1 |
| Ktor | 3.6.0 |
| kotlinx-coroutines | 1.11.0 |
| kotlinx-serialization | 1.11.0 |

An Android app on AGP 9 whose built-in Kotlin is older than 2.3 must pin the Kotlin Gradle plugin:

```kotlin
buildscript {
    repositories { mavenCentral() }
    dependencies { classpath("org.jetbrains.kotlin:kotlin-gradle-plugin:2.4.10") }
}
```

## Quick start

This example creates a room, or joins one by its code. Whenever the SDK chooses a file, the example opens it and attaches the player. It also shares a title as host, and sends the user's controls through the party. `ExamplePlayer` and `searchLibrary` stand in for your app's own player and library.

```kotlin
import io.ktor.client.HttpClient
import io.ktor.client.engine.okhttp.OkHttp
import io.ktor.client.plugins.websocket.WebSockets
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put
import kotlinx.serialization.json.putJsonObject
import party.openwatch.Candidate
import party.openwatch.Media
import party.openwatch.MediaKind
import party.openwatch.PartyEvent
import party.openwatch.PartyException
import party.openwatch.PlayerAdapter
import party.openwatch.Relays
import party.openwatch.Resolver
import party.openwatch.WatchParty
import party.openwatch.formatCode

// One Ktor client with WebSockets and one Relays for the whole app.
val http = HttpClient(OkHttp) { install(WebSockets) }
val relays = Relays.public(http)

// The app's player, read and commanded by the SDK.
class ExamplePlayer : PlayerAdapter {
    override var positionMs = 0L
        private set
    override val durationMs: Long? = null
    override var paused = true
        private set
    override val buffering = false
    override val firstFrameShown = true
    var openRef: String? = null
        private set

    suspend fun open(ref: String) { openRef = ref /* open the file in the app's player; return once it's open */ }

    override suspend fun play() { paused = false }
    override suspend fun pause() { paused = true }
    override suspend fun seek(positionMs: Long) { this.positionMs = positionMs }
}

// Stand-in for the app's own library.
class ExampleFile(val id: String, val path: String, val durationMs: Long)
fun searchLibrary(title: String): List<ExampleFile> = emptyList()

// The user's own files for the room's media, best first; ref is what the app opens (here a path).
val resolver = Resolver<String> { media ->
    searchLibrary(media.title).map { Candidate(id = it.id, ref = it.path, durationMs = it.durationMs) }
}

fun startParty(scope: CoroutineScope, player: ExamplePlayer, code: String?): WatchParty<String> {
    val party = WatchParty(relays, client = "Example/1.0", name = "Anna", resolver = resolver, scope = scope)
    // Open the file the SDK chose, one at a time, and attach the player with the ref it opened.
    var attached: String? = null // the ref the player is attached with
    val opening = Mutex()
    suspend fun openChoice() = opening.withLock {
        while (true) {
            val ref = party.state.value.choice?.ref ?: break
            if (ref == attached) break // attached: the player follows by itself
            if (player.openRef != ref) player.open(ref)
            if (party.state.value.choice?.ref != ref) continue // a newer choice came meanwhile: open that one
            party.attachPlayer(player, ref)
            attached = ref
        }
    }
    scope.launch { party.state.collect { openChoice() } }
    scope.launch {
        party.events.collect { event ->
            when (event) {
                is PartyEvent.Action -> if (!event.byMe) println("${event.by?.name}: ${event.kind}")
                is PartyEvent.Ended -> {
                    println("party ended: ${event.reason}")
                    attached = null // the end released the player
                }
                else -> {}
            }
        }
    }
    scope.launch {
        try {
            if (code == null) println("room code: ${formatCode(party.create())}") else party.join(code)
        } catch (e: PartyException) {
            println("could not start the party: ${e.code}")
        }
    }
    return party
}

// As host: share the title you play; ref reopens your own file.
fun shareMovie(party: WatchParty<String>) {
    party.setMedia(
        Media(
            kind = MediaKind.MOVIE,
            title = "Example Movie",
            durationMs = 5_400_000,
            ids = mapOf("imdb" to "tt0000001"),
            ext = buildJsonObject { putJsonObject("com.example") { put("itemId", "42") } },
        ),
        ref = "/movies/example-movie.mkv",
    )
}

// The user's controls go through the party, never straight to the player.
fun onPlayPause(party: WatchParty<String>, player: ExamplePlayer) {
    val state = party.state.value
    if (state.media == null || !state.canControl) return
    if (player.paused) party.play() else party.pause()
}

fun onSeek(party: WatchParty<String>, positionMs: Long) = party.seek(positionMs)

fun onLeave(party: WatchParty<String>) = party.close() // join the returned Job before disposing the player
```

## Relays

A `Relays` object tells your parties which relays exist and which one owns a room code. Create one for the whole app and pass it to every party, as the quick start does.

Both kinds of list take your app's Ktor `HttpClient`, which is the SDK's only transport. It must have the `WebSockets` plugin installed, and the SDK never closes it.

- `Relays.public(http)` uses the public relays. It starts from the copy of [`relays.json`](../../relays.json) compiled into the SDK.
- `Relays.at(http, "https://relay.example.com")` uses one [self-hosted relay](../../relay/README.md) for every room, and it never fetches a list.

| Member | What it does |
|---|---|
| `Relays.public(http: HttpClient): Relays` | Creates a list of the public relays. |
| `Relays.at(http: HttpClient, baseUrl: String): Relays` | Creates a list of one self-hosted relay. |
| `val list: List<Relay>` | The relays in use. |
| `fun relayFor(code: String): Relay?` | Finds the relay that owns a room code. |
| `suspend fun refresh(): Boolean` | Fetches the public list now, and returns whether a valid list came back. |

A `Relay` is `(id, name, url, prefixes)`. When `refresh()` returns false because the public source gave no valid list, the list in use stays.

A `Relays.at` list is one `custom` relay that owns every code. Its `refresh()` does nothing and returns true. `Relays.at` throws a `PartyException` with the code `invalid_message` in two cases:

- `baseUrl` isn't `https://` or `http://` followed by a host.
- `baseUrl` gives a relay URL that [`spec/SDK.md`](../../spec/SDK.md) §3.2 refuses.

## WatchParty

A `WatchParty` is one participant's session. It connects to a relay, keeps the room's state, matches the room's media to the user's own files and drives your player.

### Creating a party

The constructor takes these parameters. `T` is the type of the app's `ref` in candidates, the value your app uses to open a file.

| Parameter | Type | What it is |
|---|---|---|
| `relays` | `Relays` | The app's relay list. |
| `client` | `String` | Your app and its version, as `<app>/<version>`. |
| `name` | `String` | The user's display name, 1–32 characters. |
| `resolver` | `Resolver<T>? = null` | Your search for the user's own files. |
| `scope` | `CoroutineScope` | Where the party runs. See [Threading](#threading). |
| `clock` | `MonotonicClock = MonotonicClock.System` | The monotonic local clock. |
| `random` | `Random = Random.Default` | Drives the spread of reconnects. |

The constructor throws a `PartyException` with the code `invalid_message` for a bad `client` or `name`. Without a resolver, the party never searches, so `match` is `unavailable` at once for other participants' media.

### State and session

| Member | What it does |
|---|---|
| `state` | Everything the UI shows, as one value. |
| `events` | What happens in the room. |
| `suspend fun create(): String` | Creates a room on the first relay to answer, joins it as host and returns its code. |
| `suspend fun join(code: String, create: Boolean = false)` | Joins a room by a typed or pasted code, such as `k7m-q2x`. With `create`, it makes the room when no room has the code. |
| `fun leave()` | Leaves the room. |
| `fun close(): Job` | Leaves the room and releases everything. |
| `fun roomPositionMs(): Long?` | The room's position now. |

`leave()` during `create` or `join` cancels them. After `close()`, `create` and `join` throw `IllegalStateException`.

To bring a group back to a code it used before, such as one from your app's recent rooms, call `join(code, create = true)`. The call joins the room when it exists. Otherwise it makes a new room with that code, and the user is its host. A full relay answers `relay_full`, and `join` doesn't try another relay then. A relay that predates `create` answers `room_not_found`.

### Matching and the player

| Member | What it does |
|---|---|
| `fun acceptMismatch()` | Plays the offered file, though its duration differs. |
| `fun declineMismatch()` | Declines the offered file. |
| `suspend fun choose(candidate: Candidate<T>): Boolean` | Takes a file the user picked by hand. |
| `fun resolveAgain()` | Searches again for the current media. |
| `fun attachPlayer(player: PlayerAdapter, ref: T)` | Lets the SDK drive your player, which has the file of `ref` open. |
| `fun detachPlayer(player: PlayerAdapter? = null): Job` | Stops driving the player when the user leaves it. |
| `fun playerChanged()` | Tells the SDK the player's state changed, so it reads it now. |

[Player and resolver](#player-and-resolver) explains how to use the player members.

### Playback

These commands act on the whole room. Send the user's presses through them, not straight to the player.

| Member | What it does |
|---|---|
| `fun play()` | Asks the room to play. |
| `fun pause()` | Pauses the room. |
| `fun seek(positionMs: Long)` | Moves the room to a position in the user's own file. |
| `fun setRate(rate: Double)` | Sets the room's rate, from 0.5 to 2.0. |
| `fun setOffset(offsetMs: Long)` | Sets your local offset from the room's position. |

The offset you set with `setOffset` applies only to your own player. It may be at most 604800000 ms, or 7 days, either way.

### Host controls

Only the host may use these.

| Member | What it does |
|---|---|
| `fun setMedia(media: Media, ref: T, positionMs: Long = 0)` | Shares a title, with `ref` for your own file of it. |
| `fun clearMedia()` | Stops the media. |
| `fun setControl(mode: Control)` | Sets who may play, pause, seek and set the rate. |
| `fun transferHost(peerId: String)` | Makes another participant the host. |
| `fun kick(peerId: String)` | Removes a participant from the room. |
| `fun lock(locked: Boolean)` | Refuses or allows new joins. |
| `fun end()` | Ends the room for everyone. |

The SDK copies what the app passes in, and it publishes read-only collections.

### Party state

`state` holds a `PartyState`. It changes on events, never on every tick.

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

Enum values are upper-case, such as `Phase.PLAYING` and `EndReason.LOST`.

### Events

`events` carries what happens in the room. Everything else is state.

| `PartyEvent` | When it comes |
|---|---|
| `Action` | A room change carried an action, such as someone pressing play. |
| `ParticipantJoined` | Someone joined the room. |
| `ParticipantLeft` | Someone left the room. |
| `Error` | The relay refused a command. |
| `ResolveFailed` | The resolver failed. |
| `PlayBlocked` | The player refused to start. |
| `Ended` | The party ended. |

`PartyEvent` is non-sealed and may grow in 1.x, so give a `when` over events an `else` branch.

### Media descriptors

A `Media` value is a media descriptor: it names a title, and each participant finds their own copy of it. `Media` is `(kind, title, durationMs, year, ids, series, season, episode, file, ext)`.

- `kind` is a `MediaKind`: `MOVIE`, `EPISODE` or `VIDEO`.
- An episode needs `series`.
- The keys of `ext` are reverse-DNS names, such as `com.example`.

## Player and resolver

Your app gives the party two pieces of its own: a player adapter for your player, and a resolver for your library.

### The player adapter

Implement `PlayerAdapter`, as the quick start does. A command must finish or throw.

| Member | What it is |
|---|---|
| `positionMs` | The current position in your own file. |
| `durationMs` | The file's duration, or null when unknown. |
| `paused` | Whether the player is paused. |
| `buffering` | Whether the player stalled while trying to play. |
| `firstFrameShown` | Whether the file is open and shows a frame. |
| `canSetRate` | Whether `setRate` works. Defaults to false. |
| `seekAccuracyMs` | How far from the target a seek may land, such as your player's keyframe distance. Defaults to 0. |
| `suspend play()` | Starts playing, and returns when done. |
| `pause()` | Pauses, and returns when done. |
| `seek(positionMs)` | Seeks, and returns once the seek has landed. |
| `setRate(rate)` | Changes the speed. Does nothing by default. |

Without rate control, the SDK keeps your player in step with seeks alone.

When the player refuses to start, `play` throws. While the player then stays paused, the party reports `loading` and emits `PlayBlocked`, carrying the thrown error. Your app then asks the user to press play on the player itself.

### Attaching and detaching

Call `attachPlayer(player, ref)` each time you open a file. Call it again after the party ends with `Ended`, because the end releases the player. As host, attach with the `setMedia` ref, before or after `setMedia`.

Call `detachPlayer` when the user leaves the player or the app goes to the background. Join the `Job` it returns, or the one `close()` returns, before you dispose of the player.

When your player's state changes between ticks, call `playerChanged()` so the SDK reads it at once.

While the user is away, the quick start's `openChoice()` must not open a file or attach the player, because `attachPlayer` would bring the user back. Add a flag that it checks before it opens a file and again before it attaches. Then handle the user leaving and returning:

1. When the user leaves, set the flag and join `party.detachPlayer()`.
2. When the user returns, clear the flag, set `attached = null` and call `openChoice()`.

The `Mutex` keeps that call from running alongside the `state` collector's.

### The resolver

The resolver finds the user's own copy of the room's title. `Resolver<T>` is a fun interface, `suspend fun resolve(media: Media): List<Candidate<T>>`, which returns candidates best first. The SDK cancels it when the room's media changes.

A resolver call counts as finding no candidates, and emits `ResolveFailed`, in three cases:

- It throws.
- It is cancelled by its own cancellation, such as its own timeout.
- It runs for over 30 seconds.

`Candidate<T>` is `(id, ref, durationMs?, sizeBytes?, oshash?, name?, probe?)`. Its `probe` opens the file to learn its duration. A probe that throws counts as returning nothing, and so does a probe cancelled by its own cancellation.

- Deciding which files are the same title is the resolver's job. The SDK ranks the candidates and never drops one.
- Descriptor strings from others are untrusted input.

### Helpers

| Helper | What it does |
|---|---|
| `formatCode(code)` | Formats a room code for display, such as `K7M-Q2X`. |
| `sameTitle(a, b)` | Tells whether two descriptors name the same title. |
| `checkMedia(media)` | Checks a descriptor before you send it, and returns the first broken rule or null. |
| `parseMedia(json)` | Decodes a descriptor with the checks a receiver makes. |

## Threading

The party runs in a child `SupervisorJob` of the given `scope`, on that scope's dispatcher. A scope with no dispatcher gets `Dispatchers.Default`. The party calls the adapter and resolver there, so use a single-threaded scope, such as a Main scope on Android, and call the party from that thread.

- Non-suspend methods run on the caller's thread.
- Cancelling the caller of `create` or `join` cancels the attempt, and the caller gets its own `CancellationException`.
- `close()` cancels only the party's own child scope.
- Timers are coroutines on that scope, so a test dispatcher drives them.

`state` is a `StateFlow<PartyState<T>>`, and `events` is a `SharedFlow<PartyEvent>`. Subscribe to `events` before `create` or `join`. It has no replay, though it drops nothing while you are subscribed.

## Errors

The SDK reports failures as a `PartyException`. Show the user your own text for each `code`, with a generic fallback, because `message` is meant for logs only.

| Field | What it holds |
|---|---|
| `code` | The error code. The companion object has constants for the codes. |
| `message` | Text for logs only. |
| `re` | The type of the refused message. |

The relay's refusals of sent commands arrive as `PartyEvent.Error`, not as exceptions. The table shows what raises each code.

| Error code | Raised by |
|---|---|
| `invalid_code` | `join`, and `formatCode` with a non-code. |
| `unreachable` | `create` and `join` when no relay answers within 10 seconds, and `Ended.error` for `LOST` after the resume window. |
| `cancelled` | `create` and `join` after `leave()` or `close()`. |
| `already_in_room` | `create` and `join` while connecting or in a room. |
| `not_in_room` | Commands outside a room. |
| `forbidden` | Host-only commands from a non-host, and playback commands without control. |
| `invalid_message` | Bad arguments and bad input, listed below. |

`invalid_message` covers these cases:

- The constructor got an invalid `client` or `name`.
- A `Relays.at` base isn't an HTTP or HTTPS URL with a host.
- An argument is out of range.
- A `joined` is malformed.
- `parseMedia` got a bad descriptor.
- A command needs media, and there is none.
- A command names an unknown peer.

Other codes are the relay's own, such as `room_not_found`, `relay_full`, `resume_failed` and `client_blocked`, and they pass through as they are. `join` also raises `room_not_found` when no relay owns the code.

## Build and test

Run these commands from `sdk/kotlin` in a checkout. Gradle 9.8.0 needs a JVM from 17 to 27, and CI uses Temurin 21.

```
./gradlew --console=plain assemble check
./gradlew --console=plain -Popenwatchparty.android=false compileKotlinIosArm64 compileKotlinTvosArm64 macosArm64Test iosSimulatorArm64Test tvosSimulatorArm64Test
```

`assemble check` builds JVM and Android, plus the Apple targets and their tests on a Mac. The Android target is on when an Android SDK is known, and `-Popenwatchparty.android=false` skips it. The second line is the Apple CI job.

| Suite | Source set | Runs on | Needs |
|---|---|---|---|
| Unit: `PartyTest`, `RelayListTest`, `VectorTest`, `WireTest`, `TimersTest` | `src/commonTest` | Every test task: `jvmTest`, `testAndroidHostTest`, `macosArm64Test`, `iosSimulatorArm64Test`, `tvosSimulatorArm64Test` | `generateTestData`, which runs automatically |
| Integration: `IntegrationTest` | `src/jvmTest` | `jvmTest` only | Go, and a relay on 127.0.0.1 ports 18300–18349 |

## Links

- [`docs/integration.md`](../../docs/integration.md) is the end-to-end integration guide.
- [`spec/SPEC.md`](../../spec/SPEC.md) is the wire protocol.
- The [TypeScript](../typescript/README.md), [Dart](../dart/README.md) and [Swift](../swift/README.md) SDKs cover other platforms.
- Ktor engine support is at <https://ktor.io/docs/client-engines.html>.

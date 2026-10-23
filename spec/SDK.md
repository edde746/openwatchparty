# Open Watch Party SDKs

This document describes what an Open Watch Party SDK does and the API every SDK shares. Sections §3–§15 define the client behaviour every SDK implements, and §16 defines the shared API of the Kotlin, Dart, TypeScript and Swift SDKs.

`SPEC.md` owns the wire, and it wins over this document on anything that crosses the wire. The vector tables in `conformance/vectors/` pin the deterministic parts, as §17 describes.

The key words MUST, MUST NOT, REQUIRED, SHOULD, SHOULD NOT, RECOMMENDED, MAY and OPTIONAL in this document are to be interpreted as described in RFC 2119 and RFC 8174 when, and only when, they appear in all capitals.

## Contents

1. Scope
2. Conventions and constants
3. Relay list
4. Room codes and routing
5. Clock sync
6. Choosing a relay for `create`
7. Session lifecycle
8. Room state
9. Matching
10. Status
11. Following the room
12. Local commands
13. Offset
14. Reconnect and resume
15. Events
16. API
17. Conformance

## 1. Scope

An SDK does everything between the app and the relay, the WebSocket included:

|what the SDK does|section|
|---|---|
|keeps the relay list|§3|
|routes room codes to their relay|§4|
|syncs the clock|§5|
|chooses a relay for a new room|§6|
|tracks the room state|§8|
|matches media through the app's resolver|§9|
|reports `status`|§10|
|follows the room|§11|
|sends the user's commands|§12|
|resumes after a drop|§14|
|emits events|§15|

The app supplies a player adapter and the UI. To match other participants' media, it also supplies a resolver. §16.7 lists what an app must do.

An SDK MUST implement §3–§15 and pass every vector table of §17. This document defines the algorithms and values that `SPEC.md` §16 leaves to the SDKs.

### Packages

An SDK needs only WebSocket, HTTP GET and JSON. The app may supply its own WebSocket and HTTP GET, as §16.8 shows.

|SDK|package|depends on|
|---|---|---|
|Kotlin|Maven `party.openwatch:openwatchparty`, Kotlin package `party.openwatch`|kotlinx-coroutines, kotlinx-serialization-json and the app's Ktor `HttpClient` with WebSockets|
|Dart|`openwatchparty`|the packages `web_socket` and `web`|
|TypeScript|npm `openwatchparty`|the global `WebSocket` and `fetch` of Node ≥ 22, with no runtime dependencies|
|Swift|SwiftPM package `OpenWatchParty` from `https://github.com/edde746/openwatchparty`|Foundation only, with `FoundationNetworking` on Linux|

The Dart SDK is pure Dart, without Flutter, and needs Dart ≥ 3.4 < 4. Its default `httpGet` needs `dart:io` or JS interop. On a platform with neither, it throws `UnsupportedError`, so the app passes its own.

The Swift SDK needs Swift tools ≥ 6.0 and uses the Swift 6 language mode. It supports iOS 16, macOS 13, tvOS 16, watchOS 9 and visionOS 1, and has no dependencies beyond Foundation. Its defaults use `URLSessionWebSocketTask` and `URLSession`.

## 2. Conventions and constants

This section fixes how every SDK handles times, integers, timers and doubles, and lists the constants the later sections use.

All times and durations are integers in milliseconds, written ms.

### Integers

Every integer the SDK sends is integral and lies within ±(2^53 − 1), as `SPEC.md` §2 requires, and within its field's bounds. It is written without a fraction or exponent: `1000`, not `1000.0` or `1e3`. The Dart SDK sends an `int`, never a `double`.

The SDK checks every wire integer before any local effect and before sending it, whether the app passed it or the SDK computed it. A failed check throws `invalid_message` and changes nothing. These values are checked:

- the values of `seek`, listed in §12
- the position `pause` computes
- the `setMedia` position, which must lie within 0..2^53 − 1
- the integers in a descriptor, listed in §9.7
- the `setOffset` value, which must lie within ±604800000 as §13 says

### Timers

Delays are 64-bit integers in ms, and a negative delay counts as 0. A native timer gets at most 2147483647 ms. A clamped timer recomputes the remaining delay when it fires. No unbounded millisecond value becomes a native duration without this clamp.

### Time

Local time comes from a monotonic clock, never the wall clock. The defaults are Kotlin's `TimeSource.Monotonic`, Dart's `Stopwatch`, TypeScript's `Math.floor(performance.now())` and Swift's `ContinuousClock`. The app may pass its own clock through the parameters of §16.8.

Relay time now is local time plus the clock offset of §5. It is unknown before the first clock sample.

### Arithmetic and comparisons

Integer halving rounds down, and its operands are always ≥ 0. Doubles are IEEE 754, and each operation is rounded on its own. Where a language may fuse `a × b + c` into one operation, compute every product and quotient into its own variable.

Two phrases have a fixed meaning in comparisons:

- Two optional values are "equal" only when both are present and equal.
- Values compared "ignoring case" must also both be non-empty, and only ASCII `A`–`Z` fold to `a`–`z`.

### Constants

These values are constants, not settings:

|constant|value|section|
|---|---|---|
|ping burst|3 pings, 500 ms apart, at connection open|§5, §6|
|ping interval|5000 ms after the burst's first ping|§5|
|clock window|8 samples|§5|
|largest usable round trip|1000 ms|§5, §6|
|clock-step slack|100 ms|§5|
|connect timeout|10000 ms|§6, §7.1, §14|
|resolve timeout|30000 ms|§9.2|
|HTTP GET|aborted after 5000 ms, body at most 65536 bytes, no redirects|§3.4|
|public list source|`https://openwatch.party/relays.json`|§3|
|list refresh|on `joined`, when this object has had no successful refresh yet or the last one is 24 h old, and no background refresh is running|§3|
|match tolerance|1500 ms|§9|
|probes|4 at most, 60000 ms in total|§9|
|status heartbeat|15000 ms|§10|
|status spacing|250 ms|§10|
|tick|500 ms|§11|
|deadband|350 ms|§11|
|nudge|× 0.96 ahead, × 1.04 behind, until under 150 ms|§11|
|seek threshold|2000 ms with rate control and 1000 ms without, or the adapter's seek accuracy when larger|§11|
|held seek threshold|500 ms, or the adapter's seek accuracy when larger|§10, §11|
|seek lead|250 ms|§11|
|seek cooldown|2000 ms|§11|
|end margin|1000 ms|§11|
|rate tolerance|0.001|§11|
|command hold|2000 ms|§12|
|silence timeout|15000 ms|§14|
|reconnect backoff|500 ms doubling to 8000 ms, × a random factor 0.5 + 0.5u (§16.8)|§14|
|resume window|300000 ms|§14|
|largest timer delay|2147483647 ms|§2|
|media duration|1 to 604800000 ms (7 days)|§9.7|
|offset|−604800000 to 604800000 ms|§13|

## 3. Relay list

A party needs to know which relays exist and which room codes each one serves. This section defines where that list comes from, how the SDK checks it, when it refreshes it and how it fetches it.

### 3.1 Sources

A `Relays` object holds the relays a party uses. One object serves every party, and the list and its refresh schedule belong to the object. There are two kinds.

#### The public list

`Relays.public()` holds the public list of `SPEC.md` §5.2. It starts from the SDK's compiled copy of the repository's `relays.json`. Each valid refresh from `https://openwatch.party/relays.json` replaces the list, in memory only.

#### A self-hosted relay

`Relays.at(baseUrl)` is one self-hosted relay that serves everything. `create` uses it alone, as §6 says, and `join` and resume go to it whatever the code's first symbol is. It never fetches anything.

`baseUrl` is the relay's `https://` or `http://` base, such as `https://relay.example.com` or `http://192.168.1.5:8080/owp`. The constructor removes trailing `/`s. After that trimming, it throws `invalid_message` unless the base is `https://` or `http://` in lower case, followed by at least one character before the first `/`, `?` or `#`.

The relay URL is the base with `https://` replaced by `wss://`, or `http://` by `ws://`, followed by `/v1/ws`. The constructor also throws `invalid_message` unless that URL passes the `url` rule of §3.2.

Its `list` holds one entry:

|member|value|
|---|---|
|`id`|`"custom"`|
|`name`|the trimmed base URL|
|`url`|the relay URL|
|`prefixes`|all 31 symbols, in alphabet order|

`relayFor(code)` returns that entry for every valid code. `refresh()` does nothing and returns true.

### 3.2 Validation

The SDK uses a public list, fetched or embedded, only when it is valid by `SPEC.md` §5.2. It rejects an invalid list whole, and the current list stays.

A valid list is an object whose `relays` member is an array of at least 1 object. Each entry has these members:

- `id` is 1–32 characters from `a`–`z`, `0`–`9` and `-`. It doesn't start with `-`, and it is unique in the list.
- `name` is 1–64 code points, with no unpaired surrogate and no character that `SPEC.md` §2 forbids.
- `url` starts with `wss://` or `ws://` in lower case, and is non-empty between `://` and the next `/`. It ends with `/v1/ws`. The rest of it is printable ASCII, U+0021–U+007E, except `?` and `#`.
- `prefixes` is non-empty and holds symbols of `23456789ABCDEFGHJKMNPQRSTUVWXYZ`. No symbol appears twice in the whole list.

Every `url` in a list is `wss://`. A `ws://` URL passes only for `Relays.at` with an `http://` base. Unknown members are dropped, and only the four above are kept.

The vectors are in `relay-list.json`.

### 3.3 Refresh

A refresh is one GET of `https://openwatch.party/relays.json`, and nothing else. If the GET fails or returns an invalid list, nothing changes. Only `Relays.public()` refreshes.

A refresh starts in one of three ways:

- On `joined`, one starts in the background when this object has had no successful refresh yet, or its last success is at least 24 h old. Failed refreshes don't count. None starts while a background refresh is already running. A background refresh never delays `create` or `join`.
- Routing may force one and then wait for it, as §4.2 describes.
- `refresh()` forces one, and returns whether the list it got was valid.

A room stays on its relay for its whole life. A new list affects only later `create` and `join` calls.

### 3.4 HTTP

The SDK makes HTTP GETs only for refreshes. These rules apply to every GET, whether the SDK's own fetcher makes it or one the app supplies through §16.8:

- The GET is aborted after 5 s, body included, and the connection is closed. An app fetcher MUST stop on its cancel signal, and the SDK gives up at that point regardless.
- The body is at most 65536 bytes, and a longer one aborts or fails the GET. For an app fetcher, the limit applies to the returned text, measured as UTF-8 bytes.
- Redirects are not followed, so a 3xx response fails. App fetchers MUST NOT follow them.
- Only status 200 with a UTF-8 JSON body succeeds.

## 4. Room codes and routing

A room code tells the SDK which relay holds the room. This section defines how the SDK reads a code the user typed, and how it finds that relay.

### 4.1 Normalizing a code

`join` normalizes the code first, so that it can route by the first symbol. `relayFor` and `formatCode` share the same normalizer. It applies the relay's rule of `SPEC.md` §5.1:

1. Remove the separators. These are exactly U+0020, U+0009, U+000A, U+000D, U+00A0, U+2007, U+202F, U+002D, U+2010–U+2014 and U+2212.
2. Map ASCII `a`–`z` to `A`–`Z`, and nothing else.

The result MUST be 6 symbols of `23456789ABCDEFGHJKMNPQRSTUVWXYZ`. Otherwise the code is invalid, and the SDK reports `invalid_code` before any network use. `join` sends the normalized code.

`formatCode(code)` returns the normalized code, grouped for display as in `K7M-Q2X`. It throws `invalid_code` for an invalid code.

The vectors are in `code-routing.json`.

### 4.2 Routing

A code routes to the relay whose `prefixes` hold its first symbol. §3.2 makes that relay unique.

When no relay holds the symbol, the SDK forces a refresh as in §3.3 and routes again. If the refresh failed, it routes on the old list. When still no relay holds the symbol, the call fails with `room_not_found`.

A relay may answer a `join` with `room_not_found`. If this call hasn't forced a refresh yet, the SDK then forces one:

- If the symbol now maps to another `url`, the SDK joins there once, and that answer is final. Relays are compared by URL, not by id.
- Otherwise the `join` fails with the first `room_not_found`.

A `join` forces at most one refresh. With `Relays.at`, the one relay owns every symbol and nothing refreshes, so its `room_not_found` is final.

This holds for a `join` with `create` too. A relay answers it with `room_not_found` when it doesn't make codes with the code's first symbol, as after the symbol moved to another relay.

## 5. Clock sync

Each connection keeps its own estimate of relay time. This section defines how the SDK pings the relay, which answers it uses and how it picks the best sample.

There is one clock per connection, starting when its WebSocket opens. A connection never inherits another's clock. While the party reconnects, the dropped connection's clock stays in use, as §14 describes. The clock implements `SPEC.md` §16.2.

### Pings

Pings have the `id`s 1, 2, 3 and so on. The first three are the burst, sent at open, at open +500 and at open +1000. The rest follow at open +5000, at open +10000 and then every 5000. All these times are computed from the open, and the SDK records each send time on the local clock.

### Samples

The SDK ignores a `pong` whose `id` is unknown or already answered. The first `pong` for a ping clears that ping.

The round trip is `rtt = now − sentAt`. The SDK ignores the pong when `rtt` is below 0 or above 1000, and keeps a round trip of exactly 1000. Otherwise the pong gives a sample with `offset = serverTime − (sentAt + rtt / 2)`.

### The window

The samples go into a window. Before adding a sample, the SDK checks for a clock step. When the window isn't empty and `|offset − best.offset| > (rtt + best.rtt) / 2 + 100`, it clears the window first. A difference exactly at the limit keeps the window, and an ignored pong never steps.

The SDK then appends the sample and keeps the 8 newest. The oldest is evicted even when it is the best. The best sample is the one with the lowest rtt, and the oldest one on ties. `joined.serverTime` is unused.

The vectors are in `clock-sync.json`. For each pong they give `accepted`, `stepped` or `ignored`, and the best sample after it.

## 6. Choosing a relay for `create`

A `create` has no code to route by, so the SDK chooses the relay. It races the relays in the list and falls back to the next one when a relay is full.

### One relay

With one relay, which is always the case for `Relays.at`, the SDK connects and sends `create` at once, without waiting for a pong. A failed connection fails `create` with `unreachable`.

### Several relays

With several relays, the SDK connects to every relay at once, at t0. Each connection has its own clock and its own burst from its open, as §5 describes.

The first relay whose connection yields an accepted clock sample wins. By §5, an accepted sample is a `pong` with 0 ≤ rtt ≤ 1000. Pongs are handled in arrival order, and the first accepted one wins. Then:

- The SDK sends `create` on the winner's connection.
- It closes every other connection and attempt with status 1000.
- The winner's clock continues as the room's clock.

`create` fails with `unreachable` when no relay wins within the connect timeout of §2 from t0. It also fails with `unreachable` when every connection failed first, by being refused, HTTP 429 and 503 included, or by closing or erroring.

### Full relays

When a relay answers `relay_full`, the SDK closes that connection with status 1000. It then tries the first untried relay in list order: it connects and sends `create` at once, without waiting for a pong. When every relay has answered `relay_full`, `create` fails with the last `relay_full` error, including its `message` and `re`.

Any other refusal ends `create` with that refusal's code. A lost connection, or no answer within the connect timeout, ends it with `unreachable`. In both cases the SDK tries no more relays.

## 7. Session lifecycle

This section covers the party's connections, how `create` and `join` start a session, what the SDK does on `joined`, and how a party ends.

### 7.1 Connections

A connection must open within the connect timeout of §2, or the attempt has failed. A `joined`, or an `error` with a matching `re`, must answer within the connect timeout after the SDK sends its request. While the SDK waits, it ignores other `error`s. A connection that closes early has failed. For `resume`, the timeout covers the `joined` plus a first clock sample, as §14 says.

When a `create` or `join` fails in one of these ways, the call fails with `unreachable`. A failed reconnect attempt is retried, as §14 describes.

The SDK ignores unknown message `type`s and unknown members.

The party's constructor checks `name` and `client`, and throws `invalid_message` when either fails:

- `name` is 1–32 code points, with no forbidden character and no unpaired surrogate.
- `client` is 1–64 code points under the same rules. It contains a `/`, and its first `/` is neither its first nor its last character.

The vectors are in `names.json`.

All messages are valid UTF-8, as `SPEC.md` §3.1 requires. The SDK refuses unpaired surrogates in descriptor strings before sending, with `invalid_message` under §9.7. Objects it sends never repeat a member name, which `SPEC.md` §4 forbids. Commands are checked before sending, as §12 lists.

### 7.2 Create and join

`create()` chooses a relay as §6 describes, sends `{type: create, name, client}` and awaits `joined`.

`join(code, create)` runs these steps:

1. It normalizes the code. An invalid code fails with `invalid_code` before the `already_in_room` check, and before any change.
2. It routes the code by §4.
3. It connects.
4. It sends `{type: join, code, name, client}`. When `create` is true, it adds `create: true`. Otherwise the message has no `create` member.
5. It awaits `joined`, as §4.2 describes.

`create` is false by default. With it, the relay makes a room with the code when no room has it, and the party is that room's host, as `SPEC.md` §7.2 says. An app that brings a group back to a code it used before, such as one from a list of recent rooms, joins with `create`. A relay that predates `create` answers `room_not_found`.

A failed call carries the relay's error code, or one of the SDK's own. The relay's codes are an open set, such as `room_not_found`, `room_full`, `room_locked`, `rate_limited`, `client_blocked`, and `relay_full` for `create` and for `join` with `create`. The SDK's own are `invalid_code`, `unreachable` and `invalid_message`, the last for a malformed `joined` as §8 defines it.

#### Attempts

Both calls fail with `already_in_room` while an attempt is running, or while the party is `connected` or `reconnecting`. They work in `idle` and `ended`.

An attempt clears `endReason` and the away flag of §10.2, resets the state and publishes `connecting`. When it fails, the SDK closes its connections with status 1000 and returns to `idle`, without emitting `Ended`.

An attempt doesn't release the adapter, and neither does its failure or cancellation. Only the releases of §11.2 let an adapter go. So an adapter attached while `idle` or `ended` stays attached, with its `ref`, into the session the attempt starts.

#### Cancelling an attempt

`leave()` or `close()` during an attempt cancels it. The party goes back to `idle` without emitting `Ended`, and the pending call fails with `cancelled`.

The SDK closes the attempt's connections with status 1000, except a connection that has already sent `create` or `join`. That one is kept until its answer arrives or its timeout passes. A `joined` answer gets a `leave` and then a close, and an `error` answer gets a close, as in `SPEC.md` §11.5.

In Kotlin, cancelling the caller's coroutine cancels the attempt the same way. In Swift, cancelling the calling task does too, and the call throws `CancellationError`.

### 7.3 On `joined`

On `joined`, the SDK goes through these steps in order:

1. It keeps what a resume under §14 needs: the relay URL, the code, the peer ID and the token.
2. It accepts the snapshot as the `room` baseline of §8, at any `rev`.
3. It handles the media and the epoch by §8–§9. After a reconnect, it does so only if the epoch changed, as §14 says.
4. It sends `status` as §10 describes.

After a reconnect, the SDK also compares the participants with those from before the drop, for the events of §15. It then handles, in order, the messages that arrived before it switched to this connection.

### 7.4 Ending

A party ends for one of these reasons:

|reason|cause|
|---|---|
|`left`|the app called `leave()`. When connected, the SDK sends `leave` and closes the connection with status 1000.|
|`ended`|`closed` with reason `ended`, or with a reason the SDK doesn't know|
|`kicked`|`closed` with reason `kicked`|
|`expired`|`closed` with reason `expired`|
|`replaced`|the relay closed the connection with status 4000, because a resume elsewhere took over. Never resumed.|
|`lost`|the party can't go on: resume became impossible (§14), or a `room` starting a new epoch had no valid `media` (§8). The error is attached.|

A `leave` call while reconnecting ends the party with `left` and sends nothing. In `idle` or `ended`, it does nothing. The `end()` command of §12 ends the party through the relay's `closed`. After a `closed`, the SDK closes the connection with status 1000, since the relay leaves it open.

A `lost` ending carries one of these errors:

|error|when|
|---|---|
|`unreachable`|the resume window passed|
|`resume_failed`, `client_blocked`|the relay refused the resume|
|`invalid_message`|a new epoch came without a valid `media`. The SDK sends `leave` first.|

#### How a party ends

The SDK ends a party in these steps:

1. It releases the adapter as §11.2 describes.
2. It closes the connection and any attempts.
3. It stops the timers.
4. It cancels matching.
5. It resets the room fields.
6. It publishes `ended` and the `endReason`.
7. It then emits `Ended`.

An ended party may start over.

#### Closing

`close()` does what `leave()` does and then releases everything. It is idempotent. The release differs by language:

|language|what `close()` releases|
|---|---|
|TypeScript|It clears the listeners.|
|Dart|It closes the streams after a queued `Ended`.|
|Kotlin|It cancels its scope after attempts are answered and the rate is restored.|
|Swift|It finishes the `states()` and `events()` streams after a queued `Ended`. Later calls return finished streams.|

`close()` returns the release completion of §11.2. Afterwards, `create` and `join` throw the language's state error, not a party error. That is `IllegalStateException` in Kotlin, `StateError` in Dart, `Error` in TypeScript and `PartyClosedError` in Swift.

## 8. Room state

The relay sends the room's state as snapshots. This section defines how the SDK checks relay input, when it accepts a snapshot, how a new epoch starts, and how the SDK derives the host, the controller and the position.

The SDK checks all relay input before it changes any state, and input that fails changes nothing. The SDK ignores non-objects, a `type` that isn't a string, and unknown `type`s.

### Malformed snapshots

A snapshot is a `room` message or `joined.room`. It is malformed if it fails `snapshot.schema.json`, which is when any of these holds:

- It is not an object.
- One of `rev code epoch phase positionMs atMs rate control locked waitingOn participants` is absent. Nullable members must be present too.
- It breaks one of the rules below.

In these rules, an "int" is a JSON number that is integral as a double and within ±(2^53 − 1). So `42.0` is an int, while strings, booleans, null and `1e400` are not. The rules come from `SPEC.md` §8.2, §9.1 and §13.1:

|member|rule|
|---|---|
|`rev`|an int ≥ 1|
|`code`|matches `^[23456789ABCDEFGHJKMNPQRSTUVWXYZ]{6}$`|
|`epoch`, `atMs`|ints ≥ 0|
|`phase`|`idle`, `paused`, `waiting` or `playing`|
|`positionMs`|an int from 0 to 604800000, and 0 when `phase` is `idle`|
|`rate`|a number from 0.5 to 2.0|
|`control`|`host` or `everyone`|
|`locked`|a boolean|
|`waitingOn`|distinct strings matching `^[A-Za-z0-9_-]{1,16}$`, non-empty only in `waiting`|
|`action`|in a `room` only: absent, or an object as described below|
|`participants`|non-empty, with exactly one entry whose `host` is true, and each entry as described below|

An `action` object has a `kind` that is a non-empty string, an open set. Its `by` is present, and either null or a peer ID.

Each entry of `participants` follows these rules:

|member|rule|
|---|---|
|`peerId`|matches the peer ID pattern `^[A-Za-z0-9_-]{1,16}$`|
|`name`|1–32 code points, with no forbidden character and no unpaired surrogate|
|`client`|1–64 code points under the same rules, and non-empty on both sides of its first `/`|
|`host`, `connected`|booleans|
|`player`|`idle`, `loading`, `ready` or `buffering`|
|`match`|present, and either null or a match object as described below|
|`offsetMs`|an int within ±604800000|

A match object has a `state` of `searching`, `matched`, `mismatched` or `unavailable`. Its `deltaMs` is absent or an int.

Unknown members are ignored. The check leaves these unchecked:

- whether peer IDs are unique
- whether `waitingOn` and `action.by` name participants
- `positionMs` against the duration
- `code` against the party's own code
- a `room`'s `media`, which is checked only when its epoch is new, as described below

### Accepting a room

The SDK drops a `room` that is malformed, or whose `rev` doesn't increase. It compares `rev` per connection. An accepted `room` replaces the snapshot whole.

An accepted `room` whose `epoch` differs from the party's epoch must carry `media`, either null or valid by §9.7. Otherwise the SDK sends `leave`, closes the connection with status 1000, and ends the party as `lost` with `invalid_message`.

With such a `media`, the SDK goes through these steps in order:

1. It replaces the snapshot.
2. It sets the media and the epoch.
3. It starts the new epoch as §9.1 describes.
4. Only then does it emit the snapshot's events (§15) and step (§11.1), as for `joined` in §7.3.

A `media` member on a `room` of the party's own epoch is ignored, unchecked.

### Malformed `joined`

A `joined` is malformed when any of these holds:

- `code` is not a valid room code.
- `you` is not an object, or `you.peerId` is not a valid peer ID.
- `you.token` doesn't match `^[A-Za-z0-9_-]{43}$`.
- `serverTime` is not an int ≥ 0.
- `room` or `media` is malformed.

The SDK handles a malformed `joined` according to what it answers:

- For `create` or `join`, the SDK sends `leave`, closes the connection with code 1000 and fails the call with `invalid_message`. There is no fallback.
- For a reconnect's `resume`, the SDK closes with code 1000 without sending `leave`, and the attempt has failed (§14).

### Other messages

The SDK ignores other messages that fail their schema:

- A `pong` needs `serverTime` to be an int ≥ 0. A bad `pong` gives no clock sample (§5–§6).
- An `error` needs `code` and `message` to be strings, with `code` non-empty, and `re` present as null or a string.
- A `closed` needs a non-empty `reason`.

### Epoch and roles

The party's epoch is the `epoch` of the last `joined` or accepted `room` it processed. Epochs are compared only for equality, never for order.

The party's own entry is the first entry in `participants` with its peer ID. A snapshot may have none. The host is the entry whose `host` is true. The party is a controller when it is the host, or when its own entry is present and `control` is `everyone`.

### Position

The room position at relay time t is `P(t)`, as `SPEC.md` §12.2 defines it, with the current `durationMs`. While `playing`, it is `min(durationMs, floor(positionMs + max(0, t − atMs) × rate))`. The product is its own double, and the floor applies to the sum. In any other phase, `P(t)` is `positionMs`, uncapped.

The room is running when it is `playing` and `t ≥ atMs`.

`roomPositionMs()` returns `P(now)`, without the offset. It returns null when there is no media, no snapshot or no clock sample.

The vectors are in `timeline.json` and `room-check.json`.

## 9. Matching

When the room gets new media, each participant needs its own copy of the title. This section defines when matching runs, how the SDK ranks and walks the resolver's candidates, what each outcome sets, and which descriptors the SDK accepts.

### 9.1 When matching runs

Matching runs on every new epoch. A new epoch comes with:

- an accepted `room` of another epoch, as §8 describes
- a `joined` answering `create` or `join`
- a reconnect's `joined`, only when its epoch differs, as §14 says

Each new epoch first cancels any matching, which means the resolver call and probes in flight, and ignores their results. It sets the offset of §13 to 0, sets `choice` and `mismatch` to null and empties `candidates`. What follows depends on the media.

#### No media

When the media is null, `match` becomes null. The adapter is unbound and, when one is attached, paused once, as §10.2 describes. Unbinding doesn't discard queued commands, and the pause queues behind them.

#### Own media

The media is the party's own when the descriptor equals the one this party last sent in `setMedia`. The two are compared as parsed JSON, with unknown members outside `ext` dropped and member order ignored. For own media:

- `match` becomes `{"state": "matched", "deltaMs": 0}`.
- `choice` becomes a candidate with `id` `""`, that `setMedia`'s `ref` and the descriptor's `durationMs`.
- The attached adapter binds at once when its ref equals that `ref`, by the binding rule of §10.2.
- Nothing is paused, and the resolver isn't called.

The choice persists through the epoch.

"Last sent" is recorded at the `setMedia` call, and kept if the relay refuses it. It survives `clearMedia` and others' media, and resets only at the party's end. Equality is by value, so another host's identical descriptor is own media again.

#### Other media

For any other media, the adapter is unbound and, when one is attached, paused once. `match` becomes `{"state": "searching"}`. The SDK then calls the resolver, ranks the candidates by §9.4 and walks them by §9.5.

#### Searching again

`resolveAgain()` searches again for the current epoch. It does nothing without media, or when the current epoch is the party's own media, as decided when the epoch started. Otherwise it cancels matching in flight, meaning the resolver call and probes. It never touches the offset.

With `choice` null, it reruns the other-media steps as on a new epoch, minus the offset reset and minus any adapter action, since nothing is bound. `match` becomes `searching`, `mismatch` becomes null, and `candidates` stays empty until the resolver answers. Then the walk runs.

With `choice` set, it runs only the resolver and the ranking of §9.4, and replaces `candidates` with the result. A `deltaMs` there comes from a counting `durationMs` only, and is null otherwise. There are no probes and no walk. `choice`, `match`, `mismatch`, the binding and the adapter stay unchanged, and the app offers the new list for `choose`.

`ResolveFailed` and the resolve timeout apply in both cases. A failed call leaves `candidates` empty.

### 9.2 Resolver and candidates

#### The resolver

`resolve(media)` returns the app's candidates in its order of preference. The SDK calls it once per search, and cancels it as §9.1 says.

A call that throws, or takes longer than the resolve timeout of §2, means no candidates. On a timeout the SDK also cancels the call. In both cases the SDK emits `ResolveFailed`, with the thrown error or the language's timeout error from §16.8. A cancelled call emits none. In TypeScript, an answer that isn't an array means no candidates, without `ResolveFailed`.

A party without a resolver never searches. That is a party constructed without one, as §16.2 allows. For any other participant's media, its `candidates` is empty and its `match` is `unavailable` from the start of the epoch. So its first `status` there already says `unavailable` (§9.6, §10.3).

#### Candidates

A candidate, the `Candidate` type of §16.5, has these members:

|member|what it is|
|---|---|
|`id`|a unique ID|
|`ref`|the app's reference to the file, handed back with `choice`|
|`durationMs`|optional: the file's duration|
|`sizeBytes`, `oshash`, `name`|optional hints|
|`probe`|optional and async: opens the file, returns its duration or nothing, and closes it|

A `probe` runs only when there is no counting `durationMs`. The SDK cancels probes it no longer needs, and these then release what they opened.

A probe that throws has returned nothing. That holds even when what it throws is a cancellation of its own, such as the probe's own timeout, while the SDK hasn't cancelled it.

A duration counts only as an integer from 1 to 604800000, and any other value counts as none. A candidate with a bad `durationMs` is probed if it can be, and a bad probe answer counts as a returned nothing. A candidate with no duration and no `probe` is never chosen.

#### Identity

Identity is the resolver's job, for every kind. The resolver returns the files it takes for this title, and the `sameTitle` of §9.3 helps it decide.

The SDK never applies a same-title or same-file rule while matching. The ranking of §9.4 orders the candidates, and the walk's duration tolerance of §9.5 decides.

A `media.ext` member under the app's own name MAY shortcut the search. A member naming a known server and item is looked up there, as §9.7 allows.

### 9.3 Same title

`sameTitle(a, b)` implements this rule for resolvers that compare descriptors. It is symmetric, and `SPEC.md` §16.5 points here.

Two descriptors are the same title only when their `kind`s are equal. Then the rule depends on the kind:

- `movie`: some namespace of `imdb`, `tmdb`, `tvdb`, `plex`, `anilist`, `anidb`, `kitsu` and `mal` has the same non-empty value in both `ids`. The value must be in the same namespace in both and match exactly. Other namespaces don't count.
- `episode`: the rules below apply in order.
- `video`: both have a `file`. Either their `oshash`es are equal ignoring ASCII case, or `sizeBytes` is present and equal in both and their `name`s are equal ignoring ASCII case. IDs don't count.

Two episodes are the same title by the first of these that applies:

1. If their own `ids` share an ID as for `movie`, they are the same title. A shared episode ID beats differing numbers.
2. If some namespace of that list has different non-empty values in the two own `ids`, they are not. Differing episode IDs beat equal numbers.
3. Otherwise they are the same title when both have a `series`, the two `series` share an ID as for `movie`, and `season` and `episode` are present in both and equal.

The vectors are in `same-title.json`.

### 9.4 Ranking

The SDK sorts the candidates stably by these keys, in order. The first three compare a candidate's hints with the descriptor's `file`, and candidates with an equal value sort first.

1. `oshash` equal, ignoring ASCII case
2. `sizeBytes` equal
3. `name` equal, ignoring ASCII case
4. a counting `durationMs` before none
5. smaller `|durationMs − media.durationMs|`

Full ties keep the resolver's order. The keys apply to every kind and drop no candidate. Without a `file`, all candidates tie on the first three keys.

### 9.5 The walk

The walk goes through the candidates in ranked order and finds a duration for each:

- A counting `durationMs` is the duration.
- Otherwise the SDK probes, unless 4 probes have counted or one has missed its deadline.

#### Probes

Probes run one at a time and share 60000 ms. A probe's deadline is the budget left at its start, inclusive. Its time is measured on the local clock and spent from the budget. The SDK cancels it at the budget left + 1 ms.

A probe that misses its deadline is cancelled and not counted. It gives no delta, and no further probes run. A failed, empty or non-counting probe skips its candidate and counts toward the 4. Candidates with a counting `durationMs` are still checked after the probes run out.

#### Matching a duration

For each duration the SDK computes `delta = duration − media.durationMs`. A candidate with `|delta| ≤ 1500` matches and stops the walk. The first match in ranked order wins, even over a closer later one. Otherwise the SDK keeps the closest candidate so far, the earlier one on ties.

#### Published candidates

`candidates` is empty until the resolver answers. Then it holds all candidates, ranked, as `Ranked {candidate, deltaMs}`. A counting duration gives its `deltaMs` from the walk's start. A probed candidate gets one on a timely counting answer. Otherwise `deltaMs` is null.

The list survives the outcome, `acceptMismatch`, `declineMismatch` and `choose`. Only a new epoch or `resolveAgain()` replaces it, as §9.1 says.

The vectors are in `matching.json`.

### 9.6 Outcomes

The walk ends in one of three outcomes:

|outcome|`choice`|`match`|`mismatch`|
|---|---|---|---|
|a candidate matched|set to it|`{"state": "matched", "deltaMs": delta}`|null|
|none matched, but some had a duration|null|`{"state": "searching", "deltaMs": delta}`|`{candidate: closest, deltaMs: delta}`|
|none had a duration|null|`{"state": "unavailable"}`|null|

#### Answering a mismatch

A pending `mismatch` waits for the user. Besides `choose` below, two calls answer it:

- `acceptMismatch()` sets `choice` to the offer and `match` to `{"state": "mismatched", "deltaMs": delta}`. It clears `mismatch` and leaves the offset.
- `declineMismatch()` sets `match` to `{"state": "unavailable", "deltaMs": delta}` and clears both `choice` and `mismatch`.

Both do nothing without a pending `mismatch`. Binding follows §10.2: an outcome binds an attached adapter whose ref equals the new `choice.ref`.

#### Choosing a file

`choose(candidate)` takes a file from `candidates`, or from anywhere else the app finds one. It works in any matching state while media is present, own media included. Without media, it returns false.

The duration comes from a counting `durationMs` as §9.2 defines it, or from one probe outside the walk's budget and count, with no deadline. `choose` returns false, changing nothing, in these cases:

- The candidate has neither.
- The probe fails, returns nothing or returns a non-counting duration.
- The call is superseded, or matching is cancelled.

The latest `choose` wins and cancels earlier probes, whose answers are ignored. Cancelling matching does the same.

With a duration, and while the call is still current, `choose` goes through these steps:

1. It cancels the walk and the mismatch prompt.
2. It sets `choice` to the given candidate, also when it has the current choice's `id`, so a refreshed `ref` replaces the old one.
3. It sets `match` to `matched` when |delta| ≤ 1500, or else to `mismatched`, with `deltaMs`.
4. It leaves the offset unchanged and returns true.

Binding follows §10.2: an attached adapter whose ref differs from the new `choice.ref` unbinds, without a pause.

### 9.7 Descriptors are untrusted

A descriptor comes from another participant through a relay. Every string in it, and all of `ext`, is untrusted.

#### Senders and resolvers

Senders MUST NOT put anything that locates or fetches media in any field, `ext` included, as `SPEC.md` §10 says. Nobody checks this.

Resolvers MUST treat descriptors as search terms only. A descriptor value is never a path, URL, host, shell or SQL fragment, or format string. These rules also apply:

- `file.name` is only compared with names the app found itself, and never opened.
- Any `ext` member can be forged. Use it only to look up the app's own library or catalogue, never to choose where to connect or what to fetch.
- Apps show titles and names as plain text only.

#### Checks

The SDK checks the descriptors it sends, through `checkMedia` and `setMedia`, and those it receives in `room` and `joined`. A valid descriptor follows all of these rules:

- It is a JSON object.
- `kind` is exactly `movie`, `episode` or `video`.
- `series` is present for `episode`. For other kinds it is optional, and checked when present.
- `title` is required, and so is `series.title` within `series`. Each is at least 1 code point, with no forbidden character and no unpaired surrogate.
- `year` and `series.year` are ints within ±(2^53 − 1). `season`, `episode` and `file.sizeBytes` are ints ≥ 0.
- `durationMs` is required, and an int from 1 to 604800000.
- `ids` and `series.ids` are objects with non-empty string values, and any member names.
- `file.oshash` matches `^[0-9a-f]{16}$`.
- `file.name` is at least 1 code point, with no forbidden character and no unpaired surrogate. It contains no `/` or `\`, and is not `.` or `..`.
- `ext` is an object whose member names match `^[a-z0-9-]+(\.[a-z0-9-]+)+$`. Its values can be any JSON, and nested names are free apart from surrogates.
- `series` and `file`, when present, are objects. A wrong type fails, and null counts as a wrong type for members that aren't nullable.
- No unpaired surrogate appears in the member names and values of `ids` and `series.ids`, or in any `ext` string, member names included.
- Unknown members outside `ext` are ignored: they are not checked, not kept and not sent.

#### Size limits

Sending adds two limits: the descriptor is at most 4096 bytes, and `ext` at most 1024. Both are measured on the SDK's compact JSON of its `Media`, with `ext` measured alone. That JSON:

- holds known members only, and omits absent ones
- writes ints as integral numbers
- has no whitespace
- writes non-ASCII characters as UTF-8

It is the exact text `setMedia` sends, so a passing descriptor stays within the relay's limit, which the relay measures as received.

Empty `ids` and `series.ids` may be omitted when sending, and receivers treat absent and empty alike. Receipt skips the sizes, since the relay measured them as received.

#### Failures

When a descriptor fails before sending, `checkMedia` returns the first rule it breaks. `setMedia` throws `invalid_message` and sends nothing.

When a received descriptor fails, it makes a `joined` malformed. On a new epoch's `room`, it ends the party as §8 describes.

The vectors are in `media-check.json`.

## 10. Status

Each participant tells the room how its player is doing with `status` messages. This section defines the fields, how the SDK derives the player state, and when it sends.

### 10.1 Fields

A `status` carries these fields:

|field|value|
|---|---|
|`epoch`|the epoch (§8)|
|`player`|§10.2|
|`match`|§9, or null while the room has no media|
|`offsetMs`|the offset (§13)|
|`rttMs`|the clock's best round trip, absent before the first sample|

`rttMs` is read at send time and never compared. A change of `rttMs` alone never sends a `status`.

### 10.2 Player state

Four facts decide `player`: whether an adapter is attached, whether the party is away, and whether the adapter is bound and blocked. Binding and blocking are described below.

Away is set by `detachPlayer`, unless that call names an adapter that isn't attached. It is cleared by `attachPlayer`. It is false from the start of every `create` or `join` attempt of §7.2 and after the end, and it is kept across a reconnect.

#### Attaching and detaching

`attachPlayer(player, ref)` attaches `player` as the player of `ref`. The `ref` is that of the file the player has open. Normally that is `state.choice.ref` exactly as published, or, as host, the `ref` passed to `setMedia`.

Another attached adapter is released first, as §11.2 describes. The adapter already attached keeps its lane and queued commands, and takes the new `ref`. `attachPlayer` clears away, re-evaluates binding and steps as §11.1 says.

`attachPlayer` works while `idle` or `ended` too, and an adapter attached then stays attached into the next session (§7.2).

The app attaches once per file it opens. It attaches again after a release while the player still has that file open. The party's end releases the adapter, as §7.4 says, so an app that keeps its player open after `Ended` attaches it again, before or after the next `create` or `join`.

`detachPlayer(player?)` without an argument releases the attached adapter as §11.2 describes, unbinds and sets away. It works with no adapter attached, and it doesn't pause the adapter. With an argument, it does so only when that adapter is the one attached, and otherwise changes nothing. Both forms return the release completion of §11.2.

#### Binding

Bound is a condition, re-evaluated whenever one of its inputs changes. The adapter is bound when all of these hold:

- An adapter is attached.
- `choice` is non-null, and its `ref` equals the attached ref. Kotlin and Dart compare with `==`, Swift with `==` of `Ref: Equatable`, and TypeScript with `===`.
- `match.state` is `matched` or `mismatched` for the current epoch.
- No `setMedia` of this party is pending.

Every new epoch unbinds first, then re-evaluates, as §9.1 says.

When the condition turns true, the adapter binds. Nudging is off, and the applied rate becomes the room rate. When the adapter can set rates, one `setRate` of that rate is queued. Afterwards the SDK tracks the rate it last set.

When the condition turns false, the adapter unbinds. The play timer of §11.2 stops, and queued commands stay.

So the SDK never commands a player that holds another file. An open that finished late for a superseded choice attaches without binding. An attached player already playing `choice.ref` binds as soon as matching publishes that choice.

#### Pending setMedia

`setMedia` is pending from the call until one of these arrives: an accepted `room` with a new epoch, a `joined` with a new epoch, or an `error` with `re` `setMedia`. The call unbinds without a pause. A reconnect's `joined` with the same epoch sends it again or ends it, as §14 describes.

When it stops being pending, the SDK re-evaluates binding and steps. So a host may call `setMedia(media, ref)` and `attachPlayer(player, ref)` in either order.

#### Blocked

Blocked means the player refused to start. It turns true when a `play` the SDK issued on the bound adapter throws and the adapter then reads `paused`. A browser does this when its autoplay policy wants a user gesture. When blocked turns true, the SDK emits the `PlayBlocked` event of §15.

Blocked turns false when the adapter's `paused` reads false, on unbinding, and when the room's `phase` isn't `playing`.

The step of §11.2 still issues `play`. The app asks the user to start the player with the player's own control, which a browser counts as a gesture.

#### The reported value

`player` is the value of the first rule that applies:

1. With no media, it is `idle`.
2. When `match.state` is `unavailable`, it is `idle`.
3. When away, it is `idle`.
4. When not bound, it is `loading`.
5. When bound and the first frame isn't shown, or a seek the SDK issued is queued or running, it is `loading`.
6. When bound, the room is `playing` and relay time is unknown, it is `loading`.
7. When held (§11.2) and `|position − holdAt| > max(500, seek accuracy)`, it is `loading`.
8. When blocked, it is `loading`.
9. When buffering, it is `buffering` if the room is running as §8 defines it and not held, and `loading` otherwise.
10. Otherwise it is `ready`.

Here `ready` means what `SPEC.md` §7.5 and §16.4 define: able to play at once, and close enough that following needs no seek. An app that takes part without playing calls `detachPlayer()` after each join.

### 10.3 When to send

The SDK sends `status` at these times:

- right after every `joined`, with the last-sent values cleared
- when `epoch`, `player`, `match` or `offsetMs` differs from the last `status` sent on this connection
- 15000 ms after the last `status`, a heartbeat that every send restarts

At most one `status` goes out per 250 ms. A change inside the spacing goes out when the 250 ms are up, with the values of that moment, if they still differ. A `status` whose `epoch` differs from the last one sent skips the spacing.

No `status` is sent while reconnecting, and the status timers stop at the drop. The SDK reads the adapter on every tick of §11.1, on `playerChanged()` and once the attached adapter's lane is empty.

The first `status` of a new epoch decides this participant's part in the room's wait:

- `idle` takes it out of the wait.
- `loading` keeps the room waiting.
- `ready` stops it holding the room.

A user who won't open the new media is taken out of the wait by `detachPlayer()` or `declineMismatch()`, as soon as the app knows.

## 11. Following the room

While an adapter is bound, the SDK keeps the player in step with the room. This section defines when the SDK steps, what a step does, and how it releases an adapter.

### 11.1 When the SDK steps

A step runs only while an adapter is bound, media is present and relay time is known. It runs every 500 ms on the tick, which starts at the first `joined` and runs until the party ends, also while reconnecting.

A step also runs at once after any of these:

- an accepted snapshot
- `attachPlayer`
- the attached adapter becoming bound by §10.2, through a match, `choose`, `acceptMismatch` or the own media's `room` setting the choice to its `ref`
- `setOffset`
- `playerChanged()`
- the end of a command hold
- the play timer firing

#### Command lanes

Each adapter has its own command lane. Its commands run one at a time, in order, and each is awaited. A failing command is ignored, since the next step corrects it. Lanes of different adapters never wait for each other.

No step starts while a command on the attached adapter's lane is queued or running. A step requested meanwhile is remembered as one flag, and runs once that lane's last queued command finishes. Several requests become one step.

No step runs during a command hold of §12, and the hold's end runs one.

#### Inputs

A step reads these inputs:

- the room's `phase`, `positionMs`, `atMs` and `rate`
- the media's `durationMs`
- relay time now
- the offset of §13
- the adapter's position, `paused`, `buffering`, `durationMs`, seek accuracy and whether it can set rates
- the applied rate
- whether the SDK is nudging
- the milliseconds since the SDK last issued a seek on this adapter, on the local clock, or null when it never has

### 11.2 The step

A step compares the player with the room, and queues the commands that bring it back in line:

```
if phase is idle: no action; nudging = false; stop.
a      = the adapter's seek accuracy
cooled = the SDK never seeked on this adapter, or issued its last seek ≥ 2000 ms ago
end    = adapter.durationMs − 1000 (at least 0) when the adapter knows a duration > 0, else none
clamp(x) = max(0, x), and at most end when end exists
anchor = positionMs + offsetMs                        # local position at atMs

held = phase is paused or waiting
if phase is playing:
    startAt = atMs + (anchor < 0 ? −anchor / rate : 0) # relay time when local playback starts (double)
    if now < startAt: held = true; playIn = ceil(startAt − now)

if held:
    holdAt = clamp(anchor)
    pause                        if the adapter isn't paused
    setRate(rate)                if it can set rates and |applied − rate| > 0.001
    seek(holdAt)                 if |position − holdAt| > max(500, a) and cooled
    nudging = false; start the play timer for playIn when set; stop.

target   = max(0, P(now) + offsetMs)
nudging  = nudging and the adapter can set rates
if end exists and target ≥ end and position ≥ end:    # both at the local file's end
    setRate(rate) if it can set rates and |applied − rate| > 0.001; nudging = false; stop.
if the adapter is buffering: no action; stop.

drift = position − target
seek  = false
with rate control:
    |drift| > max(2000, a):  nudging = false; seek = cooled
    else if nudging:         nudging = (|drift| ≥ 150)
    else if |drift| > 350:   nudging = true
without rate control:
    |drift| > max(1000, a):  seek = cooled
want = nudging ? rate × (drift > 0 ? 0.96 : 1.04) : rate
setRate(want)                if it can set rates and |applied − want| > 0.001
seek(clamp(target + 250))    if seek
play                         if the adapter is paused
```

Here `P(now)` is the room position of §8, and `rate` is the room's. Each product and quotient is its own double, as §2 requires. A `setRate` action's rate is rounded to 6 decimals, and the applied rate becomes that rounded value.

Actions run in the order pause, setRate, seek, play, each queued as §11.1 says. Issuing a seek records its time, for `cooled`. From then until it finishes, the seek counts as in flight for §10.2. Step seeks never go past the end margin.

#### Seek accuracy

`a` is the adapter's `seekAccuracyMs` of §16.4, read at every step. It says how far from the requested position a seek may land, such as a player's keyframe distance. A value that isn't an integer ≥ 0 counts as 0.

`a` raises every seek threshold, here and in §10.2. The nudge thresholds, 350 and 150, and the seek lead don't change. A player that can't seek at all declares a very large value, and is followed by rate only.

#### The play timer

A step that holds with `playIn` starts a one-shot play timer for `playIn` ms, which runs a step. Each step replaces or cancels the timer, and unbinding cancels it. Its delay is clamped as §2 says.

#### Releasing an adapter

Three things release an adapter: `detachPlayer`, `attachPlayer` with another adapter, and the party's end. On release:

- The adapter's commands that haven't started are dropped.
- A command already running finishes on its lane, and its outcome is ignored.
- If the SDK was nudging, one `setRate` of the room rate, captured at that moment, is queued on the released lane. It runs, also after `close()`.
- Nothing else reaches that adapter.

`detachPlayer` and `close()` return a completion, whose type §16.8 gives per language. It settles, never failing, once no released adapter has a command running or queued, and at once when none has. An adapter command must finish or throw, and until it does, the completion waits. An app awaits the completion before disposing a player.

The vectors are in `reconcile.json`.

## 12. Local commands

The user's controls go through the party. A change made behind the party's back is undone by the next step.

Positions the app passes, and those it reads from its adapter, are in its own file's timeline. The SDK subtracts the offset of §13 before sending.

Commands don't wait for the relay. A refusal arrives as an `Error` event of §15. The SDK doesn't undo the local effects. The next step does. `setOffset` is a local setting of §13, not a command.

Each command checks its preconditions, applies its local effect and sends one message:

|method|checks before sending|local effect|sends|
|---|---|---|---|
|`play()`|member, media, controller|none: the room starts everyone (§11)|`play`|
|`pause()`|member, media, controller|the adapter pauses|`pause`, `positionMs` = max(0, adapter position − offset) when bound, else absent|
|`seek(ms)`|member, media, controller, `ms` an int ≥ 0, `ms − offset` and the sent position ints (§2)|the adapter seeks to `ms`|`seek`, `positionMs` = max(0, `ms` − offset)|
|`setRate(rate)`|member, media, controller, 0.5 ≤ rate ≤ 2.0|the adapter takes `rate` when it can|`rate`|
|`setMedia(media, ref, positionMs = 0)`|member, host, descriptor, `positionMs ≥ 0`|marks the descriptor as the party's own, with `ref` as its file (§9.1), and unbinds the adapter until its epoch arrives (§10.2)|`setMedia`|
|`clearMedia()`|member, host|none|`clearMedia`|
|`setControl(mode)`|member, host|none|`setControl`|
|`transferHost(peerId)`|member, host, a participant with that peer ID|none|`transferHost`|
|`kick(peerId)`|member, host, a participant with that peer ID|none|`kick`|
|`lock(locked)`|member, host|none|`lock`|
|`end()`|member, host|none|`end`|

### Checks

The checks run in a fixed order. For `play`, `pause`, `seek` and `setRate` it is member, then media, then controller, then values. For the other commands it is member, then host, then the descriptor, value or participant.

Local effects touch the adapter only while it is bound. Some commands have details of their own:

- A `seek` command's local seek is unclamped. It counts as an SDK seek for the cooldown and for `loading`.
- `pause` also checks its computed `positionMs`, as §2 requires.
- `setRate` rejects NaN.
- `setMedia` sends `positionMs` as given, since the new media's timeline is the host's own file.

A participant check accepts connected and disconnected entries, and doesn't refuse the party itself.

A failed check throws without sending and before any local effect. The error tells which check failed:

- `not_in_room` means the party isn't connected, which includes reconnecting.
- `forbidden` means the party isn't the host, or isn't a controller as §8 defines it.
- `invalid_message` covers no media, and a value out of range or not an int. It also covers a computed wire value that isn't a valid int, a peer ID not in the room, and a descriptor failing §9.7.

### The command hold

After `pause`, `seek` or `setRate`, bound or not, a command hold starts. No step runs until one of these happens:

- The next accepted `room` carrying an `action` of §8 arrives.
- An `error` arrives with `re` `pause`, `seek` or `rate`, matching the command.
- 2000 ms pass.

The hold keeps snapshots already in flight from pulling the player back. A new command replaces the hold, and the hold's end runs a step. There is never a hold after `play`, `setMedia` or the host commands.

## 13. Offset

The offset shifts the user's file against the room. The local position is the room position plus the offset, as `SPEC.md` §16.4 defines.

A positive offset means the user's file is ahead, with extra footage before the same scene, such as a longer intro.

`setOffset(ms)` takes an integer from −604800000 to 604800000 inclusive. Anything else throws `invalid_message` and changes nothing. It needs neither a room nor media, and it works while idle or reconnecting. But a value set outside a room is reset by the next `joined`.

The offset becomes 0 on every new epoch, as §9.1 says. A resumed `joined` with the same epoch keeps it, and so does `resolveAgain()`.

Apps that allow mismatched files SHOULD offer the offset control, with steps of 100 ms and 1 s, a reset, and the mismatch's `deltaMs` as a hint.

## 14. Reconnect and resume

A dropped connection doesn't end the party. The SDK reconnects and resumes the session for as long as the resume window allows.

### Drops

The member connection closing or failing is a drop. Every close status other than 4000 counts, including 1001, 1007 and 1009 from `SPEC.md` Appendix B. So does a close without a status.

After `closed` and after `leave()`, the SDK has closed the link itself, so it sees no drop. Status 4000 ends the party as `replaced`, a reason of §7.4.

When no message has arrived for 15 s, the SDK closes the connection with status 1000 and treats it as a drop with no status. Every received message, `pong` included, re-arms this timer.

### Attempts

On the drop, `connection` becomes `reconnecting`, the status timers stop, the window timer starts and attempt 1 is scheduled.

Attempt n waits floor(min(8000, 500 × 2^(n−1)) × (0.5 + 0.5u)) ms, with u one draw from the injectable source of §16.8. Attempt 1 waits from the drop, and attempt n from the failure of attempt n−1.

Each attempt connects to the URL the party joined on, starts a new clock and sends `{type: resume, code, peerId, token}`. An attempt fails on any of these:

- a failed connection, HTTP 429 and 503 included
- an open timeout
- no answer in time, as §7.1 defines it
- the connection closing before the switch
- a malformed `joined` (§8)
- any `error` other than `resume_failed` or `client_blocked`

After a failed attempt, the SDK closes that connection with status 1000 and schedules the next attempt.

### Outcomes

The resume window of §2 is 300000 ms, measured from the drop. When it passes, the party ends with `lost` and the error `unreachable`.

On `joined`, the party is connected again and §7.3 applies. The media is handled as a new epoch only when its epoch changed.

On `resume_failed` or `client_blocked`, the party ends with `lost` carrying that error. It never joins again on its own.

### A pending setMedia

A `setMedia` still pending (§10.2) when a reconnect's `joined` has the epoch from before the drop never reached the room. If that snapshot lists this party as host, the SDK sends the same `setMedia` again right after the `status` of §7.3, and it stays pending. Otherwise it stops being pending.

### While reconnecting

While reconnecting, the SDK keeps stepping against the last snapshot and the old clock. It switches to the new connection's snapshot and clock together, once that connection has both `joined` and a clock sample. Commands throw `not_in_room`, as §12 says.

When the party ends while a resume attempt is under way, a connection that already sent `resume` is kept until its answer arrives or the connect timeout passes. A `joined` is then answered with `leave`, as §7.2 treats a cancelled `create` or `join`.

## 15. Events

The party reports what happens through events, and everything else through its state.

SDK 1.x has seven events:

|event|what it reports|
|---|---|
|`Action`|an accepted `room` carrying `action`|
|`ParticipantJoined`|a peer ID appearing in `participants`|
|`ParticipantLeft`|a peer ID disappearing from `participants`|
|`Error`|a relay `error` on the member connection|
|`ResolveFailed`|a resolver that threw or timed out (§9.2)|
|`PlayBlocked`|a player that refused to start (§10.2)|
|`Ended`|the party's end, with its reason (§7.4)|

§16.8 gives their names per language.

### Compatibility

A later 1.x may add events. Apps handle `PartyEvent` non-exhaustively, with an `else` or `default` branch that ignores unknown events. TypeScript listeners ignore unknown `type`s.

The wire enums `Phase`, `Control`, `PlayerState`, `MatchState` and `MediaKind` are fixed for protocol v1. `Connection` and `EndReason` are fixed for SDK 1.x.

### Delivery

Every subscriber gets every event emitted while it is subscribed, in order, and none is dropped or merged:

- TypeScript calls listeners synchronously, from a copy of the set. It rethrows a throwing listener's error asynchronously, while the others still run.
- Dart delivers asynchronously, and a paused subscription buffers.
- In Swift, each `events()` call is its own `AsyncStream`, which buffers without limit. So a slow reader still gets every event in order.

### The events

`Action` fires after an accepted `room` carrying an `action` of §8. A `joined` never produces one. It carries:

|field|value|
|---|---|
|`kind`|a string, an open enum|
|`byPeerId`|the acting peer's ID, null for the relay|
|`by`|that peer's entry in this snapshot, null when absent|
|`byMe`|whether `byPeerId` is this party's peer ID|
|`room`|the accepted snapshot|

`ParticipantJoined` and `ParticipantLeft` report a peer ID appearing in or disappearing from `participants` between two accepted snapshots. That includes a reconnect's `joined`, compared with the pre-drop snapshot. It doesn't include a `joined` answering `create` or `join`. `ParticipantLeft` carries the last known entry.

For one snapshot, the events come in this order:

1. all `ParticipantJoined`, in new-snapshot order
2. all `ParticipantLeft`, in old-snapshot order
3. `Action`

`Error` carries every relay `error` on the member connection. That is an error refusing a command of §12, a `status` or a `ping`, or an `error` with `re` null. Answers to `create`, `join` and `resume` don't produce it, since those calls or attempts fail instead.

`PlayBlocked` carries the error the adapter's `play` threw. The app then asks the user to press play on the player itself.

`Ended` carries its reason and an error that is non-null only for `lost`.

The rest is state, not events. Being host is `state.isHost`, reconnecting is `state.connection`, and a kick is `Ended` with `kicked`.

### Errors

Every SDK error carries `code`, `message` and `re`. For a relay refusal, `message` is the relay's `error.message` and `re` is its `re`. The `relay_full` that ends a `create` in §6 is the last relay's error, `message` and `re` included.

The SDK raises some errors itself, before or instead of sending: `room_not_found` after routing, `already_in_room`, `not_in_room`, `forbidden`, `invalid_message`, `invalid_code`, `unreachable` and `cancelled`. These carry the SDK's English text, and `re` null.

Apps never show `message`. They map `code` to their own text, with a generic text for unknown codes.

### Actions

Each action `kind` describes what happened in the room, and an app can show a toast for it:

|`kind`|`by`|what happened|example toast|
|---|---|---|---|
|`play`|the presser|playback starts once everyone is ready|"Anna pressed play"|
|`pause`|the presser|the room paused|"Anna paused"|
|`seek`|the presser|the room moved to `positionMs`|"Anna jumped to 12:34"|
|`rate`|the presser|the room plays at `rate`|"Anna set the speed to 1.25×"|
|`media`|the host|new media (`state.media` holds it) or none (`clearMedia`)|"Anna started Pilot" / "Anna stopped the video"|
|`stall`|the stalled participant|the room waits for them|"Waiting for Anna"|
|`ended`|none|the media reached its end, and the room paused there|"Finished"|
|`host`|the old host, or none|the participant with `host` true is the new host|"Ben is now the host"|
|other|any|an action this SDK doesn't describe|nothing|

A `seek` toast shows `positionMs + offset`, which is the user's own timeline of §13.

## 16. API

This section defines the API the four SDKs share. It gives the names in §16.1, the types in §16.2–§16.6, what an app must do in §16.7, how each language expresses the API in §16.8 and the test seams in §16.9.

### 16.1 Concepts and names

Each concept has one name in every SDK. Only the idioms of §16.8 differ.

|concept|name|what it is|
|---|---|---|
|party|`WatchParty`|one participant's session: connection, room state, matching, status, following|
|relay list|`Relays`, `Relay`|a list source (§3) and one entry (`id`, `name`, `url`, `prefixes`)|
|player adapter|`PlayerAdapter`|the app's player, read and commanded by the SDK (§16.4)|
|resolver|`Resolver`|the app's search, where `resolve(media)` returns candidates|
|candidate|`Candidate`|one file the user could play (§9.2), with the app's `ref` of type `T`|
|ranked candidate|`Ranked`|a candidate of the walk and its `deltaMs` once known (§9.5)|
|descriptor|`Media`, `Series`, `MediaFile`, `MediaKind`|the media descriptor of `SPEC.md` §10|
|party state|`PartyState`|everything the UI shows, as one value (§16.6)|
|room|`Room`|the snapshot of `SPEC.md` §9 as types|
|participant|`Participant`|one entry of `participants`|
|match|`Match`, `MatchState`|`{state, deltaMs}` as on the wire|
|offered file|`Mismatch`|the closest candidate and its delta while the user decides|
|enums|`Phase`, `Control`, `PlayerState`, `Connection`, `EndReason`|wire values, fixed for protocol v1. `Connection` and `EndReason` are the SDK's, fixed for SDK 1.x (§15).|
|event|`PartyEvent`|`Action`, `ParticipantJoined`, `ParticipantLeft`, `Error`, `ResolveFailed`, `PlayBlocked`, `Ended`. A later 1.x may add more (§15).|
|error|`PartyException` (Kotlin, Dart), `PartyError` (TypeScript, Swift)|`code`, `message`, `re`|
|cancellation|the language's own (§16.8)|how the SDK stops a resolver call or a probe|
|transport, clock, randomness|the language's own (§16.8)|where sockets, HTTP GETs, local time and random numbers come from|

Error codes are the relay's open set of `SPEC.md` Appendix A, plus `invalid_code`, `unreachable` and `cancelled`. They are strings, so a newer relay's code passes through.

### 16.2 `WatchParty`

This is the party's API in Kotlin. §16.8 maps it to Dart, TypeScript and Swift.

```kotlin
class WatchParty<T>(
    relays: Relays,
    client: String,          // "Example/1.0", checked by SPEC.md §6.3; the constructor throws invalid_message
    name: String,            // the user's display name, checked by SPEC.md §6.3
    resolver: Resolver<T>? = null,      // without one, others' media is unavailable at once (§9.2)
    scope: CoroutineScope,   // the party runs in a child SupervisorJob of it, on its dispatcher
    clock: MonotonicClock = MonotonicClock.System,
    random: Random = Random.Default,
)

val state: StateFlow<PartyState<T>>
val events: SharedFlow<PartyEvent>

suspend fun create(): String              // the room code
suspend fun join(code: String, create: Boolean = false)   // create: make the room when no room has the code

fun attachPlayer(player: PlayerAdapter, ref: T)    // ref: the file the player has open (§10.2)
fun detachPlayer(player: PlayerAdapter? = null): Job   // completes once released adapters are done (§11.2)
fun playerChanged()                       // optional nudge: the player's state changed

fun acceptMismatch()
fun declineMismatch()
suspend fun choose(candidate: Candidate<T>): Boolean
fun resolveAgain()

fun play()
fun pause()
fun seek(positionMs: Long)
fun setRate(rate: Double)
fun setOffset(offsetMs: Long)

fun setMedia(media: Media, ref: T, positionMs: Long = 0)
fun clearMedia()
fun setControl(mode: Control)
fun transferHost(peerId: String)
fun kick(peerId: String)
fun lock(locked: Boolean)
fun end()

fun leave()
fun roomPositionMs(): Long?
fun close(): Job                          // completes as detachPlayer's does
```

`create()` returns the room code as 6 symbols, unformatted. `choose` is described in §9.6.

#### Helpers

The SDK also has four top-level helpers:

|helper|what it does|
|---|---|
|`formatCode(code)`|formats a room code for display (§4.1)|
|`sameTitle(a, b)`|tells whether two descriptors are the same title (§9.3)|
|`checkMedia(media)`|returns null when the descriptor passes the send checks of §9.7, else the first rule it breaks|
|`parseMedia(json)`|the one descriptor decoder. It applies the receive checks of §9.7.|

With `checkMedia`, an app trims `file.name` or `ext` and checks again, instead of getting `invalid_message`.

`parseMedia` doesn't apply the send-only sizes. It returns a `Media`, which in TypeScript is a fresh frozen copy, and throws `invalid_message` on failure.

### 16.3 `Relays`

A `Relays` object is a list source of §3:

```kotlin
class Relays {
    companion object {
        fun public(http: HttpClient): Relays
        fun at(http: HttpClient, baseUrl: String): Relays   // throws invalid_message for a bad base URL (§3.1)
    }
    val list: List<Relay>                 // the list in use (§3.1)
    fun relayFor(code: String): Relay?    // normalizes (§4.1); null for an invalid code or a symbol nobody owns
    suspend fun refresh(): Boolean        // a forced refresh (§3.3); false when no valid list came back
}
data class Relay(val id: String, val name: String, val url: String, val prefixes: String)
```

`relayFor` never waits, never refreshes and never throws.

### 16.4 `PlayerAdapter`

The adapter is the app's player, as the SDK reads and commands it:

```kotlin
interface PlayerAdapter {
    val positionMs: Long              // current position in the app's file
    val durationMs: Long?             // the file's duration; null when unknown
    val paused: Boolean
    val buffering: Boolean            // stalled while trying to play
    val firstFrameShown: Boolean      // the file is open and shows a frame
    val canSetRate: Boolean get() = false
    val seekAccuracyMs: Long get() = 0   // how far from its target a seek may land (§11.2)
    suspend fun play()                   // throws when the player refuses to start (§10.2)
    suspend fun pause()
    suspend fun seek(positionMs: Long)   // returns once the seek has landed
    suspend fun setRate(rate: Double) {}
}
```

#### Reads

The reads are cheap and synchronous, since the SDK reads them on every tick of §11. `canSetRate` may change while attached. `firstFrameShown` may stay false until the app's own start-up work is done, and the SDK reports `loading` until then.

#### Commands

Commands return when the player has done them. A `seek` that returns early makes the SDK measure mid-seek positions. A command must finish or throw, because a hung one holds its lane and the release completion of §11.2.

`play` throws when the player refuses to start, such as a browser blocking playback without a user gesture. The SDK then reports `loading` and emits `PlayBlocked` (§10.2).

`seekAccuracyMs` is how far from the requested position a seek may land. A player that seeks to keyframes declares its keyframe distance, and one that can't seek declares a very large value, as §11.2 explains.

A 1.x release may add adapter members with defaults. §16.8 says how each language keeps that from breaking apps.

### 16.5 `Resolver` and `Candidate`

```kotlin
fun interface Resolver<T> {
    suspend fun resolve(media: Media): List<Candidate<T>>   // cancelled as §9.1 says
}

class Candidate<T>(
    val id: String,
    val ref: T,
    val durationMs: Long? = null,
    val sizeBytes: Long? = null,
    val oshash: String? = null,
    val name: String? = null,
    val probe: (suspend () -> Long?)? = null,
)

data class Ranked<T>(val candidate: Candidate<T>, val deltaMs: Long?)
```

The field rules are those of §9.2.

### 16.6 `PartyState` and the room types

```kotlin
data class PartyState<T>(
    val connection: Connection,         // idle, connecting, connected, reconnecting, ended
    val peerId: String?,                // own peer ID while in a room
    val room: Room?,                    // the last accepted snapshot
    val media: Media?,                  // the current descriptor
    val match: Match?,                  // as reported in status
    val choice: Candidate<T>?,          // the file to open for the current media
    val mismatch: Mismatch<T>?,         // the file offered while the user decides
    val candidates: List<Ranked<T>>,    // the walk's ranked candidates (§9.5)
    val offsetMs: Long,
    val player: PlayerState,            // as reported in status
    val endReason: EndReason?,
) {
    val me: Participant?                // own entry of room.participants
    val isHost: Boolean
    val canControl: Boolean             // host, or own entry present and control is everyone
}

data class Room(
    val rev: Long, val code: String, val epoch: Long, val phase: Phase, val positionMs: Long, val atMs: Long,
    val rate: Double, val control: Control, val locked: Boolean, val waitingOn: List<String>,
    val participants: List<Participant>,
)
data class Participant(
    val peerId: String, val name: String, val client: String, val host: Boolean, val connected: Boolean,
    val player: PlayerState, val match: Match?, val offsetMs: Long,
)
data class Match(val state: MatchState, val deltaMs: Long?)
data class Mismatch<T>(val candidate: Candidate<T>, val deltaMs: Long)
```

State changes when a snapshot, the media, matching, the player state, the offset or the connection changes. Equal states aren't republished. Positions are read on demand, through `roomPositionMs()` or the adapter.

#### Read-only state

Published state is read-only, and each language makes it so in its own way.

In TypeScript, every published type is deeply `readonly`: `PartyState`, `Room`, `Participant`, `Match`, `Mismatch`, `Ranked`, `Media`, `Series`, `MediaFile`, `Relay` and `Relays.list`. The SDK deep-freezes every object and array it creates and publishes, once at creation. That covers the snapshot, participants, media it parsed, match, mismatch, the ranked list and relay lists. It never freezes app objects, such as candidates, refs and adapters.

In Kotlin and Dart, the SDK keeps its own deep copy of every descriptor, list and map the app passes in. Changing the app's object afterwards changes nothing in the party. The collections the SDK publishes are unmodifiable.

In Swift, `state` is `private(set)`. `Media` and every published type but `Candidate`, which is a class of constants, are value types. So neither side's later change reaches the other.

#### Media

`Media` mirrors `SPEC.md` §10 field for field: `kind`, `title`, `year`, `ids`, `series`, `season`, `episode`, `durationMs`, `file` and `ext`.

In Kotlin and Dart, `Media` is an immutable value with `toJson()`. In TypeScript it is a plain object. In Swift it is a `struct` with `toJSON()`, which returns a `JSONValue`.

The `parseMedia` helper of §16.2 decodes. Decoding and encoding keep unknown `ids` namespaces and every `ext` member unchanged. They drop unknown members outside `ext`, including inside `series` and `file`.

### 16.7 What apps must do

Every app does these things:

- It constructs the party with a `Relays`, a true `client` name and the user's name. To match other participants' media, it also passes a resolver (§16.2).
- It calls `create()` or `join(code)`, shows the code, and handles `Ended` (§15).
- It handles `PartyEvent` non-exhaustively, ignoring events it doesn't know (§15).
- It writes a `PlayerAdapter` with the reads and `play`, `pause` and `seek`. In Dart, it extends `PlayerAdapter` (§16.4, §16.8).
- It opens `state.choice` whenever it changes, and calls `attachPlayer(player, ref)` once per opened file, with the `ref` of the file it opened (§10.2). A player already playing `choice.ref` isn't reopened, nor attached again while it is still attached.
- After `Ended`, which releases the adapter, it attaches a player that still has its file open again, before or after the next `create` or `join` (§7.4, §10.2).
- As host, it calls `setMedia(media, ref)` with the `ref` that reopens the shared file, and `attachPlayer(player, ref)` with that `ref` once it's open. The two calls can come in either order (§10.2).
- It calls `detachPlayer()` when the user leaves the player or the app goes to the background.
- It awaits the completion of `detachPlayer` or `close()` before disposing a player (§11.2).
- It answers `state.mismatch` with `acceptMismatch()`, `declineMismatch()` or `choose(candidate)` (§9.6).
- It routes the user's play, pause, seek and speed controls to the party (§12).
- It calls `close()` when done with the party (§7.4).

These are optional:

- `setRate` and `canSetRate` on the adapter. Without them, the SDK follows by seeking only.
- `seekAccuracyMs` on the adapter, for a player that seeks to keyframes or can't seek (§11.2).
- A known `durationMs` on the adapter. It may be null, and then the SDK doesn't protect the end of a shorter file.
- `playerChanged()`. Without it, changes reach the relay on the next tick.
- Probes. Candidates may carry durations instead (§9.2).
- The offset control (§13).
- Toasts from `Action`, and the participant events.
- Host controls.
- `choose`, `resolveAgain`, `relayFor`, `formatCode`, `sameTitle`, `checkMedia` and `parseMedia`.
- The transport, clock and randomness parameters.

### 16.8 Per-language idioms

This table maps each concept of §16.1–§16.7 to each language. Longer explanations follow the table.

|concept|Kotlin|Dart|TypeScript|Swift|
|---|---|---|---|---|
|constructor|parameters as in §16.2, with `scope` passed by name when there is no resolver|named parameters, with `relays`, `client` and `name` required, and `resolver`, `connect`, `nowMs` and `random` optional|`new WatchParty({relays, client, name, resolver?, connect?, nowMs?, random?})`|`init(relays:client:name:resolver:connect:clock:random:) throws` with `Ref: Equatable & Sendable`. The parameters after `name` have defaults.|
|`Relays`|`Relays.public(http)`, `Relays.at(http, baseUrl)`|`Relays.public({httpGet})`, `Relays.at(baseUrl)`|`Relays.public({httpGet?})`, `Relays.at(baseUrl)`|`Relays.public(httpGet:clock:)`, whose parameters have defaults, and `Relays.at(_:) throws`|
|transport|the app's Ktor `HttpClient` with WebSockets installed, given to `Relays`|`connect: Future<WebSocket> Function(Uri)` from package `web_socket` on the party, and `httpGet: Future<String> Function(Uri, Cancellation)` on `Relays.public`|`connect: (url: string) => WebSocket` on the party, and `httpGet: (url: string, signal: AbortSignal) => Promise<string>` on `Relays.public`|`connect: WebSocketConnect` on the party, and `httpGet: HTTPGet` on `Relays.public`|
|clock|`clock: MonotonicClock`, a `fun interface { fun nowMs(): Long }`. Timers run on the scope's dispatcher, so a test dispatcher drives them.|`nowMs: int Function()`. Timers are `dart:async` `Timer`s, so `fake_async` drives them.|`nowMs: () => number`. Timers are `setTimeout`, so fake timers drive them.|`clock: any PartyClock` on the party and on `Relays.public`, by default `SystemClock`|
|randomness|`random: kotlin.random.Random`, with u from `nextDouble()`|`random: Random` from `dart:math`, with u from `nextDouble()`|`random: () => number` in [0, 1), with u from `random()`|`random: any RandomNumberGenerator`, by default `SystemRandomNumberGenerator()`, with u from `Double.random(in: 0..<1, using:)`|
|state|`state: StateFlow<PartyState<T>>`|`PartyState<T> get state` and `Stream<PartyState<T>> get states`, a broadcast stream that emits on change|`get state(): PartyState<T>` and `on("state", listener)`|`private(set) var state: PartyState<Ref>` and `states() -> AsyncStream<PartyState<Ref>>`, a new stream per call with every state published after it, in order|
|events|`events: SharedFlow<PartyEvent>` with an unbounded buffer, so it never drops and never suspends the party|`Stream<PartyEvent> get events`, a broadcast stream|`on("event", listener)`, where `on` returns an unsubscribe function|`events() -> AsyncStream<any PartyEvent>`, a new stream per call that buffers without limit, so it never drops and never suspends the party|
|async work|`suspend` functions|`Future`s|`Promise`s|`async` functions, with `create` and `join` `async throws`|
|player calls|`attachPlayer(player: PlayerAdapter, ref: T)`, `detachPlayer(player: PlayerAdapter? = null): Job` and `close(): Job`|`void attachPlayer(PlayerAdapter player, T ref)`, `Future<void> detachPlayer([PlayerAdapter? player])` and `Future<void> close()`|`attachPlayer(player: PlayerAdapter, ref: T): void`, `detachPlayer(player?: PlayerAdapter): Promise<void>` and `close(): Promise<void>`|`attachPlayer(_ player: any PlayerAdapter, ref: Ref)`, `detachPlayer(_ player: (any PlayerAdapter)? = nil) -> Task<Void, Never>` and `close() -> Task<Void, Never>`, the last two `@discardableResult`|
|`PlayerAdapter`|an `interface` whose optional members have defaults, which apps implement|an `abstract base class` whose optional members have defaults. Apps `extend` it, with `base` or `final` subclasses.|an object type with optional members|a `@MainActor` class-bound `protocol`, whose optional members have defaults in a protocol extension. Apps implement it with a class.|
|`Resolver`|`fun interface Resolver<T>`|`abstract interface class Resolver<T>` with `resolve(media, cancel)`|an object `{ resolve(media, signal) }`|`struct Resolver<Ref>` made from a closure, as in `Resolver { media in … }`|
|cancellation|coroutine cancellation of `resolve` and `probe`|`resolve(Media media, Cancellation cancel)` and `probe(Cancellation cancel)`. `Cancellation` has `bool isCancelled` and `Future<void> whenCancelled`.|`resolve(media, signal: AbortSignal)` and `probe(signal: AbortSignal)`|Task cancellation of the task running the resolver closure or `probe`, which is a `@MainActor @Sendable () async throws -> Int64?`|
|`PartyEvent`|non-sealed `interface` with nested `PartyEvent.Action`, `PartyEvent.ParticipantJoined`, …|`abstract final class`, not exhaustive and not implementable outside the library. Subclasses take the names plus `Event`, as in `ActionEvent` and `ErrorEvent`.|a union discriminated by `type`: `"action"`, `"participantJoined"`, `"participantLeft"`, `"error"`, `"resolveFailed"`, `"playBlocked"`, `"ended"`. Listeners ignore other `type`s.|`protocol PartyEvent: Sendable`. Structs take the names plus `Event`, as in `ActionEvent` and `ErrorEvent`. Apps match them with `as` casts and a `default:` branch.|
|enums|enum classes|enums|string literal unions|enums, with `String` raw values for the wire enums|
|errors|`PartyException`|`PartyException implements Exception`|`PartyError extends Error`|`struct PartyError: Error`, whose `code` is a `PartyError.Code`: a string wrapper with constants such as `.unreachable`|
|integers|`Long`|`int`|`number`, for integers up to 2^53 − 1|`Int64`|
|optional adapter rate control|`canSetRate` and `setRate` with defaults|`canSetRate` and `setRate` with defaults|an optional `setRate?` method, whose presence means it can set rates|`canSetRate` and `setRate` with defaults|
|seek accuracy|`val seekAccuracyMs: Long get() = 0`|`int get seekAccuracyMs => 0`|optional `readonly seekAccuracyMs?: number`|`var seekAccuracyMs: Int64 { get }`, `0` by default|
|published collections|read-only views not backed by a list the SDK still mutates|`List.unmodifiable` and `Map.unmodifiable`|deeply `readonly` types, frozen with `Object.freeze`|value types such as `Array` and `Dictionary`, so the app's copy never changes the party's|
|`ext`|`JsonObject?`|`Map<String, Object?>?`|`Record<string, unknown>`|`[String: JSONValue]?`|
|`setMedia`|`setMedia(media, ref, positionMs = 0)`|`setMedia(media, ref, {positionMs = 0})`|`setMedia(media, ref, positionMs = 0)`|`setMedia(_ media: Media, ref: Ref, positionMs: Int64 = 0) throws`|
|`join`|`join(code, create = false)`|`join(code, {create = false})`|`join(code, { create: true })`, an options object whose `create` is false when absent|`join(_ code: String, create: Bool = false) async throws`|
|`Media` JSON|`parseMedia(json: JsonElement): Media` and `toJson()`|`Media parseMedia(Object? json)` and `toJson()`|plain objects, and `parseMedia(value: unknown): Media`|`parseMedia(_ json: JSONValue) throws -> Media` and `toJSON() -> JSONValue`. `JSONValue(parsing:)` reads JSON text.|
|threads|a single-threaded scope, on Android a Main scope|one isolate|one event loop|`@MainActor`|

#### Transport

The Kotlin party opens its sockets with the app's `HttpClient`. The SDK makes its GETs through a copy configured with `followRedirects = false`, and reads the body as a stream up to the limit.

Dart's defaults are the `dart:io` `HttpClient` with `followRedirects = false`, or the browser's `fetch` with `redirect: "error"`. Elsewhere, the app passes `httpGet`.

In TypeScript, the default uses `fetch` with `redirect: "error"` and the signal.

Swift's `WebSocketConnect` is `@MainActor (String) async throws -> any WebSocketConnection`, a text WebSocket with `receive`, `closeCode`, `send` and `close(code:)`. Its `HTTPGet` is `@MainActor (String) async throws -> String`.

The Swift defaults are `urlSessionConnect`, which uses `URLSessionWebSocketTask`, and `urlSessionGet`. That uses `URLSession` without redirects, with status 200 only and at most 65536 bytes. Both stop when their task is cancelled.

#### Clock and randomness

Swift's `PartyClock` is a `@MainActor` protocol with `nowMs() -> Int64` and `schedule(afterMs:_:)`, which returns a cancel function. The default `SystemClock` uses `ContinuousClock` and `Task.sleep`. Every SDK timer goes through the clock, so a virtual clock drives them.

Each random draw is one u in [0, 1), and the reconnect backoff of §14 is the only draw.

#### Threads

In Kotlin, the app passes a single-threaded scope and calls the party from that thread. The party, adapter calls and resolver calls run on its dispatcher.

In Swift, the party and `Relays` run on the main actor, and call the adapter, resolver, clock and transport there.

#### Compatibility and errors

Each adapter type lets a 1.x release add a member with a default without breaking apps. Kotlin's interface members have defaults, Dart apps extend the base class, TypeScript members are optional, and Swift protocols get the default in a protocol extension.

On a resolver timeout of §9.2, `ResolveFailed` carries the language's timeout error. That is Kotlin's `TimeoutCancellationException`, Dart's `TimeoutException`, Swift's `TimeoutError`, or in TypeScript a `DOMException` named `TimeoutError`.

TypeScript also accepts plain values where a Promise is expected from adapter commands and the resolver.

#### Unpaired surrogates in Swift

A Swift `String` can't hold an unpaired surrogate. So `name`, `client` and descriptors from a Swift app never carry one, and the refusal of §7.1 and §9.7 before sending holds by the type.

The SDK reads relay messages with its own JSON reader. It turns an unpaired surrogate escape into U+FFFD and marks it, so the receive checks still refuse it. `JSONValue(parsing:)` throws `invalid_message` on one.

### 16.9 Test seams

Each SDK exposes the deterministic parts as functions, at least to its own tests:

- the room position and running check
- the clock, fed pings and pongs with explicit times
- the reconciliation step
- ranking, and the matching walk with simulated probe times
- `sameTitle`
- code normalization and routing
- relay list validation
- the `name` and `client` checks
- the snapshot check
- the receive and send checks of descriptors

A runner may instead drive a rule through a party in virtual time, as TypeScript runs `matching.json`. It may also drive one through the constructor, as Kotlin, TypeScript and Swift run `names.json`.

Tests run a whole party against a fake relay in virtual time. They do this through the clock and randomness parameters of §16.8 and a socket seam. That seam is `connect` in Dart, TypeScript and Swift, and an internal transport in Kotlin.

## 17. Conformance

Every SDK shows its conformance in three ways: the shared vector tables, tests of its own, and four scenarios against a real relay.

Every SDK runs every table in `conformance/vectors/` in its own suite and passes all of them. Its suite fails on a table it has no runner for. `conformance/vectors/README.md` documents each table's format.

|table|section|
|---|---|
|`timeline.json`|§8|
|`clock-sync.json`|§5|
|`reconcile.json`|§11, §13|
|`matching.json`|§9.4, §9.5|
|`same-title.json`|§9.3|
|`code-routing.json`|§4|
|`relay-list.json`|§3.2|
|`names.json`|§7.1|
|`media-check.json`|§9.7|
|`room-check.json`|§8|
|`party.json`|§3–§15|

`party.json` holds traces of a whole party in virtual time. Each step is an API call, a relay message, a player, resolver or HTTP answer, or time passing. It lists what the party must then have sent, opened, fetched, published and emitted. A rule a trace pins needs no test of its own.

### Tests of its own

Some rules can't be a trace or a table. Each SDK also has its own tests that check these:

- Its embedded relay list equals the repository's `relays.json`.
- It accepts a relay message of 32768 bytes and more, as `SPEC.md` §3.1 requires.
- It writes every outgoing int without a fraction or exponent, also when the value came from arithmetic (§2).
- It refuses an integer API input or a computed wire value outside its bounds before any local effect (§2, §12, §13).
- A timer longer than 2147483647 ms doesn't run its action early, and doesn't re-arm in a tight loop (§2).
- It refuses unpaired surrogates in `name`, `client` and descriptors before sending (§7.1, §9.7).
- In TypeScript, changing published state throws in strict mode.
- In Kotlin and Dart, changing a map or list the app passed to `setMedia` afterwards changes nothing in the party. Swift needs neither test, since `state` is `private(set)` and `Media` is a value type (§16.6).
- `parseMedia` applies the receive checks and fails with `invalid_message` (§16.2).
- A probe that throws a cancellation of its own, such as its own timeout, counts as returning nothing, and the walk goes on (§9.2).
- Its HTTP GETs are aborted after 5 s even when the fetcher ignores the signal. A body over 65536 bytes fails, redirects aren't followed and only status 200 succeeds (§3.4).

### Scenarios

Each SDK runs exactly four scenarios against a real relay on `127.0.0.1`:

1. `create`, and `join` with a typed code, in lower case and with a separator. Then the host ends the room, and its `join` of that code with `create`, typed the same way, makes the room again with the party as host.
2. `setMedia`: the other party's resolver matches, both report `ready`, the room plays and both players follow.
3. `pause`, `seek` and `setRate` reach the other player.
4. A dropped connection is resumed, as §14 describes.

TypeScript also restarts the relay with `--state-dir`, and both parties resume into the saved room.

A test of an SDK's own transport against a local fake server is not a scenario. Such tests cover things like 32768-byte messages, close codes or HTTP limits.

### The Swift suites

Swift runs its suites with swift-testing through `swift test` at the repository root, where its `Package.swift` sits.

`VectorTests`, in `sdk/swift/Tests/OpenWatchPartyTests/VectorTests.swift`, runs every table but `party.json` against the SDK's internal functions, through `@testable import OpenWatchParty`. It runs the matching walk of `matching.json` on a `VirtualClock`. Its `everyTableRuns` fails on a table it doesn't run. `PartyTraceTests` runs `party.json`.

The party tests the traces can't replace are `SessionTests`, `MatchingTests` and `PlayerTests`, with the helpers in `Harness.swift`. They run a `WatchParty` against `FakeRelay` in virtual time, with:

- a `VirtualClock` as `clock`, which runs timers only when a test calls `run()`, `advance(by:)` or `runUntil`
- `FakeRelay.connect` as `connect`
- a seeded `RandomNumberGenerator`, `SeededRandom`, as `random`
- `Relays.public` with an `httpGet` that fails, `notFoundRelays`

`RelayListTests` drives `Relays` through `httpGet` on a `VirtualClock`. `DefaultHTTPGetTests` runs the default `urlSessionGet` against a local HTTP server. `WireTests` covers outgoing ints and timers longer than 2147483647 ms.

`IntegrationTests`, in `IntegrationTests.swift` with `RelayHarness.swift`, runs the four scenarios with the default `URLSession` transport against a real relay. Each test gets a fresh relay on `127.0.0.1`, with `--timer-scale 0.1` and raised per-IP and rate limits, as Kotlin's harness runs it. The relay is `OWP_RELAY_BIN` when set, else `relay/` built with `go build` for the suite. `TcpProxy` cuts the connection in the resume scenario.

The suite builds only on macOS and Linux, which can start the relay. It uses `#if os(macOS) || os(Linux)` for that.

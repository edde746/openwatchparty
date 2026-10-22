# Conformance vectors

These eleven JSON tables pin the deterministic parts of [SDK.md](../../spec/SDK.md). The same input gives the same result in every SDK, and every SDK runs every table. Rules a table cannot express are covered by each SDK's own tests (SDK.md §17).

Read this guide when you change a client rule, add a case or a table, or write the runner for an SDK.

A Go reference in `tools/` generates the tables:

- `ref.go` and `check.go` hold the rules.
- `cases.go` holds the cases.
- `cases_party*.go` hold the traces of `party.json`.

## Generating and checking

In `conformance/vectors/tools`, with Go 1.26:

```
go run .            # rewrite every table
go run . -check     # exit 1 if any table differs from the generator's output
go test ./...       # boundary tests of the reference implementation and the trace checks
```

`-dir <path>` names the vectors directory, which is `..` by default.

`-check` reports each problem, sorted, under the header ``vectors are stale (run `go run .` in conformance/vectors/tools):``:

```
  extra.json: not produced by the generator
  names.json: missing
  timeline.json: differs from the generator's output
```

The tests guard the generator itself:

- `ref_test.go` pins hand-worked boundaries. It also rejects duplicate case names, nondeterministic output, invalid JSON and bytes that aren't printable ASCII.
- `party_check_test.go` pins the traces that the trace check refuses.

CI checks the formatting, runs vet and the tests, and runs `-check`.

## Adding a case or a table

Cases and tables start in the generator, and the tables are regenerated from it.

Add cases in `cases.go`, with helpers such as `add(...)`. `value(...)` adds a case both ways. The expectations come from `ref.go` and `check.go`, never by hand.

The one exception is `party.json`. `cases_party.go` and the `cases_party_<area>.go` files beside it write its outputs by hand, from SDK.md and SPEC.md, and each trace names its sections in a comment. `party_check.go` checks the shape of those outputs, not their values.

To change a rule:

1. Change the generator's files and SDK.md together. They must agree.
2. Run `go run .`.
3. Commit the tables.

A new table needs an `xxxTable()` function, an entry in `tables()`, and a runner in each SDK. Every SDK fails until it has its runner.

The generator refuses size cases that depend on the encoder or are mislabelled. It also refuses inputs that hold U+FDD0.

## File format

Each file is `{"table": <name>, "cases": [...]}`, in that order. `table` is the file name minus `.json`.

Each case is `{"name","input","expect"}`:

- Each case is on its own line, as compact JSON in printable ASCII.
- Non-ASCII characters are written as `\uXXXX`, with no HTML escaping.
- Names are unique within a table.
- Times are integer ms.

```json
{
  "table": "timeline",
  "cases": [
    {"name":"idle holds positionMs","input":{"room":{"phase":"idle","positionMs":0,"atMs":1000,"rate":1},"durationMs":3487000,"serverNowMs":50000},"expect":{"positionMs":0,"running":false}},
    …
  ]
}
```

Decode the tables with the platform's JSON decoder. The inputs hold unpaired surrogate escapes such as `"Anna\ud800"`, and the expectations are computed on the strings as the SDK platforms hold them.

An int may be written with a fraction or an exponent. Any integral number within ±(2^53−1) counts ([SPEC.md](../../spec/SPEC.md) §2).

All `matching` and `same-title` descriptors pass the receive checks, so parse them strictly.

## Comparing results

Compare whole `expect` objects. Member order doesn't matter, but an absent member is not the same as `null`. For example:

- In `code-routing` and `relay-list`, and in no other table, an error expect is just `{"error":…}`.
- Actions carry only their own members.

Kotlin's runner can't tell an absent member from `null`.

Round `setRate` rates to 6 decimals before you compare them.

`same-title` is symmetric. The TypeScript, Kotlin and Swift runners also test each case with the descriptors swapped.

`party.json` is compared step by step instead, as [its section](#partyjson-sdkmd-315) says.

## Runners in the SDKs

Each SDK has one runner for the tables and a driver for `party.json`. A guard test fails on any table the runner doesn't run.

| SDK | Runner | Guard test | Run |
|---|---|---|---|
| TypeScript | `sdk/typescript/test/unit/vectors.test.ts` | `every vector table has a runner` | `npm run test:unit` in `sdk/typescript` |
| Dart | `sdk/dart/test/vectors_test.dart` | `every table has a runner` | `dart test` in `sdk/dart` |
| Kotlin | `sdk/kotlin/src/commonTest/kotlin/party/openwatch/VectorTest.kt` | `everyTableRuns` | `./gradlew check` in `sdk/kotlin` |
| Swift | `sdk/swift/Tests/OpenWatchPartyTests/VectorTests.swift` | `everyTableRuns` | `swift test --filter VectorTests --filter PartyTraceTests` at the repository root |

The runners find the tables in different ways:

- TypeScript runs every `*.json` in `conformance/vectors/` except `party.json`, which `party-traces.test.ts` runs.
- Dart runs each table it names, read from `../../conformance/vectors/`, and `party.json` through `party_traces_test.dart`.
- Kotlin runs each table it names, from test data generated out of `conformance/vectors/`, and `party.json` through `PartyTraces.kt`.
- Swift runs each table it names, read from `conformance/vectors/`, except `party.json`, which `PartyTraceTests.swift` runs.

The `matching` table runs differently in each SDK:

- TypeScript runs a real party on mocked timers.
- Kotlin runs the walk under `runTest`.
- Swift runs the walk on a virtual clock.
- Dart calls the ranking and walk functions directly.

Swift decodes the tables with the SDK's own JSON reader, not the platform decoder. A Swift `String` can't hold an unpaired surrogate. The reader therefore turns each unpaired surrogate escape into U+FFFD and marks the string, so the checks still see it.

## The tables

Each table below lists its input and expect members. The rule itself is in the SDK.md section in the table's heading, and the constants in play are those of SDK.md §2.

### `timeline.json` (SDK.md §8)

The input holds `room{phase,positionMs,atMs,rate}`, `durationMs` and `serverNowMs`. The expectation holds `positionMs` and `running`.

### `clock-sync.json` (SDK.md §5)

The input is `events`, a list of `{"ping":id,"atMs":t}` and `{"pong":id,"serverTime":s,"atMs":t}`, with local times.

The expectation is `pongs[]`. Each pong has a `result`, plus the best `offsetMs` and `rttMs` after it, or null if there is none. The `result` is one of these:

- `accepted`: the sample was kept.
- `stepped`: the window was cleared, then the sample was kept.
- `ignored`: the pong was unknown, or its round trip was outside 0..1000.

### `reconcile.json` (SDK.md §11)

The input holds `room`, `durationMs`, `serverNowMs`, `offsetMs`, `player{positionMs,paused,buffering,durationMs|null,canSetRate,rate,seekAccuracyMs?}`, `nudging` and `sinceSeekMs`.

- `seekAccuracyMs` is an int ≥ 0. When it is absent, it counts as 0.
- A null `sinceSeekMs` means the adapter has cooled down.

The expectation holds `actions`, `nudging` and `playInMs`:

- `actions` lists the adapter calls in the order pause, setRate, seek, play. Each is `{"do":…}` plus only `rate` or `positionMs`.
- `playInMs` is the wait for a scheduled local start, rounded up, or null. It is also set when a negative local position delays the start.

Only a playing seek gets the +250 lead, clamped. A held seek goes to `clamp(positionMs + offsetMs)`.

### `matching.json` (SDK.md §9.4, §9.5)

The input holds `media` and `candidates[]{id,durationMs?,sizeBytes?,oshash?,name?,probe?{durationMs|null,takesMs}}`. A candidate's `probe` is the simulated answer of its probe. A null `durationMs` there means the probe didn't open the file.

The expectation holds `order`, `candidates[]{id,deltaMs|null}`, `probed`, `result`, `choice` and `deltaMs`. The `result` is `matched|mismatch|unavailable`.

### `same-title.json` (SDK.md §9.3)

The input holds two descriptors, `a` and `b`. The expectation is `same`.

### `code-routing.json` (SDK.md §4)

The input holds `code` and `relays[]{id,prefixes}`.

The expectation holds the normalized `code`, and `relay`: the first relay that owns the code's first symbol, or null if none does. For a code that isn't valid, it is only `{"error":"invalid_code"}`.

### `relay-list.json` (SDK.md §3.2)

The input is `list`, any JSON value taken as a public list document. The expectation is `relays` as given, or only `{"error":"invalid_list"}`.

### `names.json` (SDK.md §7.1)

The input holds `field`, which is `name|client`, and `value`. The expectation is `valid`. When it is false, the constructor throws `invalid_message`.

- A `name` has 1-32 code points.
- A `client` has 1-64 code points, in the form `<app>/<version>`, with both parts non-empty. The app ends at the first `/`.

### `media-check.json` (SDK.md §9.7)

The input holds `on` and `media`. `on` is `receive|send`, and it is also the suffix of the case name. The expectation is `valid`.

Which checks apply depends on the direction:

- Shape and value checks apply both ways. The shape cases are receive-only.
- Size checks apply on send only: at most 4096 bytes for the descriptor and 1024 for `ext`. They measure the SDK's compact encoding of the known members, with ints integral, in UTF-8.

Run the receive check for a receive case, and `checkMedia` for a send case.

### `room-check.json` (SDK.md §8)

The input is `room`: the members of a `room` message except `type` and `media`, that is the snapshot fields and an optional `action`. The expectation is `valid`, true when the SDK accepts the `room`.

When `action` is present, it is an object whose `kind` is a non-empty string and whose `by` is null or a peer ID.

### `party.json` (SDK.md §3–§15)

`party.json` holds traces of the party state machine. A driver runs a real `WatchParty` against a scripted relay, scripted adapters and a scripted resolver, in virtual time. For each step i, it runs the step, settles, and compares what the party did with `outputs[i]`.

The drivers are:

- `sdk/typescript/test/unit/party-traces.test.ts`
- `sdk/dart/test/party_traces_test.dart`
- `sdk/kotlin/src/commonTest/kotlin/party/openwatch/PartyTraces.kt`, run by `VectorTest.party()`
- `sdk/swift/Tests/OpenWatchPartyTests/PartyTraceTests.swift`

A trace's input holds `setup` and `steps[]`. Its expectation is `outputs[]`, one per step.

#### Setup

| Member | What it sets |
|---|---|
| `startMs` | The local time at the start: the `nowMs` seam and the timers' clock. |
| `relayOffsetMs` | Relay time − local time. |
| `rttMs` | The starting round trip `rtt` of every URL. |
| `random` | The draws `u` the randomness seam yields, in order. |
| `base` | `Relays.at(base)`, one relay that never fetches, or null for `Relays.public()`. |
| `list` | Null with a `base`. Otherwise a valid public list, as JSON text. |
| `name`, `client` | The party's name and client. |
| `resolver` | True when the scripted resolver is passed. |

Each `ping` gets its `pong` `rtt` later. The pong's `serverTime` is the local send time + floor(`rtt` / 2) + `relayOffsetMs`.

Each draw in `random` is a multiple of 2^-53 in [0, 1). A backoff waits floor(base × (0.5 + 0.5`u`)), as SDK.md §14 says.

`list` is the document every GET of `https://openwatch.party/relays.json` answers. `Relays.public()` starts from the SDK's embedded list. A trace that routes before a refresh succeeded therefore uses the repository's `relays.json`, which the generator reads. The other traces on the public list call `refresh` first.

#### Steps

A step has one kind. It calls the API, plays the relay or the network, moves time, or drives an adapter or the resolver.

A call step is `{"call": api, "id"?, …args}`. These are the calls, with their arguments:

| Area | Calls |
|---|---|
| Room | `create`, `join{code,create?}`, `leave`, `end`, `close` |
| Media | `setMedia{media,ref,positionMs?}`, `clearMedia` |
| Playback | `play`, `pause`, `seek{ms}`, `setRate{rate}`, `setOffset{ms}` |
| Host | `setControl{mode}`, `lock{locked}`, `kick{peerId}`, `transferHost{peerId}` |
| Matching | `choose{candidate}`, `acceptMismatch`, `declineMismatch`, `resolveAgain` |
| Player | `attachPlayer{player,ref}`, `detachPlayer{player?}`, `playerChanged` |
| Relays | `refresh` (`Relays.refresh()`) |

- `id` names the call for its `done` output. Calls that return a result or a completion need one: `create`, `join`, `close`, `choose`, `detachPlayer` and `refresh`.
- A call without `id` must not throw.
- `setMedia` may carry a descriptor that fails the send checks (SDK.md §9.7) only with an `id`.

These steps play the relay:

- `{"recv": msg, "malformed"?: true}`: the relay sends `msg`.
- `{"recvText": text}`: the relay sends `text` as it is, such as text that isn't JSON, or an array.
- `{"drop": code|null}`: the relay closes the connection with `code`, or with none for null.

`recv`, `recvText` and `drop` take `"on": url` for the newest open connection to that URL. Without `on`, they act on the newest open connection. `nextOpen` and `rtt` take `on` too, for that URL's connections only.

A received `joined`, `room`, `error` or `closed` passes SDK.md §8's check of its type, or fails it when marked `malformed`. For a `room`, that check is the `room-check.json` rule. Its `media` member is outside the rule, because the party checks `media` only on a `room` of a new epoch.

A received `pong` is only ever a malformed one, whose `serverTime` is not an int ≥ 0, since the driver answers pings itself. Other types aren't checked and can't be marked.

These steps play the network and the clock:

- `{"nextOpen": "fail"|"hang"}`: the next connection fails without opening, or never opens.
- `{"rtt": ms|null}`: pings the party sends from now on are answered `ms` ≥ 0 later, or never. Pings already sent keep their round trip.
- `{"nextGet": doc|"fail"|"hang"}`: the next GET answers `doc` (an object) instead of `list`, fails, or never answers.
- `{"advance": ms}`: local time moves `ms` ≥ 1 ahead.

With `on`, `nextOpen` applies to the next connection to that URL. Without it, it applies to the next connection to any URL, but one scripted for that connection's URL goes first. Every other connection opens in the settle after the connect.

With `on`, `rtt` applies to that URL's connections. Without it, it applies to every URL's.

These steps drive the adapters:

- `{"player": id, positionMs?, paused?, buffering?, durationMs?, firstFrameShown?, canSetRate?, seekAccuracyMs?, playThrows?}` creates adapter `id` on first use and sets the members given.
- `{"hold": id}`: adapter `id`'s next command stays running.
- `{"finish": id, "throws"?}`: the held command finishes, or throws with no effect.

A new adapter starts with `positionMs` 0, `paused` true, `buffering` false, `durationMs` null, `firstFrameShown` true, `canSetRate` false, `seekAccuracyMs` 0 and `playThrows` false.

While an adapter is neither paused nor buffering, its position moves at the rate last set: the position last set + floor(elapsed × rate). With `canSetRate` false, the adapter has no `setRate`.

A command takes effect when it finishes: `play` unpauses, `pause` pauses, `seek` sets the position and `setRate` the rate. It finishes at once unless held. With `playThrows`, `play` throws and changes nothing.

These steps drive the resolver:

- `{"answer": [candidate]}` / `{"answerThrows": true}`: the pending `resolve` returns the candidates, or throws.
- `{"probeAnswer": id, "durationMs": int|null}` / `{"probeThrows": id}`: candidate `id`'s running probe returns, or throws.

A candidate is `{id, ref?, durationMs?, sizeBytes?, oshash?, name?, probe?}`. Its `ref` is a non-empty string, and is `id` when absent. `"probe": true` gives the candidate a scripted probe.

#### Settling and time

Settling runs everything that is ready without moving time, again and again until nothing is ready. That covers continuations, connections opening, commands finishing, and timers due now, `pong`s with a round trip of 0 included.

`advance` moves time like this:

1. It takes each timer due by the end in turn: earliest first, and timers due at the same time in the order they were set.
2. For each timer, it sets the clock to the timer's time and settles.
3. Then it sets the clock to the end and settles.

There is no wall-clock wait anywhere.

#### Outputs

An output is `{send?, command?, event?, resolver?, net?, done?, state?}`. An absent channel means nothing happened on it. A present one is non-empty and compared exactly, in order. The order across channels isn't pinned.

- `send`: each message the party sent as JSON, except `ping`s. Where the trace also pins the connection a message went on, the entry is `{on: url, msg}`.
- `command`: `{player, do}` when the SDK calls the adapter, plus `positionMs` for `seek` or `rate` for `setRate`. The rate is rounded to 6 decimals.
- `event`: one of the events below.
- `resolver`: `{resolve: title}` / `{resolveCancelled: title}` as the party calls or cancels the resolver, and `{probe: id}` / `{probeCancelled: id}` likewise for probes.
- `net`: `{get: url}` per GET, `{open: url}` per connect, and `{close: url, code}` when the party closes a connection that had opened. `code` is null when the party gave none.
- `done`: `{id}`, `{id, value}` or `{id, error: code, re?, message?}` once call `id` completes.
- `state`: the party's state, read after the settle.

The events are:

- `{type:"action", kind, byPeerId, by, byMe, room}`, where `by` is the entry's `peerId` or null and `room` is the snapshot's `rev`.
- `{type:"participantJoined"|"participantLeft", peerId}`.
- `{type:"error", code, re}`.
- `{type:"resolveFailed", timeout}`, where `timeout` is true for the resolve timeout and false when the resolver threw.
- `{type:"playBlocked"}`.
- `{type:"ended", reason, error}`, where `error` is its code or null.

`net` is compared per URL: each URL's entries in order, with the order between URLs not pinned.

In `done`, `value` is `create`'s code, or `choose`'s or `refresh`'s boolean. `error` is the `PartyError` code. `re` (a string or null) and `message` are compared only where the trace writes them, and `message` only for a relay's refusal. The order of a step's `done` entries isn't pinned, so compare them by `id`.

`state` holds only the members the trace lists, from these:

| Member | Value |
|---|---|
| `connection`, `peerId`, `offsetMs`, `player`, `endReason`, `isHost`, `canControl` | Its value. |
| `room` | `rev`, or null. |
| `media` | `title`, or null. |
| `match` | `{state, deltaMs?}`, or null. |
| `choice` | `{id, ref}`, or null. |
| `mismatch` | `{id, deltaMs}`, or null. |
| `candidates` | `[{id, deltaMs}]`. |
| `roomPositionMs` | Its value then. |

#### When a trace fails

Besides an output that doesn't match, a trace fails when any of these holds after its last step:

- A `random` value wasn't drawn, or one more was.
- A `resolve` or probe is unanswered.
- An adapter still runs a held command.
- A call with `id` never reported `done`.

The driver then closes the party without checking more.

The generator refuses a trace when any of these holds:

- It has an unknown step kind, call, channel, member or state field.
- Its count of outputs is other than its count of steps.
- It has a command of an unknown adapter, or a `done` of an unknown or finished call.
- Relay input passes or fails its check against its `malformed` mark.
- A draw isn't a multiple of 2^-53.
- Its name is a duplicate.

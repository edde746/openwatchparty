# Agents

This is the operating guide for automated contributors. The human process and commit style are in [CONTRIBUTING.md](CONTRIBUTING.md). An explicit user instruction in the current conversation overrides this file.

## Invariants

- A behaviour difference between the SDKs is a bug in one of them. Every SDK uses one name per concept; only the language idioms differ.
- In v1, receivers ignore unknown members at every depth, and clients ignore unknown relay message types. The relay answers an unknown client message type with `error` `unsupported`. A change that a v1 peer can't safely ignore is a new major version, served beside v1 at `/v2/…`.
- Closed enums, where a new value means v2: `phase`, `player`, `match.state`, `control` and media `kind`. Open enums: `error.code`, `closed.reason` and `action.kind`.
- Every SDK runs and passes every table in `conformance/vectors/`.
- Sources of truth:
  - [`spec/SPEC.md`](spec/SPEC.md) for the wire. It wins over the schemas, so a disagreement is a schema bug.
  - [`spec/SDK.md`](spec/SDK.md) for client behaviour and the SDK API.
  - `.github/workflows/` for checks.
  - The root [`relays.json`](relays.json) for the public relay list.

## Layout

Each area has its own workflow, and each workflow file holds its triggers:

| Area | Workflow |
|---|---|
| `spec/` | `spec.yml` |
| `relay/` and `conformance/relay/` | `relay.yml` |
| `conformance/vectors/` | `vectors.yml` |
| `sdk/typescript/` and `conformance/peer/` | `sdk-typescript.yml` |
| `sdk/dart/` | `sdk-dart.yml` |
| `sdk/kotlin/` | `sdk-kotlin.yml` |
| `Package.swift` and `sdk/swift/` | `sdk-swift.yml` |
| `website/` and the rendered docs | `website.yml`, on every pull request and push to `main` |
| The relay image | `image.yml`, on every push and pull request |
| `examples/web/` | none |

Relay and TS unit tests and the conformance `examples` scenario read `spec/examples/`. TS, Kotlin and Swift fail when a vectors table has no runner.

These files are rendered as site pages: `docs/integration.md`, `spec/SPEC.md`, `spec/SDK.md`, the SDK and relay READMEs, `docs/terms.md`, `docs/privacy.md` and `SECURITY.md`. In them:

- Use relative links and GitHub-style anchors.
- `check-links` fails on a link to a missing path, on a page that loads anything external, and on a `/relays.json` that differs by one byte from the root `relays.json`.
- `[OWNER INPUT:` anywhere in a page shows a draft banner.

## What changes together

Each row lands in one pull request.

| Change | Touches |
|---|---|
| Wire (message, field, error code, protocol limit, descriptor rule) | `spec/SPEC.md` (message tables), `spec/schema/`, `spec/examples/valid` and `invalid`, `relay/`, `conformance/relay/` scenarios, `spec/SDK.md` and `conformance/vectors/` (tools + regenerated tables), all four SDKs, their READMEs and `docs/integration.md` where apps see it |
| Client rule (SDK.md: clock, timeline, reconcile, matching, code routing, list validation, `name`/`client` checks, descriptor and snapshot checks) | `spec/SDK.md`, `conformance/vectors/tools/ref.go` and/or `cases.go`, regenerated tables, all four SDKs |
| Party behaviour (SDK.md §3–§15 rules a trace can pin) | `spec/SDK.md`, a trace in `conformance/vectors/tools/cases_party*.go` plus the regenerated `party.json`, all four SDKs; a new step or field also changes the party section of `conformance/vectors/README.md` and every SDK's trace driver |
| New vectors table | `tools/main.go` `tables()`, SDK.md §17 table, `conformance/vectors/README.md`, a runner in each SDK |
| SDK API (SDK.md §16) | `spec/SDK.md`, all four SDKs and READMEs, `docs/integration.md`, `examples/web/app.js`, `conformance/peer/owp-peer.js` |
| Public relay list | Root `relays.json` and the embedded copies, plus `go run .` in `conformance/vectors/tools` (`party.json` routes with it); merging to `main` deploys it to https://openwatch.party/relays.json |
| Relay flag | `relay/README.md` flag table, `relay/compose.yaml` if it sets it, `conformance/relay` `relayproc.go` if the suite passes it, SDK test harnesses (TS `harness.ts`, Dart `test_relay.dart`, Kotlin `Harness.kt`, Swift `RelayHarness.swift`), `sdk-typescript.yml` peer job |
| Public policy value (SPEC.md §13.2, §13.3) | Relay default (a §13.2 flag default in `relay/cmd/owp-relay/main.go`, a §13.3 constant in `relay/internal/relay/room.go`), SPEC.md table, `relay/README.md`, `conformance/relay/cmd/owp-conform/info.go`; no client change. `resumeMs` is fixed at 300000 in v1: clients assume it |
| Descriptor ID namespace | SPEC.md §10 makes it valid on the wire; for it to count in matching, also SDK.md §9.3, `conformance/vectors/tools/ref.go` plus regenerated tables, all four SDKs, `docs/integration.md` |
| Heading or link in a rendered doc | Run the website checks |

- The §13.2 policy limits are flags, and their public values are the defaults.
- The §13.3 policy durations are constants in `relay/internal/relay/room.go`, scaled only by `--timer-scale`.
- `/v1/info` reports both; no SDK reads it.
- Every flag has an `OWP_*` environment variable, which is how `relay/compose.yaml` sets them.
- `owp-peer.js` imports `sdk/typescript/src/index.ts` directly, so keep the SDK's `erasableSyntaxOnly: true`.

## Checks per area

Run from the named directory. Every SDK's integration tests need Go (they build the relay; Dart and Swift skip the build when `OWP_RELAY_BIN` names a binary).

`spec/check/` (`-v` shows why each invalid example fails; `-spec DIR` defaults to `..`):

```sh
go vet ./...
go run .
```

`relay/` (`-count=1`: tests read `spec/examples`, outside the module, which Go's test cache doesn't track):

```sh
test -z "$(gofmt -l .)" || { gofmt -l .; exit 1; }
go vet ./...
go test -race -count=1 ./...
go build -o /tmp/owp-relay ./cmd/owp-relay
```

`conformance/relay/` (starts/stops the relay itself, 3–4 minutes; `--only NAME`, `--list`, `--ports LO-HI`):

```sh
go vet ./...
go run ./cmd/owp-conform run --relay-bin /tmp/owp-relay --timer-scale 0.1
```

`conformance/vectors/tools/` (`go run .` rewrites every table):

```sh
test -z "$(gofmt -l .)" || { gofmt -l .; exit 1; }
go vet ./...
go test ./...
go run . -check
```

`sdk/typescript/` (`test:unit` needs no Go; `npm test` runs both):

```sh
npm ci
npm run typecheck
npm run test:unit
npm run test:integration
```

`conformance/peer/`, from the repo root (relay prints `listening 127.0.0.1:18200` when ready; stop it after):

```sh
(cd relay && go build -o /tmp/owp-relay ./cmd/owp-relay)
/tmp/owp-relay --addr=127.0.0.1:18200 --admin-addr= &
conformance/peer/scenario.sh http://127.0.0.1:18200
```

`sdk/dart/` (`dart test -x integration` needs no Go):

```sh
dart pub get
dart format --output=none --set-exit-if-changed .
dart analyze --fatal-infos
dart test
```

`sdk/kotlin/` (second line: CI's macOS job, needs macOS with Xcode; last line: CI's API 23 emulator job, needs a running device or emulator):

```sh
./gradlew --console=plain assemble check
./gradlew --console=plain -Popenwatchparty.android=false compileKotlinIosArm64 compileKotlinTvosArm64 macosArm64Test iosSimulatorArm64Test tvosSimulatorArm64Test
./gradlew --console=plain connectedAndroidDeviceTest
```

Repository root (Swift):

```sh
swift build
swift test
```

`website/`:

```sh
npm ci
npm run build
npm run check-links
```

`examples/web/`: `npm run build` in `sdk/typescript`, then the TypeScript checks. Image, from the root: `docker build -t owp-relay relay`.

Toolchains:

- Go 1.26.
- Node 22 or later; 22.18 or later for `owp-peer`; the website CI uses 24.
- Dart `^3.4.0`; CI runs format and analyze on stable only.
- JDK 17 or later for Kotlin (CI uses 21). The Android target is on or off per `-Popenwatchparty.android=true|false`, and otherwise on when an Android SDK is known. Apple targets build on macOS only.
- Swift 6, with Xcode on Apple platforms.

## Generated and copied files

- `conformance/vectors/*.json`: generated by `conformance/vectors/tools` (`tables()` in `main.go`); regenerate, never hand-edit; `-check` fails on any byte difference.
- Kotlin `TestData.kt`: generated by task `generateTestData` into `build/generated/testData`.
- Copies of `relays.json`: `sdk/typescript/src/public-relays.ts`, `sdk/dart/lib/src/public_relays.dart`, `sdk/kotlin/src/commonMain/kotlin/party/openwatch/PublicRelays.kt`, `sdk/swift/Sources/OpenWatchParty/PublicRelays.swift` (Kotlin and Swift compare exact text, TS/Dart parsed JSON).
- Lockfiles (`sdk/typescript/package-lock.json`, `website/package-lock.json`, each `go.sum`; `sdk/dart/pubspec.lock` is ignored) change only via their package tool, with the manifest change.

## Test ports

| What | Ports |
|---|---|
| `owp-conform run --relay-bin` | `18150-18199` (`--ports LO-HI` moves it) |
| TypeScript integration | `18200-18249` (`OWP_TEST_PORT_BASE` moves it; `relays.test.ts` binds base+24) |
| CI peer job relay | `127.0.0.1:18200` |
| Dart (unit too: 18298, 18299) | `18250-18299`, fixed |
| Kotlin `jvmTest` | `18300-18349`, fixed |
| Swift (`DefaultHTTPGetTests` too: 18390) | `18350-18399`, fixed |

## Privacy

SPEC.md §15 binds the relay and the SDKs:

- Logs hold no descriptors, names, codes, peer IDs, tokens, positions or message contents. `client` appears only in aggregates; error codes and counts are fine.
- IP addresses stay in memory only, and are never logged or stored.
- Metrics are counts.
- Room data dies with the room.
- Persisted state holds token hashes, never tokens.
- SDKs keep tokens in memory only.

The `privacy` and `state_tokens` scenarios check the log and state parts of this. Never weaken `privacy` to make it pass.

## Never run

- `owp-peer` without `--relay`, or `examples/web` with an empty Relay field: both hit the public relays.
- `owp-conform load` (default 2000 clients) or any load test against the public relays.
- A `relay-v<version>` tag push, which publishes `ghcr.io/edde746/owp-relay`, or any package publish, unless asked for a release.
- Killing a process you did not start.

## Working rules

- Stop every process you start. Local relays listen on `127.0.0.1`.
- Tests defend observable behaviour. Keep code small and boring.
- `examples/web/app.js` stays under 200 lines.
- No license headers.
- Write as [CONTRIBUTING.md](CONTRIBUTING.md#writing) describes: readable, structured prose, not compressed notes. Also: present tense, one line per paragraph or list item, no real app named, and no hardcoded deployment facts (the public relays are whatever https://openwatch.party/relays.json lists).

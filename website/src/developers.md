# For app developers

Adding Open Watch Party to an app takes an SDK (or the protocol directly), three pieces of your own code, and the rules below. The [integration guide](../../docs/integration.md) is the step-by-step path.

## What you write

- A player adapter: it reports position, duration and player state, and carries out play, pause and seek. Speed control is optional; without it the SDK keeps in step by seeking.
- A resolver: given the room's title, it returns the user's possible copies in order of preference. The SDK ranks them and reports whether the copy matches the host's.
- The interface: show the room code, open the file the SDK chooses, answer the mismatch prompt, send the user's playback controls through the party, and handle the party ending.

The SDK does the rest: it keeps the relay list, picks a relay for a new room, owns the WebSocket and reconnects, syncs the clock, holds room state, and drives the player to follow the room.

## SDKs

All four SDKs implement the same API in each language's idioms.

| SDK | Package | Runs on |
|---|---|---|
| [TypeScript](../../sdk/typescript/README.md) | npm `openwatchparty` | Browsers and Node 22 or later |
| [Dart](../../sdk/dart/README.md) | pub.dev `openwatchparty` | Dart 3.4 or later, Flutter |
| [Kotlin Multiplatform](../../sdk/kotlin/README.md) | Maven `party.openwatch:openwatchparty` | Android, JVM, iOS, macOS, tvOS |
| [Swift](../../sdk/swift/README.md) | SwiftPM `https://github.com/edde746/openwatchparty`, from `1.1.0` | iOS 16, macOS 13, tvOS 16, watchOS 9, visionOS 1, Linux |

A worked example is in [`examples/web/`](../../examples/web/), an HTML video page on the TypeScript SDK.

## Without an SDK

An app can speak the protocol directly. The [protocol](../../spec/SPEC.md) defines the wire format and relay behaviour, [client behaviour and the SDK API](../../spec/SDK.md) defines what a client does, and [`spec/schema/`](../../spec/schema/) has a JSON Schema for every message.

## Conformance

[`conformance/vectors/`](../../conformance/vectors/) holds the test tables every SDK runs. [`conformance/peer/`](../../conformance/peer/) is a scripted participant for driving your app in tests. [`conformance/relay/`](../../conformance/relay/README.md) checks a relay against the protocol; its README covers running it.

## The public relay

The SDKs ship a copy of the public relay list and refresh it only from [/relays.json](/relays.json). A relay added there reaches apps without an app release, and no relay can change the list.

The public relay is free for open-source apps, with no registration. Any other app needs written permission from contact@openwatch.party before connecting. When your app uses the public relay:

- It sends its own name and version as `client`, such as `Example/1.0`, not the SDK's.
- It tells its users that it uses the public relay, and gives them access to the [privacy notice](../../docs/privacy.md).

The [terms of use](../../docs/terms.md) state the rules in full.

## Running your own relay

Self-hosted relays have no usage restriction, and their rooms are separate from the public relays' rooms. The relay is one binary, `owp-relay`, and [Relay and self-hosting](../../relay/README.md) covers running it. Apps then point their custom relay setting at its base URL, such as `https://relay.example.com`.

## Security

Report vulnerabilities privately as described in the [security policy](../../SECURITY.md).

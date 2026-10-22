# Security

This page explains how to report a vulnerability in Open Watch Party, what is in scope, how to test safely and what happens after you report.

## How to report

Report a vulnerability privately, not in a public issue or pull request. Use [GitHub's private reporting](https://github.com/edde746/openwatchparty/security/advisories/new), or write to contact@openwatch.party.

In your report, include:

- What you found.
- The component and version.
- Steps to reproduce.
- The expected impact.

## In scope

These components are in scope:

- The relay: the code in [`relay/`](relay/README.md), the image `ghcr.io/edde746/owp-relay`, and the public relays listed at https://openwatch.party/relays.json.
- The SDKs for [TypeScript](sdk/typescript/README.md), [Dart](sdk/dart/README.md), [Kotlin](sdk/kotlin/README.md) and [Swift](sdk/swift/README.md).
- The specification in [`spec/`](spec/SPEC.md), where one of its rules makes a flaw unavoidable for any implementation.

For example, these are in scope:

- Acting in a room you are not in, resuming as another participant, or acting as host without being host.
- Getting around the relay's limits, or making it spend CPU or memory far beyond the traffic.
- Crashing the relay.
- Making the relay log names, codes, tokens, descriptors or addresses, or write tokens or addresses to disk.
- Making the relay show room data to anyone outside the room.
- Making an SDK fetch, open or run something, or crash its app, from a malicious relay or participant.

## Out of scope

These are out of scope:

- Apps built on OWP. Report those to their developers.
- Other people's relays. Report their setup to their operator.
- Denial of service by traffic volume alone.
- Scanner reports without demonstrated impact.

Behaviour that is by design is also out of scope:

- Anyone with a code can join a room until the host locks it.
- The relay reads descriptors and positions. OWP v1 has no end-to-end encryption.
- Codes can in principle be guessed. Join limits slow guessing down.
- A code carries no ownership. Once its room is deleted, anyone who joins the code with `create` gets a new room with it, as its host.

## Testing rules

Test against a relay you run yourself. The [relay guide](relay/README.md) explains how to run one.

On the public relays:

- Use only your own rooms.
- Leave other people's rooms and data alone.
- Do not load-test or flood the relays.
- Stop once you have shown the issue.

Research that follows these rules is allowed under [Acceptable use](docs/terms.md#acceptable-use) in the terms of use. See also the [privacy notice](docs/privacy.md).

## What to expect

After you report:

- You get an acknowledgement within 7 days.
- Disclosure is coordinated with you, within 90 days.
- Fixes go into the latest relay and SDK releases.
- A GitHub advisory is published, with a CVE when one applies.
- You are credited, unless you decline it.

There is no bug bounty.

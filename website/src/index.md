---
layout: home
title: Open Watch Party
titleTemplate: Watch together across different apps
hero:
  name: Open Watch Party
  text: Watch together across different apps
  tagline: Each person plays their own copy. The room keeps everyone in sync.
  actions:
    - theme: brand
      text: For app developers
      link: /developers
    - theme: alt
      text: The protocol
      link: /docs/protocol
features:
  - icon: room
    title: One room, any app
    details: Share the 6-character code, such as <code>K7M-Q2X</code>, and everyone joins the same room.
  - icon: copy
    title: Your own copy
    details: The room names a title, not a file. The protocol forbids anything in the room that locates or fetches media, and the relay never fetches media.
  - icon: free
    title: Free, no account
    details: You join with the code and a display name. The public relay is free for open-source apps.
    link: /terms
    linkText: Who may use it
  - icon: server
    title: Open source, self-hostable
    details: The protocol, relay and SDKs are open source under the Apache License 2.0. Anyone can run their own relay.
    link: /relay#self-hosting
    linkText: Run a relay
---

<div class="home-section">

## How a room works

<div class="home-steps">

1. ### Create a room

   One person creates a room and shares its code.

2. ### Pick the title

   The host picks the title and controls playback, or lets everyone control it. Each app finds its own copy of the title.

3. ### Wait for everyone

   The room waits up to 15 seconds for everyone to be ready, or up to 30 seconds after a new title, then plays.

4. ### Stay in step

   The room keeps playback in step. If your copy is a different cut, your app can let you set an offset. The room is unaffected.

</div>

<div class="home-facts">

- **Up to 32** people in a room, by default
- **5 minutes** for a dropped connection to resume
- **24 hours** at most for a room

</div>

</div>

<div class="home-section home-relay">

## What the relay holds

<div class="home-relay-cards">

- ### While the room runs

  Display names, app names, the title's details and the playback position. All deleted when the room ends.

- ### After the room ends

  Daily counts per app name, kept for 400 days. The server's daily disk backup can hold the room for up to a day.

- ### Never

  No cookies, and no logs of names, codes, titles or positions.

</div>

The [privacy notice](../../docs/privacy.md) says exactly what the public relay keeps. The public relays are the ones listed in [/relays.json](/relays.json).

</div>

<div class="home-section">
<div class="home-developers">

## Add it to your app

Use an SDK for TypeScript, Dart, Kotlin or Swift, or speak the protocol directly. Your app supplies a player adapter, a resolver and its interface; the SDK does the rest.

<div class="home-sdks">

- [TypeScript](../../sdk/typescript/README.md) Browsers and Node 22 or later
- [Dart](../../sdk/dart/README.md) Dart 3.4 or later, Flutter
- [Kotlin Multiplatform](../../sdk/kotlin/README.md) Android, JVM, iOS, macOS, tvOS
- [Swift](../../sdk/swift/README.md) iOS, macOS, tvOS, watchOS, visionOS, Linux

</div>

<div class="home-actions">

[For app developers](./developers.md) [Integration guide](../../docs/integration.md)

</div>

</div>
</div>

# Privacy notice for the public relay

Effective: 23 October 2026

## Summary

The public relay keeps people in a room in sync. This notice explains what personal data it processes to do that, how long it keeps it, who receives it and what rights you have.

In short:

- The relay processes your IP address, in memory only.
- It processes the display name and app name you send, your status in the room, and a description of what the room is watching.
- Your data in a room is deleted when you leave or the room ends. A room ends at most 24 hours after it was created.
- The hosting provider's daily backup of the server's disk can hold a copy for up to a day after that.
- There are no accounts, no cookies and no tracking.
- The operator keeps usage counts for at most 400 days. They count how often each app created, joined or resumed a room, and hold only app names and numbers.

## Who is responsible

The controller of your personal data is Edvard Wikhall, in Sweden. You can reach the controller at contact@openwatch.party.

This notice covers the public relays listed at https://openwatch.party/relays.json. It does not cover the app you use, which receives the room's data and has its own privacy policy. Nor does it cover relays that other people run from the published software.

## What the relay processes

This section describes each kind of data the relay handles: your connection, you in a room, what the room watches, saved rooms, and the counts and logs the relay keeps.

### Your connection

When your app connects, the relay sees the IP address of the connection. It uses the address for its limits:

- An IPv4 address is used whole.
- For an IPv6 address, only its /64, /56 and /48 networks are used.

The full address stays only in the open connection, and goes when the connection closes. Addresses are held in memory only. They are never written to disk or to a log, and a restart erases them.

Your app also sends clock pings, and the relay answers with its clock time. Nothing is kept from these.

To create a room, your app connects to every public relay at once. So every public relay briefly sees your address and pings, under the same rules. Joining or resuming a room connects only to the relay that holds the room.

The apps fetch the current relay list from https://openwatch.party/relays.json when they first connect. They fetch it again on a later connection once a day has passed, and when a room code points to no known relay.

### You in a room

Your app sends the relay two names:

- A display name of 1 to 32 characters. It can be almost any text, but control and direction-control characters are refused. It does not have to be unique or your real name.
- A `client` name that names the app and its version, such as `Example/1.0`.

The relay gives you a peer ID and a random resume token. The token is sent to you when you join, and again each time your app resumes. The relay keeps only its SHA-256 hash.

While you are in the room, the relay holds your status:

- Whether you are the host.
- Whether you are connected.
- Your player state.
- Whether your copy matches the room's title, and by how much its duration differs.
- Your playback offset.
- Optionally, your connection's round-trip time. It is held in memory only, to time the start of playback.

Nobody in the room receives your IP address or your round-trip time.

### What the room watches

The host's app describes what the room is watching in a media descriptor. The descriptor holds:

- The kind of title: a movie, an episode or a video.
- The title and year.
- Catalogue IDs, such as IMDb, TMDB or TVDB.
- For an episode, the series title, year and IDs, and the season and episode.
- The duration.
- Optional file hints: the file's size, a file hash, and the file name without directories.
- Optional app-private hints.
- Any other fields the app adds.

The descriptor must not contain media itself, or anything that locates or fetches media. Everyone in the room receives the descriptor unchanged.

The relay also holds the room's playback state:

- The position.
- Whether it is playing, paused or waiting.
- The playback rate.
- Whether the host or everyone may control playback.
- Whether the room is locked.

With each change, the relay tells everyone in the room which action caused it and who took it. It keeps neither.

### Saved rooms

So that rooms survive a restart, the relay saves each room to its disk. A saved room holds:

- The room's code, creation time and media descriptor.
- Its position, rate, control setting and locked flag.
- Each participant's peer ID, token hash, display name, `client` name, host flag, match and offset.

A saved room never holds the tokens themselves, IP addresses or round-trip times.

If a relay restarts, it restores the rooms younger than 24 hours. A restored room is paused, and everyone in it is disconnected and able to resume for 5 minutes.

The hosting provider backs up each server's disk once a day, and keeps only the latest backup. So what is on the disk, saved rooms included, can remain in that backup for up to a day after it leaves the disk. The operator makes no other copy.

### Usage counts, live counts and logs

Each relay counts the requests it accepts to create, join or resume a room, for each UTC day and each app name. These daily counts hold no IP addresses, display names, versions, room codes, tokens or titles.

The app name is the part of the `client` name before the `/`. The [terms of use](terms.md) require it to name the app. The relay stores it as the app sent it.

The relay also keeps live counts of connections, rooms, participants, messages and errors. Participants are also counted per app name. These live counts are available only on the server itself, and the public deployment runs no service that collects them.

The relay's logs record only events about the relay itself, such as starting, certificate expiry, and errors reading or writing its own files. On the public relays, the logs are rotated and firewall logging is off.

## How long it is kept

This section says how long each kind of data is kept, and where.

A room is deleted at the first of these moments:

- The host ends it.
- Its last participant leaves or is removed.
- Nobody has been connected for 5 minutes.
- 24 hours have passed since it was created.

A restart does not extend that.

Your time in a room ends when you leave, when the host removes you, when you have stayed disconnected for 5 minutes, or when the room is deleted.

| Data | Where | Kept until |
| --- | --- | --- |
| Your full IP address, in an open connection | Memory | That connection closes. |
| Your IPv4 address, or your IPv6 /64, /56 and /48, for the join-lookup limit | Memory | At most about 80 seconds after the last join attempt. |
| The same, as the creator of a room | Memory | The room is deleted, at most 24 hours. A restart drops it. |
| Your IPv4 address or IPv6 /64, for the app names it added to the day's usage counts | Memory | Within a minute after that UTC day ends, or a restart. |
| Display name, `client` name, peer ID, token hash, host flag, match and offset | Memory and disk | Your time in the room ends. It is off the disk within 5 seconds, and anything left in memory goes when your connection closes. |
| Player state and round-trip time | Memory | Your time in the room ends. Anything left goes when your connection closes. |
| Media descriptor | Memory and disk | The host replaces or clears it, or the room is deleted. |
| Playback state | Memory and disk | The room is deleted. Anything left in memory goes when the connections close. |
| Daily counts per app name | Disk | At most 400 days after the day they count. |
| Anything on the disk above | The hosting provider's backup of the disk | The next daily backup replaces it, at most about a day after it leaves the disk. |

## What the relay doesn't do

Some things the relay never does:

- There are no accounts or sign-in.
- The relay sets no cookies. It keeps none of the cookies, User-Agent or Origin values a connection may carry.
- There is no tracking, analytics or advertising.
- The relay makes no outbound connections, except to its certificate authority to obtain and renew its certificate.
- The relay never receives, fetches or links to video. Each person plays their own copy.
- There is no profiling, and no automated decision with legal or similar effect on you. Apart from protocol errors, the only automatic refusals are the published limits and the list of refused app names.
- Your data is not sold, and it is shared only as described below.

## Who receives it

Your data reaches these recipients, and no others:

- Everyone in your room, and their apps. They receive your peer ID, display name, `client` name, host and connection status, player state, match and offset, together with the media descriptor and playback state.
- Hosting providers. They host the servers, process the traffic to them and hold the daily backup of each server's disk.
- Authorities, only when the law requires it. They then receive only what still exists. There is nothing about a room deleted more than a day ago, and no address that is no longer in memory.

Anyone who has the room code can join the room, up to 32 participants, until the host locks it.

## Where it is processed

The public relays are those listed in https://openwatch.party/relays.json. Each entry names where that relay runs.

A room lives its whole life on one relay. The relays share no data and never contact each other. Joining a room held on another relay therefore sends your data to that relay.

For a relay outside the EEA, the safeguard relied on is the standard contractual clauses adopted by the European Commission, under GDPR Article 46(2)(c). These clauses are part of the hosting provider's data processing agreement. You can ask for a copy at contact@openwatch.party.

## Purposes and legal bases

The relay processes your data for three purposes, each with its own legal basis. References are to the EU and UK GDPR.

| Purpose | Legal basis |
| --- | --- |
| Running the room you ask to be in, covering the room data described above | Necessary to provide the service you request under the [terms of use](terms.md), Article 6(1)(b) |
| Applying limits against abuse, using IP addresses in memory only | The operator's legitimate interest, Article 6(1)(f) |
| Complying with the law, where a legal obligation requires a disclosure | Article 6(1)(c) |

The operator's legitimate interest is in protecting the public relay, the people using it and the accuracy of its usage counts from abuse and overload.

The daily counts per app name count the use of apps, not people, because the terms require an app name to name an app.

## Your rights

You have the rights of access, rectification, erasure, restriction and portability. You also have the right to object to processing based on legitimate interest.

Send any request to contact@openwatch.party. It will be answered within one month.

In practice, your app already receives everything the room holds about you. Leaving a room removes you from it, and the host ending a room deletes it.

When a room ends, its names, app names, token hashes and descriptor are cleared. What is left of the room in memory goes when the connections close. After that, at most an IP address remains in memory for the limits described above, with no name, room or title attached. The daily disk backup can hold the room for up to a day.

You may complain to any of these supervisory authorities:

- Integritetsskyddsmyndigheten (IMY), the Swedish Authority for Privacy Protection, at [imy.se/en](https://www.imy.se/en/).
- The supervisory authority where you live or work.
- The supervisory authority where you believe the infringement took place.

## Security

Connections use TLS 1.2 or later. The public relays terminate the encrypted connection themselves, so no proxy or content delivery network in between decrypts your traffic.

The relay is not end-to-end encrypted, though. It reads the room's data in order to run the room. If you do not want an operator to be able to read what a room watches, you can use a [relay you run yourself](../relay/README.md).

Saved room files are readable only by the relay's user account on the server. How to report a vulnerability is described in [SECURITY.md](../SECURITY.md).

## Changes to this notice

This notice is updated when what the relay processes changes. Every change is published in this file, in the repository at [github.com/edde746/openwatchparty](https://github.com/edde746/openwatchparty). The effective date at the top is updated with it.

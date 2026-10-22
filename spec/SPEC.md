# Open Watch Party protocol v1

This document defines the Open Watch Party (OWP) v1 wire protocol and the behaviour of a relay. It is the reference for anyone who builds a relay, or a client that speaks the protocol directly.

The JSON Schemas in `schema/` define the shape of each message. This document defines what the messages mean, and it wins where the two disagree. Client behaviour beyond the wire is in `SDK.md`. A relay built from this document, and a client built from it and `SDK.md`, interoperate with every conforming implementation.

The key words MUST, MUST NOT, REQUIRED, SHALL, SHALL NOT, SHOULD, SHOULD NOT, RECOMMENDED, MAY and OPTIONAL are to be interpreted as described in RFC 2119 and RFC 8174 when, and only when, they appear in all capitals.

## Contents

1. Overview (non-normative)
2. Conventions
3. Endpoints and transport
4. Versioning and extension
5. Relays, room codes and routing
6. Messages: common rules
7. Client → relay messages
8. Relay → client messages
9. Room snapshot
10. Media descriptor
11. Rooms and roles
12. Playback state machine
13. Limits and policy
14. Persistence and restart
15. Privacy
16. Client behaviour
- Appendix A. Error codes
- Appendix B. WebSocket close codes and HTTP refusals

## 1. Overview (non-normative)

This section sketches how the protocol works before the rules begin.

People in a room watch one title together. Each person plays their own copy in their own app, and the room keeps the copies in sync.

The relay owns the timeline, the readiness barrier and the roles. Clients follow the relay, so a faulty client desyncs only its own user. Each participant has one WebSocket, which carries JSON text frames.

The relay's clock is the room's clock, and clients estimate it from `ping` and `pong`. Every change sends the whole room. The room carries the media descriptor only when the descriptor changes.

The descriptor names a title, never a source, and each client finds its own file. Positions are in the reference file's timeline. If there is a local offset, only the client applies it. There are no accounts.

A room lives on one relay for its whole life. The first character of the room's code names that relay.

## 2. Conventions

This section defines the terms, value types and notation that the rest of the document uses.

### Terms

- A *relay* implements §3–§15.
- A *client* is an app that connects to a relay.
- A *connection* is one WebSocket.
- A *participant* is a person in a room, identified by a peer ID. A participant is *connected* while a connection is attached to it, and *disconnected* otherwise.
- A *member* is a connection attached to a participant. Any other connection is *outside a room*.
- The *host* has host rights. A *controller* may send `play`, `pause`, `seek` and `rate`. Both roles are defined in §11.3.

### Numbers and strings

An *int* is a JSON number with an integral value and a magnitude of at most 2^53 − 1 (9007199254740991). Senders MUST write ints without a fraction or an exponent. Receivers MUST accept any integral number in range, so `3.0` is the int 3. A *number* is any JSON number.

String length counts Unicode code points. *Bytes* are the UTF-8 bytes of the JSON text as received or sent. Every message is valid UTF-8, as §3.1 requires.

### Forbidden characters

A *forbidden character* is a character of either of these kinds:

- General category Cc, which is U+0000–U+001F and U+007F–U+009F.
- The Bidi_Control property, which is U+061C, U+200E, U+200F, U+202A–U+202E and U+2066–U+2069.

A forbidden character is allowed in no `name`, `client`, relay `name`, descriptor `title`, `series.title` or `file.name`.

### Time

Times and durations are int milliseconds. *Relay time* is the relay's monotonic clock, counted from an origin the relay chooses, typically process start. Relay time is meaningless across restarts. `serverTime` and `atMs` are relay times.

*Media time* is a position in the reference file. `positionMs` and `durationMs` are media times.

### Notation

Optional fields are marked `?`. An optional field is either absent or a value of the field's type. `null` is a value only where the type lists it, and elsewhere it is a wrong type.

A policy value is written as its `/v1/info` key followed by the public value in parentheses, as in "the stall grace (`stallGraceMs`, 500)".

## 3. Endpoints and transport

A relay serves the protocol on one WebSocket endpoint and two plain HTTP endpoints. This section defines all three, and the limits a relay applies to connections.

### 3.1 The WebSocket: `GET /v1/ws`

The protocol runs over WebSocket (RFC 6455) at the path `/v1/ws`. Public relays use `wss://`. A relay MUST NOT refuse a connection because of its `Origin` header.

#### Messages

Each message is one JSON object, encoded in UTF-8 and sent in one WebSocket text message. The relay reassembles fragments first, and the limits apply to the whole message.

Three rules cover encoding, size and binary messages:

- Clients MUST send valid UTF-8 (RFC 3629). A text message that is invalid anywhere MUST make the relay close the connection with 1007, without an `error`. This includes the parts the relay passes through: the `media` descriptor and the `ping` `id`.
- A message over `maxMessageBytes` (8192) MUST make the relay close the connection with 1009, again without an `error`.
- A binary message gets `invalid_message`, with `re` null.

In the other direction, clients MUST accept relay messages of at least 16384 bytes. Each relay message is one JSON object in one text message.

#### Message rate

Each connection has a token bucket. The bucket starts full at `messageBurst` (30) tokens and refills at `messagesPerSecond` (10) tokens per second. Every data message takes one token, whether it is valid or not. Control frames take none.

When the bucket is empty, the relay drops the message and answers it with `rate_limited`. The answer's `re` is set as §6.2 says. The connection stays open.

#### Keepalive

The relay sends a WebSocket ping frame every 30 s on every connection. It closes a connection from which no frame of any kind, whether data, ping or pong, has arrived for 60 s. Both values are protocol constants and are never scaled, not even by the test mode of §13.3. Client WebSocket stacks answer ping frames themselves.

#### Connection limits

The relay refuses a connection over either of two limits at upgrade, without upgrading it:

- When one of the client IP's address groups already holds `maxConnectionsPerIp` (10) × its group factor open connections, the answer is HTTP 429.
- When the relay already holds `maxConnections` (10000) open connections, the answer is HTTP 503.

When both apply, the answer is 503.

#### The client IP

The client IP is the TCP peer address. Behind a proxy that the operator configures as trusted, it is the first `X-Forwarded-For` address instead, falling back to the peer address when that header is missing. A relay MUST NOT trust that header unless it is configured to.

Behind a trusted proxy, an upgrade gets HTTP 400 when the first `X-Forwarded-For` value is not an IP address or carries a zone. An IPv4-mapped IPv6 address counts as the IPv4 address.

#### Address groups

The relay keeps its per-IP limits per address group. These limits cover connections, `join` lookups under §7.2 and rooms under §11.10. Each group has a factor:

|address|group|factor|
|---|---|---|
|IPv4|the address itself|1|
|IPv6|its /64|1|
|IPv6|its /56|4, the key `ipv6Factor56`|
|IPv6|its /48|16, the key `ipv6Factor48`|

A group's limit is the per-IP value × its factor. The factor multiplies the bucket size and the refill rate alike. A request counts in every group only when it is within the limit in all of them. A refused request counts in none.

#### Shutdown

On graceful shutdown, the relay first writes its persisted state, if it keeps one as §14 allows. It then closes every connection with status 1001.

### 3.2 `GET /v1/info`

`GET /v1/info` returns 200 with `Content-Type: application/json` and the body defined in §5.3. The response MUST carry `Access-Control-Allow-Origin: *`.

### 3.3 `GET /health`

While the relay accepts connections, `GET /health` returns 200 with `Content-Type: text/plain` and the body `ok`. The response MUST carry `Access-Control-Allow-Origin: *`.

## 4. Versioning and extension

This section says how the protocol may change within v1, and what a receiver does with values it doesn't know.

The major version is in the path, as in `/v1/ws` and `/v1/info`. A change that a v1 peer cannot safely ignore is a new major version. The same relay serves it beside v1, at `/v2/…`.

### Additive changes

Within v1, changes are additive. They add new optional fields, message types, descriptor ID namespaces, keys in `/v1/info`, `limits` and `policy`, and values of the open enums `error.code`, `closed.reason` and `action.kind`.

Every receiver MUST ignore unknown members at every depth. A client MUST ignore a relay message of unknown `type`. It MUST treat the other unknown values like this:

- An unknown `error.code` is a refusal of the message named in `re`.
- An unknown `closed.reason` is the end of the client's membership.
- An unknown `action.kind` is an action the client does not describe.

The relay MUST answer a client message of an unknown type with `unsupported`, as §6.2 says.

The other enums are closed: `phase`, `player`, `match.state`, `control` and the media `kind`. A new value in one of them is v2.

### JSON text rules

Two rules apply to the JSON text itself:

- Senders MUST NOT send two members with the same name in one object.
- Senders MUST NOT send a string escape that yields an unpaired surrogate. A `\uXXXX` escape in the range `\uD800`–`\uDFFF` is valid only as a high surrogate (`\uD800`–`\uDBFF`) directly followed by a low-surrogate escape (`\uDC00`–`\uDFFF`).

The relay refuses a message that breaks either rule with `invalid_message`, at the steps §6.2 gives.

## 5. Relays, room codes and routing

This section defines room codes, the public relay list, the `/v1/info` body and how a client picks the relay for each request.

### 5.1 Room codes

A room code is 6 symbols from the alphabet `23456789ABCDEFGHJKMNPQRSTUVWXYZ`. The alphabet has 31 symbols, with no 0, 1, I, L or O.

The relay always sends codes in upper case. Clients MAY display them in groups, as in `K7M-Q2X`. A client MAY send a code in `join` or `resume` as the user typed it.

The relay normalizes a received code in three steps:

1. It removes the separators U+0020, U+0009, U+000A, U+000D, U+00A0, U+2007, U+202F, U+002D, U+2010–U+2014 and U+2212.
2. It converts ASCII `a`–`z` to upper case.
3. It looks the result up exactly. A result that is not 6 alphabet symbols names no room.

When `create` makes a room, the relay draws its code with a CSPRNG. It draws the first symbol uniformly from its creation prefixes, which §5.2 defines, and the rest uniformly from the alphabet. The code MUST differ from the codes of every room the relay holds, including rooms restored under §14. The relay redraws on a collision. A `join` that makes a room gives it the join's code instead, as §7.2 describes.

A relay MUST answer `join` and `resume` for every room it holds, whatever the code's first symbol.

### 5.2 The relay list

The relay list names the public relays. It is the file `relays.json` at the repository root, served unchanged at `https://openwatch.party/relays.json`. Its schema is `relays.schema.json`.

Relays publish no list of their own. `/v1/info` describes only the relay that serves it, as §5.3 shows.

This example list of three relays leaves the symbols `QRSTUVWXYZ` to no relay:

```json
{
  "relays": [
    { "id": "a", "name": "Relay A", "url": "wss://a.relay.example/v1/ws", "prefixes": "2345678" },
    { "id": "b", "name": "Relay B", "url": "wss://b.relay.example/v1/ws", "prefixes": "9ABCDEF" },
    { "id": "c", "name": "Relay C", "url": "wss://c.relay.example/v1/ws", "prefixes": "GHJKMNP" }
  ]
}
```

|field|type|rule|
|---|---|---|
|`relays`|array of relay entries|1 or more entries, in the order clients try them for `create` under §5.4|
|`relays[].id`|string|1–32 characters of `a`–`z`, `0`–`9` and `-`, starting with a letter or digit, unique in the list and never reused for another relay|
|`relays[].name`|string|1–64 characters with no forbidden characters (§2), for display|
|`relays[].url`|string|the URL of the relay's WebSocket endpoint, written as the rules below the table say|
|`relays[].prefixes`|string|1–31 distinct upper-case symbols of the code alphabet, which are the first symbols this relay owns|

#### The `url` field

A relay's `url` follows these rules:

- It is an absolute `wss://` URL that ends in `/v1/ws`, without a query or fragment.
- It may carry a path prefix before that ending, as in `wss://h/owp/v1/ws`.
- It holds printable ASCII only, U+0021–U+007E, so it has no spaces, control characters or non-ASCII characters. A non-ASCII host name is written in its `xn--` form.
- It has no `/` in the host, and no `?` or `#` anywhere.

A relay's HTTP endpoints derive from its WebSocket URL. Replace `wss://` with `https://`, or `ws://` with `http://`, and replace the trailing `/v1/ws` with `/v1/info` or `/health`.

#### Prefixes

No symbol may appear in the `prefixes` of two relays. A list may leave symbols unassigned. No relay creates codes with an unassigned symbol, and a later list may give it to a new relay. A code whose first symbol no relay of the list owns cannot be joined through the list.

A relay creates codes only with its *creation prefixes*. These are distinct symbols of the code alphabet, set by the relay's operator, and all 31 by default. A public relay's operator MUST keep its creation prefixes within its entry's `prefixes`. The relay doesn't check this.

Creation prefixes differ from the list's `prefixes` only while symbols move between relays. The old relay first stops creating codes with the moving symbols. At least `maxRoomAgeMs` (86400000) later, the list moves them.

### 5.3 `/v1/info` body

This is the body that `GET /v1/info` returns. Its schema is `info.schema.json`.

```json
{
  "protocol": 1,
  "id": "b",
  "limits": {
    "maxParticipants": 8, "maxMessageBytes": 8192, "maxMediaBytes": 4096, "maxExtBytes": 1024,
    "maxNameLength": 32, "maxClientLength": 64, "minRate": 0.5, "maxRate": 2.0,
    "maxConnections": 10000, "maxConnectionsPerIp": 10, "maxRooms": 10000, "maxRoomsPerIp": 3,
    "ipv6Factor56": 4, "ipv6Factor48": 16, "messageBurst": 30, "messagesPerSecond": 10,
    "joinBurst": 20, "joinsPerSecond": 1, "joinBurstTotal": 2000, "joinsPerSecondTotal": 200
  },
  "policy": {
    "stallGraceMs": 500, "stallReleaseMs": 400, "waitMs": 15000, "mediaWaitMs": 30000, "exclusionMs": 60000,
    "startDelayMinMs": 750, "startDelayMaxMs": 2000, "startDelayRttFactor": 1.5, "defaultRttMs": 500,
    "seekCoalesceMs": 200, "pauseToleranceMs": 2000, "hostPromotionMs": 30000, "resumeMs": 300000,
    "maxRoomAgeMs": 86400000
  }
}
```

|field|type|rule|
|---|---|---|
|`protocol`|int|`1`|
|`id`|string|this relay's ID, set by its operator, which for a public relay is its `id` in the relay list (§5.2)|
|`limits`|object|every key of §13.1 and §13.2 with this relay's value|
|`policy`|object|every key of §13.3 with this relay's value|

A relay MUST report every key listed in §13. The `policy` values are the durations in effect, after any test-mode scaling under §13.3.

`/v1/info` serves operators and test suites. Clients read nothing from it, as §13 explains.

### 5.4 Choosing the relay (client)

This section says which relay a client connects to for each request. A client takes its relays from one of two sources: the public list or a custom source.

#### The public list

With the public list, the client ships a copy of the repository's `relays.json`. It refreshes that copy only from `https://openwatch.party/relays.json`. It uses a fetched list only when the list is valid by §5.2, and otherwise keeps the one it has.

*Refreshing* is reading that URL again. `SDK.md` §3 defines when a client refreshes, and the HTTP limits.

#### A custom source

A custom source is one self-hosted relay's base URL, such as `https://relay.example.com`. The client uses that relay alone for every `create`, `join` and `resume`, and fetches no list. The relay's WebSocket URL is the base URL with `https://` changed to `wss://`, or `http://` to `ws://`, plus `/v1/ws`.

#### Creating

To create with several relays, the client connects to all of them at once. It creates on the first one to answer its clock sync, as `SDK.md` §6 describes.

On `relay_full`, the client creates on the first relay it has not tried, in list order. It fails when every relay answered `relay_full`.

#### Joining and resuming

To join, the client connects to the relay whose `prefixes` contain the code's first symbol, compared case-insensitively. To resume, it goes back to the relay the party joined on.

When no relay owns the first symbol, the client refreshes the list, at most once per join. If still no relay owns it, joining fails as `room_not_found` would.

On `room_not_found` answering a `join`, the client refreshes the list within that same allowance. If the first symbol now belongs to a relay with another `url`, the client retries the join there once.

A custom source's relay gets every `join`, whatever the first symbol, and its `room_not_found` is final.

## 6. Messages: common rules

This section gives the rules that every message follows, and the order in which the relay validates a client's message.

### 6.1 Envelope

Every message, in both directions, is a JSON object with a string member `type` that names the message. `type` values are case-sensitive.

Each message type in §7 and §8 has a schema, `schema/<type>.schema.json`. Shared definitions are in `common.schema.json`.

### 6.2 Validation and refusals

The relay has already checked size and UTF-8 under §3.1. It then checks each client message in this order, and stops at the first refusal:

1. Rate limit. A message that finds the connection's bucket of §3.1 empty gets `rate_limited`.
2. Envelope. The message gets `invalid_message` in any of these cases:
   - It is not valid JSON, or not an object.
   - It has no string `type` member, or it has more than one `type` member.
   - Its `type` value has an unpaired surrogate escape (§4).
3. Type. A type name that is not defined for client → relay messages gets `unsupported`. The names of relay → client types count as unknown.
4. Shape. The message gets `invalid_message` when it breaks any of the shape rules listed below this list.
5. Membership. A room command from a connection outside a room gets `not_in_room`. Room commands are every type except `create`, `join`, `resume` and `ping`. A `create`, `join` or `resume` from a member gets `already_in_room`.
6. The message-specific checks of §7 run last, in the order each message's section lists them: access, per-IP allowances, permission (`forbidden`), then room state.

#### Shape rules

At step 4, a message breaks the shape rules when any of these holds:

- A required field is missing, or a field has the wrong type.
- A value is out of range, or a string breaks its rule.
- A closed enum has an unknown value.
- An object at any depth, `media` and `ext` included, has two members of the same name, which §4 forbids.
- A string anywhere else, member names included, has an unpaired surrogate escape, which §4 also forbids.

Member names are compared after decoding escapes, so `"a"` and `"\u0061"` are the same name. Unknown members are ignored, never refused.

#### Refusals and replies

A refused message changes no room state and gets exactly one `error`, the message of §8.5. The error's `re` is the message's `type` when the envelope passes step 2, and null otherwise. This holds for every refusal, `rate_limited` included. A refusal never closes the connection.

An accepted message gets no acknowledgement. Its effects are the reply: `joined`, `pong`, `room` or `closed`, as §7 defines for each message. `leave` gets no reply.

### 6.3 `name` and `client`

`name` is a display name of 1–32 characters (`maxNameLength`) with no forbidden characters (§2). It need not be unique, and the relay stores it unchanged.

`client` names the app and its version as `<app>/<version>`, as in `Example/1.31.0`. It is 1–64 characters in all (`maxClientLength`), with no forbidden characters. Its two parts are split at the first `/`:

- `<app>` is everything before that first slash, and is at least 1 character long.
- `<version>` is everything after it, and is at least 1 character long. It may contain more slashes.

A `client` without a slash, or with an empty part, gets `invalid_message`.

`<app>` MUST name the real app sending the message. Every participant sees `client`. The relay's deny list of §11.9, its usage counts of §15 and the terms of use are all keyed on the app name.

## 7. Client → relay messages

This section defines each message a client sends: its fields, who may send it, its refusals and its effect.

Every field is required unless marked `?`. Each message's section lists its fields, then its refusals in check order, then its effect. The refusals listed come after §6.2 steps 1–5. The snapshot each message causes follows §9.2.

|type|fields|allowed for|
|---|---|---|
|`create`|`name`, `client`|a connection outside a room|
|`join`|`code`, `name`, `client`, `create`?|a connection outside a room|
|`resume`|`code`, `peerId`, `token`|a connection outside a room, for the participant it names|
|`ping`|`id`|any connection|
|`status`|`epoch`, `player`, `match`, `offsetMs`, `rttMs`?|members|
|`play`|—|controllers|
|`pause`|`positionMs`?|controllers|
|`seek`|`positionMs`|controllers|
|`rate`|`rate`|controllers|
|`setMedia`|`media`, `positionMs`?|the host|
|`clearMedia`|—|the host|
|`setControl`|`mode`|the host|
|`transferHost`|`peerId`|the host|
|`kick`|`peerId`|the host|
|`lock`|`locked`|the host|
|`leave`|—|members|
|`end`|—|the host|

### 7.1 `create`

|field|type|rule|
|---|---|---|
|`name`|string|§6.3|
|`client`|string|§6.3|

Refusals, in order:

1. `client_blocked` when the deny list of §11.9 refuses the app.
2. `rate_limited` when an address group of the client IP is the creator of its limit of rooms (§11.10).
3. `relay_full` when the relay holds `maxRooms` (10000) rooms, as §11.10 describes.

If no refusal applies, the relay makes a new room with a new code (§5.1) and the defaults of §9.3. The sender becomes the room's first participant (§11.1) and its host. The relay sends `joined` to the sender with the first snapshot, whose `rev` is 1.

### 7.2 `join`

|field|type|rule|
|---|---|---|
|`code`|string|any string, matched as §5.1 says|
|`name`|string|§6.3|
|`client`|string|§6.3|
|`create`?|boolean|`true` asks the relay to make a room with this code when no room has it|

Refusals, in order:

1. `client_blocked` when the deny list of §11.9 refuses the app.
2. `rate_limited` when the client IP or the relay has no join allowance left, as described below.
3. `room_not_found` when no room has this code, unless `create` is `true`. Then the relay makes the room, as described below.
4. `room_full` when the room has `maxParticipants` (8) participants, disconnected ones included.
5. `room_locked` when the room is locked.

The join allowance works with token buckets:

- Each address group has a bucket of `joinBurst` (20) tokens, refilled at `joinsPerSecond` (1) per second, both times the group's factor.
- The relay has one bucket of `joinBurstTotal` (2000) tokens, refilled at `joinsPerSecondTotal` (200) per second.

A `join` that reaches step 2 takes one token from each bucket when every one has a token, whether or not the code exists. Otherwise it takes none and is refused.

If no refusal applies, the sender becomes a new participant at the end of the join order (§11.1), and the host promotion of §11.4 runs. The relay sends `joined` to the sender and a snapshot to the other connected members.

#### Making the room

When no room has the code and `create` is `true`, the relay makes a room with that code, unless one of these refusals applies, in order:

1. `room_not_found` when the normalized code is not 6 alphabet symbols, or its first symbol is not one of the relay's creation prefixes (§5.2).
2. `rate_limited` when an address group of the client IP is the creator of its limit of rooms (§11.10).
3. `relay_full` when the relay holds `maxRooms` (10000) rooms, as §11.10 describes.

If no refusal applies, the relay makes the room as `create` does in §7.1, with the join's normalized code. The sender becomes the room's first participant and its host. The relay sends `joined` to the sender with the first snapshot, whose `rev` is 1.

Looking up the code and making the room happen in one step. When two `join`s with `create` arrive for the same free code, one makes the room and the other joins it. A code is free from the moment §11.8 deletes its room, before any member reads the room's `closed`.

A relay that predates `create` ignores the field, as §4 requires, and answers `room_not_found`.

### 7.3 `resume`

|field|type|rule|
|---|---|---|
|`code`|string|matched as §5.1 says|
|`peerId`|string|any string|
|`token`|string|any string|

Refusals, in order:

1. `resume_failed` when no room has this code, when the room has no participant with this `peerId`, or when `token` is not that participant's token.
2. `client_blocked` when the deny list of §11.9 refuses the participant's stored `client`.

A participant is missing when it never existed, or when it left, was kicked or was removed. The relay compares tokens in constant time, and the refusal is the same in each case of step 1. `resume` never takes a join token from the buckets of §7.2.

If no refusal applies, the relay does this:

- If the participant is connected, the relay closes its old connection with status 4000 and sends that connection nothing else.
- The sender's connection becomes the participant's.
- The participant keeps its peer ID, token, name, client, place in the join order, host flag, `player`, `match`, `offsetMs`, reported `rttMs`, exclusion and status epoch.
- Its reservation timer stops (§11.5).
- If it is the host, the host's absence ends (§11.4). Otherwise, host promotion runs.
- `waitingOn` is not recomputed. The resumer's next `status` does that.

The relay sends `joined` to the sender and a snapshot to the other connected members, even when no snapshot field changed.

### 7.4 `ping`

|field|type|rule|
|---|---|---|
|`id`|string or number|chosen by the client and echoed unchanged|

`ping` is allowed on any connection, in a room or not. It has no refusals beyond §6.2 steps 1–4, and no effect on the room.

At once, the relay answers the sender with the `pong` of §8.4. It carries the same `id` value, with its bytes echoed, and a `serverTime` read as late as possible before sending.

### 7.5 `status`

|field|type|rule|
|---|---|---|
|`epoch`|int|≥ 0: the room epoch this report is about|
|`player`|string|one of the four values in the first table below|
|`match`|object or null|null while the client hasn't tried to match, and otherwise the match object below|
|`match.state`|string|one of the four states in the second table below|
|`match.deltaMs`?|int|own file's duration − `durationMs` of the media, positive when the own file is longer|
|`offsetMs`|int|−604800000 ≤ offsetMs ≤ 604800000 (± 7 days, §13.1): the local offset this participant plays with, of either sign, as §16.4 defines it|
|`rttMs`?|int|≥ 0: the client's current best round trip to the relay, as §16.2 defines it|

#### `player` values

|value|meaning|
|---|---|
|`idle`|not in the player, because there is no media or the user is not playing it|
|`loading`|opening, searching, seeking, waiting on the user's choice after a mismatch, or waiting for the user to start a player that refused to play|
|`ready`|close enough to the room's position to follow it without seeking, as §16.4 says, and able to play at once|
|`buffering`|was playing and is now stalled|

#### `match.state` values

|value|meaning|
|---|---|
|`searching`|candidates are being tried, or the user has not decided about a mismatch|
|`matched`|the chosen file's duration is within 1500 ms of `durationMs`|
|`mismatched`|the user plays a file outside that margin, with an offset|
|`unavailable`|no file plays, because none was found or the user declined|

`deltaMs` may accompany any state. It gives the difference for the file chosen or offered.

#### Effect

The relay stores `player`, `match` and `offsetMs`, and it stores `epoch` as the participant's *status epoch*. It stores `rttMs` when present, and otherwise keeps the last one. The peer-event rules of §12.5 then run, and stall handling follows §12.9.

A participant has *reported unavailable for the current epoch* when its latest status has an `epoch` equal to the room's and a `match.state` of `unavailable`.

The relay sends a snapshot when any snapshot field changed. That field can be this participant's `player`, `match` or `offsetMs`, or a room field that §12.5 changed. A status that changes only `epoch` or `rttMs` sends nothing.

### 7.6 `play`

`play` has no fields.

Refusals, in order:

1. `forbidden` when the sender is not a controller.
2. `invalid_message` when the room has no media, which means `phase` is `idle`.

In `paused`, the relay sets the action `play` by the sender (§9.1). The room then enters waiting at `positionMs` with timeout `waitMs` (15000), as §12.4 describes. In `playing` or `waiting`, nothing happens.

### 7.7 `pause`

|field|type|rule|
|---|---|---|
|`positionMs`?|int|≥ 0: where the sender's player stopped, in media time|

Refusals, in order:

1. `forbidden` when the sender is not a controller.
2. `invalid_message` when the room has no media.

In `playing` or `waiting`, the room pauses as §12.1 defines, and the relay sets the action `pause` by the sender. The new `positionMs` depends on the field:

- When the field is present and `|positionMs − P(now)| ≤ pauseToleranceMs` (2000), the new position is `min(positionMs, durationMs)`.
- Otherwise, the new position is `P(now)`.

In `paused`, nothing happens.

### 7.8 `seek`

|field|type|rule|
|---|---|---|
|`positionMs`|int|≥ 0: the target in media time|

Refusals, in order:

1. `forbidden` when the sender is not a controller.
2. `invalid_message` when the room has no media.

The target is `min(positionMs, durationMs)`. The relay applies the seek now, or holds it under §12.8. Applying a seek first sets the action `seek` by its sender, and then depends on the phase:

- In `paused`, the room sets `positionMs` to the target and stays paused.
- In `playing` or `waiting`, the room enters waiting at the target with timeout `waitMs` (15000).

The snapshot goes out at the moment the seek is applied.

### 7.9 `rate`

|field|type|rule|
|---|---|---|
|`rate`|number|`minRate` (0.5) ≤ rate ≤ `maxRate` (2.0)|

Refusals, in order:

1. `forbidden` when the sender is not a controller.
2. `invalid_message` when the room has no media.

If no refusal applies, the relay sets the action `rate` by the sender. In `playing`, it then works in this order:

1. If `now ≥ atMs`, it first re-anchors the timeline: `positionMs` becomes `P(now)`, and `atMs` becomes `now`.
2. It sets `rate` to the new rate.
3. It reschedules the end-of-media timer (§12.11).

In `paused` and `waiting`, only the rate changes, and it applies when playback starts.

### 7.10 `setMedia`

|field|type|rule|
|---|---|---|
|`media`|object|a media descriptor (§10), at most `maxMediaBytes` (4096) bytes as received|
|`positionMs`?|int|≥ 0: where the new media starts, 0 by default|

A sender other than the host gets `forbidden`.

Otherwise, the message works in any phase. It has these effects, in order:

1. `epoch` increases by 1, and the relay stores `media`.
2. The held seek of §12.8, if there is one, is dropped, and every exclusion of §12.7 is cleared.
3. `positionMs` becomes `min(positionMs or 0, durationMs)`.
4. The relay sets the action `media` by the sender.
5. The room enters waiting at that position with timeout `mediaWaitMs` (30000). Every connected participant counts as `loading` until it reports `status` with the new epoch (§12.3).

As §8.3 says, the `room` this publishes carries the new `media`.

### 7.11 `clearMedia`

`clearMedia` has no fields. A sender other than the host gets `forbidden`.

When the room has no media, nothing happens: there is no error and no snapshot. Otherwise, the command is *effective*, and it has these effects in order:

1. `epoch` increases by 1, and the media becomes null.
2. Every playback timer stops (§12.12), a held seek is dropped, and every exclusion is cleared.
3. `phase` becomes `idle`, `positionMs` becomes 0 and `waitingOn` becomes empty. `rate` and `atMs` are unchanged.
4. The relay sets the action `media` by the sender.

As §8.3 says, the `room` this publishes carries `media` null.

### 7.12 `setControl`

|field|type|rule|
|---|---|---|
|`mode`|string|`host` or `everyone`|

A sender other than the host gets `forbidden`. Otherwise, `control` becomes `mode`, with the meaning §11.3 gives.

### 7.13 `transferHost`

|field|type|rule|
|---|---|---|
|`peerId`|string|a participant of the room, connected or not|

Refusals, in order:

1. `forbidden` when the sender is not the host.
2. `invalid_message` when no participant has this peer ID.

Naming the current host changes nothing. Otherwise, the named participant becomes host, and the relay sets the action `host` by the sender. If the new host is disconnected, its absence starts now (§11.4).

### 7.14 `kick`

|field|type|rule|
|---|---|---|
|`peerId`|string|a participant of the room, connected or not, the host itself included|

Refusals, in order:

1. `forbidden` when the sender is not the host.
2. `invalid_message` when no participant has this peer ID.

If no refusal applies, the kick works in two steps:

1. If the target is connected, it gets `closed` with reason `kicked`. Its connection stays open, outside any room.
2. The target is removed as §11.6 describes, so its peer ID and token stop working.

The relay sends `closed` to the target and a snapshot to the remaining connected members, unless the room was deleted.

### 7.15 `lock`

|field|type|rule|
|---|---|---|
|`locked`|boolean|`true` refuses new `join`s, and `false` allows them|

A sender other than the host gets `forbidden`. Otherwise, the room's `locked` becomes the value sent, with the effect §11.7 describes.

### 7.16 `leave`

`leave` has no fields and no refusals beyond §6.2.

The relay removes the sender as §11.6 describes. The sender's connection stays open, outside any room, and the relay never sends it anything about the room again. The relay sends a snapshot to the remaining connected members, unless the room was deleted, and nothing to the sender.

### 7.17 `end`

`end` has no fields. A sender other than the host gets `forbidden`.

Otherwise, the relay sends `closed` with reason `ended` to every connected member, the sender included. The room is deleted as §11.8 describes, and every connection stays open, outside any room.

## 8. Relay → client messages

This section defines the messages the relay sends. The relay sends messages to one connection in the order it produced them. For one event of §9.2, each connection gets at most one snapshot.

|type|fields|
|---|---|
|`joined`|`code`, `you` (`peerId`, `token`), `room`, `media`, `serverTime`|
|`room`|the snapshot fields of §9, `action`?, `media`?|
|`pong`|`id`, `serverTime`|
|`error`|`code`, `message`, `re`|
|`closed`|`reason`|

### 8.1 `joined`

`joined` answers an accepted `create`, `join` or `resume`, and starts the connection's membership.

|field|type|rule|
|---|---|---|
|`code`|string|the room code, upper case|
|`you`|object|this connection's participant|
|`you.peerId`|string|§11.1|
|`you.token`|string|§11.1, kept by the client to `resume`|
|`room`|object|the snapshot of §9.1, without `type`, `action` or `media`|
|`media`|object or null|the room's media descriptor exactly as the host sent it, or null without media|
|`serverTime`|int|relay time when the message was sent|

### 8.2 `room`

A `room` message is the snapshot. It has `type` `room`, every field of §9.1, and two optional fields:

|field|type|rule|
|---|---|---|
|`action`?|object|the action set by the event that published this snapshot (§9.1), absent when that event set none|
|`media`?|object or null|present only on the snapshot that starts a new epoch, as §8.3 says|

### 8.3 Media in `room`

A `room` carries `media` exactly when it is the snapshot that `setMedia` or an effective `clearMedia` publishes, the one whose `epoch` is new. The field then holds the descriptor exactly as the host sent it, or null after `clearMedia`. Every other `room` leaves it out.

A participant that was disconnected at that moment gets the current media in `joined` when it resumes.

"Exactly as the host sent it", here and in `joined`, means the same JSON value: the same members and values, nested and unknown members included. A relay SHOULD forward the bytes it received.

### 8.4 `pong`

The relay sends this message in answer to a `ping`, as §7.4 describes.

|field|type|rule|
|---|---|---|
|`id`|string or number|the `id` of the `ping`, unchanged|
|`serverTime`|int|relay time when the message was sent|

### 8.5 `error`

The relay sends this message when it refuses a client's message.

|field|type|rule|
|---|---|---|
|`code`|string|an error code of Appendix A, which is an open enum (§4)|
|`message`|string|English text for logs and developers, which clients MUST NOT parse|
|`re`|string or null|the `type` of the refused message, or null when its envelope was invalid (§6.2 step 2) or it was binary|

### 8.6 `closed`

|field|type|rule|
|---|---|---|
|`reason`|string|one of the three reasons in the next table|

|reason|meaning|
|---|---|
|`ended`|the host ended the room|
|`kicked`|the host removed this participant|
|`expired`|the room reached its maximum age, as §11.8 says|

After `closed`, the connection is outside any room and stays open. The relay sends it nothing more about that room, and it may `create`, `join` or `resume` again.

## 9. Room snapshot

The snapshot is the state of a room that every member sees. This section defines its fields, when the relay sends it and what a new room holds.

### 9.1 Fields

The snapshot has two schemas: `snapshot.schema.json` for the fields as they appear in `joined.room`, and `room.schema.json` for the `room` message.

```json
{
  "type": "room",
  "rev": 42,
  "code": "K7MQ2X",
  "epoch": 3,
  "phase": "paused",
  "positionMs": 61234,
  "atMs": 918273645,
  "rate": 1.0,
  "control": "host",
  "locked": false,
  "waitingOn": [],
  "participants": [
    {
      "peerId": "p1", "name": "Edde", "client": "Example/1.31.0", "host": true, "connected": true,
      "player": "ready", "match": { "state": "matched", "deltaMs": -120 }, "offsetMs": 0
    }
  ],
  "action": { "kind": "pause", "by": "p1" }
}
```

Every field below is always present. A `room` message adds `action` and `media`, as §8.2 shows.

|field|type|rule|
|---|---|---|
|`rev`|int|≥ 1, as §9.2 describes|
|`code`|string|the room code, upper case|
|`epoch`|int|≥ 0, increased by 1 on every `setMedia` and every effective `clearMedia`|
|`phase`|string|`idle` (no media), `paused`, `waiting` (will play once the barrier releases) or `playing`|
|`positionMs`|int|0 ≤ positionMs ≤ the media's `durationMs`, so at most 604800000, and 0 in `idle`|
|`atMs`|int|≥ 0, in relay time. In `playing`, the moment playback runs from `positionMs`. Clients ignore it in other phases.|
|`rate`|number|`minRate` ≤ rate ≤ `maxRate`|
|`control`|string|`host` or `everyone`|
|`locked`|boolean||
|`waitingOn`|array of strings|peer IDs the barrier waits for, in join order, and empty except in `waiting`|
|`participants`|array|every participant, connected or not, in join order, and never empty|
|`participants[].peerId`|string|§11.1|
|`participants[].name`|string|§6.3|
|`participants[].client`|string|§6.3|
|`participants[].host`|boolean|true for exactly one participant|
|`participants[].connected`|boolean|whether a connection is attached|
|`participants[].player`|string|as last reported in `status`, and `idle` until the first|
|`participants[].match`|object or null|as last reported, without `deltaMs` when the status didn't carry it, and null until the first status|
|`participants[].offsetMs`|int|as last reported, so within ± 604800000, and 0 until the first status|

#### Actions

An action says who caused a change, so that apps can show it, for example in a toast. An event *sets an action* as the second table below says, and the `room` messages that event publishes carry it as `action`.

|field|type|rule|
|---|---|---|
|`action.kind`|string|one of the kinds in the second table below, from an open enum (§4)|
|`action.by`|string or null|the peer ID that caused it, or null for the relay. It may name a participant no longer in the room.|

|kind|set by|`by`|
|---|---|---|
|`play`|`play` from `paused`|the sender|
|`pause`|`pause` from `playing` or `waiting`|the sender|
|`seek`|an applied `seek`|the seek's sender|
|`rate`|`rate`|the sender|
|`media`|`setMedia`, effective `clearMedia`|the sender|
|`stall`|a stall grace that ran out under §12.9|the stalled participant|
|`ended`|end of media under §12.11|null|
|`host`|`transferHost`|the sender|
|`host`|host promotion under §11.4, or a removed host's replacement under §11.6|null|

Nothing else sets an action. An event that sets an action always publishes a snapshot, even when no snapshot field changed. An event that sets several actions carries the last one.

Each snapshot that carries an action reports a new one. `joined.room` never carries one, and the relay neither persists nor repeats an action.

### 9.2 When snapshots are sent and `rev`

An *event* is one inbound message, one connection closing, or one timer firing.

After every event that changes any snapshot field, the relay publishes one snapshot. `rev` increases by 1, and every connected member gets the snapshot, the actor included. One event that changes several fields publishes one snapshot.

An event that changes no snapshot field publishes none, with two exceptions. `resume` always publishes, as §7.3 says, and so does an event that sets an action under §9.1.

`create`, `join` and `resume` deliver the snapshot to the sender inside `joined`. The other connected members get `room`.

`rev` starts at 1 at creation and increases by exactly 1 per published snapshot. On one connection, each snapshot therefore has a higher `rev` than the one before it. A client drops a `room` whose `rev` is not higher than that of the last snapshot it accepted on that connection.

`joined` resets the comparison. The client accepts the snapshot it carries, whatever its `rev`, and compares later `room` messages with it. This covers a relay restart under §14 that makes `rev` go back.

### 9.3 A new room

A new room starts with these values:

|field|value|
|---|---|
|`rev`|1|
|`epoch`|0|
|`phase`|`idle`|
|`positionMs`|0|
|`atMs`|the relay time of creation|
|`rate`|1|
|`control`|`host`|
|`locked`|false|
|`waitingOn`|empty|

It has one participant, the creator, with `host` true, `connected` true, `player` `idle`, `match` null and `offsetMs` 0.

## 10. Media descriptor

The room plays a title described by a *media descriptor*, or descriptor for short. Each client resolves the descriptor to its own file, as §16.5 says. The schema is `descriptor.schema.json`.

```json
{
  "kind": "episode",
  "title": "Pilot",
  "ids": { "imdb": "tt0959621", "tvdb": "349232" },
  "series": { "title": "Breaking Bad", "year": 2008, "ids": { "imdb": "tt0903747", "tmdb": "1396", "tvdb": "81189" } },
  "season": 1,
  "episode": 1,
  "durationMs": 3487000,
  "file": { "sizeBytes": 2147483648, "oshash": "8e245d9679d31e12", "name": "Breaking.Bad.S01E01.1080p.mkv" },
  "ext": { "com.example": { "server": "1a2b3c", "item": "4711" } }
}
```

|field|type|rule|
|---|---|---|
|`kind`|string|`movie`, `episode` or `video`, where a video is a file with no catalogue identity, such as a home video, a recording or a clip|
|`title`|string|the movie's, episode's or video's title, 1 or more characters with no forbidden characters (§2)|
|`year`?|int|release year|
|`ids`?|object|a map from namespace to ID, described below, holding the episode's own IDs for an episode|
|`series`?|object|REQUIRED when `kind` is `episode`|
|`series.title`|string|1 or more characters with no forbidden characters, as defined in §2|
|`series.year`?|int|first-air year|
|`series.ids`?|object|a map from namespace to the series' IDs|
|`season`?|int|≥ 0, as the setter's source numbers it, with 0 for specials|
|`episode`?|int|≥ 0, as the setter's source numbers it|
|`durationMs`|int|1 ≤ durationMs ≤ 604800000 (7 days, §13.1): the reference file's duration, from its player or from reliable metadata|
|`file`?|object|hints that pick the same release among candidates|
|`file.sizeBytes`?|int|≥ 0|
|`file.oshash`?|string|the OpenSubtitles hash: 16 lower-case hex digits|
|`file.name`?|string|the file name without directories, 1 or more characters, following the file-name rules below|
|`ext`?|object|app-private hints, at most `maxExtBytes` (1024) bytes as received|

#### IDs

Each `ids` member maps a namespace to a non-empty string, even where the catalogue uses numbers, as in `"tmdb": "1396"`. The v1 namespaces are `imdb`, `tmdb`, `tvdb`, `plex`, `anilist`, `anidb`, `kitsu` and `mal`.

- A `plex` ID is a `plex://` GUID.
- A movie's `tmdb` is a movie ID, and a series' is a TV ID.

Unknown namespaces are ignored. Adding a namespace is a documentation change.

#### App-private hints

Each `ext` member name is the owning app's reverse-DNS name, such as `com.example`. It has two or more dot-separated labels of `a`–`z`, `0`–`9` and `-`. The values are JSON defined by the owner, and apps ignore members they do not own.

#### File name

`file.name` MUST NOT contain `/`, `\` or a forbidden character, such as NUL. It MUST NOT be `.` or `..`. Other names that start with a dot, such as `.Arrival.mkv` or `...`, are allowed. A name is a hint for matching, never a path.

#### Size

The whole descriptor is at most `maxMediaBytes` (4096) bytes. Descriptor and `ext` sizes are measured as the JSON text of the value as received.

#### Nothing that fetches media

A sender MUST NOT put any of these in any field, `ext` included: URLs, magnet links, info hashes, stream links, or anything else that locates or fetches media. A receiver never treats descriptor content as a path, URL or host. `ids` values are identifiers, not locators.

#### What the relay checks

The relay checks the schema, the table above, and the rules for `ids`, `ext` names, file names and size. It refuses a violation with `invalid_message` under §6.2. It does not check the rule against fetching media.

The relay reads only `durationMs`. The rest of the descriptor is opaque to it and forwarded unchanged.

## 11. Rooms and roles

This section defines participants and their states, the host and controller roles, and how participants leave and rooms end.

### 11.1 Participants

A participant has a peer ID, a token, a `name`, a `client`, a place in the join order, and the status fields of §7.5.

- A *peer ID* is 1–16 characters of `A`–`Z`, `a`–`z`, `0`–`9`, `_` and `-`. It is unique in the room and never reused within one room's life.
- A *token* is 32 bytes from a CSPRNG, written in base64url without padding as 43 characters.
- The *join order* is the order in which participants entered the room. `resume` keeps a participant's place.

A room holds at most `maxParticipants` (8) participants, connected or not.

### 11.2 Membership states

A participant is in one of three states:

- A *connected* participant has a connection attached.
- A *disconnected* participant has none, and is reserved for `resume`.
- A *removed* participant has left the snapshot, and its peer ID and token stop working.

### 11.3 Host and controllers

A room always has exactly one host, and the creator is the first. The host-only commands are `setMedia`, `clearMedia`, `setControl`, `transferHost`, `kick`, `lock` and `end`.

Controllers may send `play`, `pause`, `seek` and `rate`. The room's `control` decides who is a controller:

- With `control` `host`, the host is the only controller.
- With `control` `everyone`, every participant is a controller.

These four commands from a non-controller, and host-only commands from anyone but the host, get `forbidden`.

### 11.4 Host absence and promotion

The host is *absent* while it is disconnected. Absence starts at one of these moments:

- When the host disconnects.
- When a disconnected participant becomes host, through `transferHost` or the removal of the previous host.
- When the relay restores the room after a restart under §14.

Absence ends when the host connects again.

When the host has been absent for `hostPromotionMs` (30000) and at least one participant is connected, the connected participant earliest in join order becomes host. The relay sets the action `host` by null. With nobody connected, the host stays.

The relay checks for promotion when the absence reaches `hostPromotionMs`. It checks again after every `join` and `resume` while the absence is that long or longer.

### 11.5 Disconnect

When a member's connection closes for any reason other than a `resume` takeover, the relay works in this order:

1. The participant stays, with `connected` false. Its `player`, `match` and `offsetMs` are kept.
2. Its stall grace of §12.9 stops, and its reservation timer starts. After `resumeMs` (300000) disconnected, it is removed as §11.6 describes, unless it resumed.
3. If it is the host, its absence under §11.4 starts.
4. The peer-event rules of §12.5 run.

The relay sends a snapshot to the connected members.

### 11.6 Removal

Three things remove a participant: `leave`, `kick` and an expired reservation. Removal works in order:

1. The participant leaves `participants` and `waitingOn`. Its peer ID and token stop working, and its stall grace and reservation timer stop.
2. If no participant remains, the room is deleted as §11.8 describes, and the later steps do not run.
3. If the participant was the host, the host passes at once to the connected participant earliest in join order. With nobody connected, it passes to the participant earliest in join order. The relay sets the action `host` by null, and a disconnected new host is absent from now (§11.4).
4. The peer-event rules of §12.5 run.

The relay sends one snapshot to the remaining connected members.

### 11.7 Lock

A locked room refuses `join` with `room_locked`, but `resume` still works. A kicked participant cannot come back into a locked room. Into an unlocked room, it can `join` again with the code.

### 11.8 End, expiry and deletion

A room is deleted in three cases:

- When its last participant is removed by `leave`, `kick` or an expired reservation under §11.6.
- By `end`, as §7.17 describes.
- At `maxRoomAgeMs` (86400000, 24 hours) after creation. Every connected member first gets `closed` with reason `expired`.

Deletion has these effects:

- Every timer stops, and the relay forgets the media, the participants and the tokens.
- The room leaves the persisted state of §14.
- The code names no room afterwards, so it answers `room_not_found` and `resume_failed`. `create` may draw it again, and a `join` with `create` makes a new room with it (§7.2).
- Connections that were members stay open, outside any room.

### 11.9 Refused clients (`client_blocked`)

A relay MAY keep a deny list of app names, matched against the `<app>` of §6.3. The operator's terms of use decide its entries. The relay compares the app name with each entry case-insensitively for ASCII, and exactly otherwise.

These messages get `client_blocked`:

- A `create` or `join` whose `client` names a denied app.
- A `resume` of a participant whose stored `client` names a denied app.

A relay MUST apply a changed deny list without a restart. A change does not affect connections already in rooms.

### 11.10 Rooms per IP and room cap

The relay remembers, in memory only, the client IP that created each room, and counts rooms per address group of that IP as §3.1 defines. While a group is the creator of `maxRoomsPerIp` (3) × its group factor rooms that the relay holds, a `create` from it gets `rate_limited`, and so does a `join` that would make a room (§7.2). Rooms restored under §14 count for no IP.

While the relay holds `maxRooms` (10000) rooms, restored ones included, `create` gets `relay_full`, and the client creates on another relay as §5.4 says. A `join` that would make a room gets `relay_full` too. Restoring rooms after a restart is never refused.

## 12. Playback state machine

This section defines the room's playback phases and position, the readiness barrier, and the timers that drive them.

### 12.1 Phases

A room is in one of four phases:

|phase|meaning|
|---|---|
|`idle`|There is no media. `play`, `pause`, `seek` and `rate` from a controller get `invalid_message`.|
|`paused`|The position holds at `positionMs`.|
|`waiting`|The position holds at `positionMs` while the barrier waits for `waitingOn`.|
|`playing`|The position advances from `positionMs` at `atMs` with `rate`.|

*Pausing* at `p` stops every playback timer (§12.12) and sets `phase` to `paused`, `positionMs` to `p` and `waitingOn` to empty. It leaves `atMs` and `rate` unchanged.

### 12.2 Position

`P(t)` is the room's position at relay time `t`:

- In `playing`, it is `min(durationMs, floor(positionMs + max(0, t − atMs) × rate))`, computed in IEEE 754 double precision with the floor of the whole sum.
- In `paused`, `waiting` and `idle`, it is `positionMs`.

`durationMs` is the current media's. Every `positionMs` the relay stores is between 0 and `durationMs`.

A `playing` room with `atMs` in the future is a scheduled group start. Clients hold at `positionMs` and start at `atMs`.

### 12.3 Counted and ready

The barrier works with three derived terms: the effective player, counted and ready.

A participant's *effective player* is its reported `player` when its latest status has the room's `epoch`. It is `loading` when its latest status is for another epoch, or when it has sent none.

A participant is *counted* when all of these hold:

- It is connected.
- It is not excluded under §12.7.
- Its effective player is not `idle`.
- It has not reported unavailable for the current epoch, as §7.5 defines.

A participant is *ready* when its status epoch is the room's `epoch` and its `player` is `ready`. So a counted participant whose latest status is for another epoch is not ready.

After `setMedia`, everyone connected counts as `loading` until it reports on the new media. A status in flight for the old media changes nothing.

### 12.4 Entering waiting

To enter waiting at `p` with timeout `T`, the relay works in this order:

1. Every playback timer stops.
2. `phase` becomes `waiting`, and `positionMs` becomes `p`.
3. The wait is a *stall wait* if a stall grace of §12.9 caused it, and not otherwise.
4. `waitingOn` becomes the counted participants that are not ready, in join order.
5. The barrier timer of §12.7 starts with `T`.
6. If `waitingOn` is empty, the room releases at once (§12.6).

### 12.5 Peer-event rules

These rules run after every `status`, disconnect (§11.5) and removal (§11.6). They run in this order:

1. Nobody in the player. If `phase` is `playing` or `waiting` and no connected participant has an effective player other than `idle`, the room pauses at `P(now)` without setting an action, and the rules stop.
2. Barrier. If `phase` is `waiting`, `waitingOn` is recomputed as the counted participants that are not ready, in join order. Then:
   - If `waitingOn` is non-empty, the stall release timer stops.
   - If it is empty and the wait is not a stall wait, the room releases at once.
   - If it is empty in a stall wait and the stall release timer is not running, that timer starts with `stallReleaseMs` (400). When it fires, the room releases if it is still waiting with `waitingOn` empty.

These rules run only after these events. They do not run after commands, `join`, `resume` or an exclusion ending, and the effects of those show at the next peer event.

### 12.6 Release

To release, the relay works in this order:

1. The barrier and stall release timers stop.
2. `phase` becomes `playing`, and `waitingOn` becomes empty.
3. `atMs` becomes `now + clamp(floor(startDelayRttFactor × maxRtt), startDelayMinMs, startDelayMaxMs)`. With the public values, that is `now + clamp(floor(1.5 × maxRtt), 750, 2000)`.
4. No action is set.
5. The end-of-media timer is scheduled (§12.11).

`maxRtt` is the highest `rttMs` reported by a counted participant. A counted participant that never reported one counts with `defaultRttMs` (500). When no participant is counted, `maxRtt` is `defaultRttMs`.

### 12.7 Barrier timeout and exclusion

The barrier timer starts each time the room enters waiting, with `waitMs` (15000), or with `mediaWaitMs` (30000) after `setMedia`. Leaving `waiting` stops it.

When the barrier timer fires, every participant then in `waitingOn` is *excluded* until `now + exclusionMs` (60000), and the room releases. Excluded participants catch up by themselves, and they do not hold the room until the exclusion ends.

An exclusion survives disconnect and `resume`. `setMedia` and an effective `clearMedia` clear every exclusion. An exclusion ends silently when its time passes, and the end takes effect at the next computation that looks at it.

### 12.8 Seek coalescing

The relay applies at most one seek per `seekCoalesceMs` (200), and records when it last applied a seek in the room. A `seek` is handled in one of two ways:

- When no seek was applied in the last `seekCoalesceMs`, the `seek` is applied at once, as §7.8 describes.
- Otherwise, it becomes the room's *held seek*, replacing any held seek. It is applied at `last applied + seekCoalesceMs`, with the target and sender it carries, under the rules of the phase at that moment.

`setMedia`, an effective `clearMedia` and deletion drop the held seek. Permission is checked when the `seek` arrives, and a held seek is applied even if its sender has lost control since.

### 12.9 Stall

A participant is *stalled* when it is counted and its latest status has the room's `epoch` and `player` `buffering`.

In `playing`, a status that leaves its sender stalled starts the sender's stall grace of `stallGraceMs` (500), unless one is running. Further `buffering` statuses do not restart it. These stop the grace:

- A `ready` status from the participant.
- The participant's disconnect or removal.
- The room leaving `playing`.

When the grace runs out while the room is `playing` and the participant is still stalled, the relay sets the action `stall` by that participant. The room enters waiting at `P(now)` with timeout `waitMs` (15000), as a stall wait. Otherwise, nothing happens.

A stall wait releases `stallReleaseMs` (400) after `waitingOn` empties, as §12.5 says.

### 12.10 Nobody in the player

Section 12.5 step 1 looks at every connected participant with an effective player other than `idle`. Excluded participants and those that reported unavailable are included. So an excluded participant that is still watching keeps the room going.

### 12.11 End of media

In `playing`, when `P(t)` reaches `durationMs`, the room pauses there, and the relay sets the action `ended` by null at that moment.

The end-of-media timer fires at `atMs + ceil((durationMs − positionMs) / rate)`. It is scheduled on every release and on every `rate` while `playing`. It stops whenever the room leaves that phase.

If the timer is scheduled while `P(now)` is already `durationMs`, the room ends at once, in the same event. That event publishes one snapshot: `paused` at `durationMs`, with the action `ended`.

The relay never advances to other media. The host's app decides what plays next, and sets it with `setMedia`.

A `play` at the end enters waiting, and the event that releases it pauses the room at the end again. So to play again, the host seeks first.

### 12.12 Timers

|timer|per|starts|stops|on firing|section|
|---|---|---|---|---|---|
|barrier|room|entering waiting|leaving `waiting`|exclude `waitingOn`, release|§12.7|
|stall release|room|`waitingOn` empties in a stall wait|`waitingOn` non-empty, or leaving `waiting`|release if still waiting with `waitingOn` empty|§12.5|
|end of media|room|release, or `rate` in `playing`|leaving `playing`|pause at `durationMs`, `ended`|§12.11|
|held seek|room|a coalesced `seek`|`setMedia`, `clearMedia` or deletion|apply the seek|§12.8|
|stall grace|participant|§12.9|§12.9|enter a stall wait|§12.9|
|host absence|room|§11.4|the host connects, or another participant becomes host|promote|§11.4|
|reservation|participant|disconnect|`resume` or removal|remove|§11.6|
|maximum age|room|creation|deletion|`closed` `expired`, delete|§11.8|

"Every playback timer stops" means that the barrier, stall release and end-of-media timers stop, and every stall grace stops. A held seek survives a phase change.

A timer that fires runs as one event of §9.2. A timer stopped or restarted before its event runs does nothing.

## 13. Limits and policy

This section lists the limits and durations of v1, with the values of the public relays. They come in two kinds.

*Protocol values* are the same on every v1 relay. The schemas encode them, and changing one is a v2 change.

*Policy values* are each relay's choice, and `/v1/info` reports them. Only `resumeMs` is fixed, at 300000 on every v1 relay, and it is scaled only in a relay's test mode, described in §13.3.

Clients act on no policy value but `resumeMs`, which §16.1 relies on. They assume 300000 and read nothing from `/v1/info`, so changing any other policy value needs no client release.

The public values below are the public relays' values and the relay's built-in defaults.

### 13.1 Protocol limits (`limits`, fixed in v1)

|key|value|meaning|section|
|---|---|---|---|
|`maxMessageBytes`|8192|largest inbound message|§3.1|
|`maxMediaBytes`|4096|largest media descriptor|§10|
|`maxExtBytes`|1024|largest `ext`|§10|
|`maxNameLength`|32|longest `name`|§6.3|
|`maxClientLength`|64|longest `client`|§6.3|
|`minRate`|0.5|lowest `rate`||
|`maxRate`|2.0|highest `rate`||

Two more bounds are fixed in v1 and encoded in the schemas, but not reported in `/v1/info`. `durationMs` is at most 604800000 under §10, and `offsetMs` is between −604800000 and 604800000 under §7.5.

### 13.2 Policy limits (`limits`)

|key|public value|meaning|section|
|---|---|---|---|
|`maxParticipants`|8|participants per room, disconnected ones included, at most 20 as explained below|§11.1|
|`maxConnections`|10000|open WebSocket connections on the relay|§3.1|
|`maxConnectionsPerIp`|10|open WebSocket connections per address group, times its factor|§3.1|
|`maxRooms`|10000|rooms the relay holds, beyond which `create` and a `join` that would make a room get `relay_full`|§11.10|
|`maxRoomsPerIp`|3|rooms an address group created that the relay still holds, times its factor|§11.10|
|`ipv6Factor56`|4|factor of an IPv6 /56 group|§3.1|
|`ipv6Factor48`|16|factor of an IPv6 /48 group|§3.1|
|`messageBurst`|30|per-connection message bucket size|§3.1|
|`messagesPerSecond`|10|per-connection refill per second|§3.1|
|`joinBurst`|20|per-group `join` bucket size, times its factor|§7.2|
|`joinsPerSecond`|1|per-group join refill per second, times its factor|§7.2|
|`joinBurstTotal`|2000|relay-wide join bucket size|§7.2|
|`joinsPerSecondTotal`|200|relay-wide join refill per second|§7.2|

`maxParticipants` is at most 20, so that every relay message stays within the 16384 bytes that clients accept under §3.1.

### 13.3 Policy durations (`policy`)

|key|public value|scaled|meaning|section|
|---|---|---|---|---|
|`stallGraceMs`|500|yes|buffering this long while playing starts a stall wait|§12.9|
|`stallReleaseMs`|400|yes|a stall wait releases this long after `waitingOn` empties|§12.5|
|`waitMs`|15000|yes|barrier timeout after `play`, `seek` and a stall|§12.7|
|`mediaWaitMs`|30000|yes|barrier timeout after `setMedia`|§12.7|
|`exclusionMs`|60000|yes|how long a participant that timed out doesn't hold the room|§12.7|
|`startDelayMinMs`|750|yes|shortest start delay|§12.6|
|`startDelayMaxMs`|2000|yes|longest start delay|§12.6|
|`startDelayRttFactor`|1.5|no|start delay per ms of the highest round trip|§12.6|
|`defaultRttMs`|500|no|round trip assumed for a participant that reported none|§12.6|
|`seekCoalesceMs`|200|yes|minimum time between applied seeks|§12.8|
|`pauseToleranceMs`|2000|no|largest distance at which `pause` keeps the sender's position|§7.7|
|`hostPromotionMs`|30000|yes|host absence before promotion|§11.4|
|`resumeMs`|300000|yes|how long a disconnected participant stays reserved, 300000 on every v1 relay|§11.5, §13|
|`maxRoomAgeMs`|86400000|yes|longest life of a room|§11.8|

`startDelayRttFactor` is a number, and every other policy value is an int.

#### Test mode

A relay MAY offer a test mode that multiplies every scaled duration by one factor, rounded down to whole milliseconds, so that a conformance suite runs fast. `/v1/info` reports the scaled values. The public relays run unscaled.

Some values are never scaled: media positions, `durationMs`, reported `rttMs`, the keepalive of §3.1 and the rate-limit buckets.

## 14. Persistence and restart

A relay MAY persist rooms across restarts. This section says what such a relay writes, what it never writes, and how it restores rooms on start.

### Writing the state

A relay that persists rooms writes a changed room within 5 s, every `playing` room at least every 5 s, and all rooms at graceful shutdown. It writes atomically, renaming a new file over the old one. The state is one file, or one file per room.

The state holds these fields:

|for each|fields|
|---|---|
|room|the code, `rev`, `epoch`, media, position (`P` at writing), `rate`, `control`, `locked`, wall-clock creation time and peer-ID sequence|
|participant|the peer ID, the SHA-256 hash of the token, `name`, `client`, join order, host flag, `match` and `offsetMs`|

The relay MUST NOT persist tokens, client IPs, `rttMs`, actions, exclusions or timers. Deleted rooms leave the state within 5 s. The state is readable by the relay's user only.

### Restoring

On start, the relay restores every room whose maximum age has not passed:

- The room is `paused` at the persisted position, except that an idle room stays `idle`.
- `waitingOn` is empty, and `atMs` is the restore time.
- `rev` continues from the persisted value.
- Participants are disconnected, with `player` `idle`, and their tokens work.

After a crash, the state can be up to 5 s old. `rev` can then go back, which §9.2 covers, and a room that was playing restores a position at most 5 s old.

Reservation timers and host absence start at restore. A restored room therefore lives until its participants' reservations expire, unless someone resumes. Maximum age still counts from the original creation.

Held seeks, stall graces, exclusions and `rttMs` are gone. Until participants report again, `defaultRttMs` counts for them in §12.6, and their effective player of §12.3 is `loading`.

Without persistence, rooms are lost, and `resume` gets `resume_failed`.

## 15. Privacy

This section says what a relay keeps about the people who use it. The relay reads titles and positions, because it needs them to run a room, but it never reads media (§10).

These rules bind the operator of a public relay. Self-hosting operators SHOULD follow them.

### Logs and memory

The relay logs none of the following: media descriptors or any part of them, participant names, room codes, peer IDs, tokens, positions or message contents. `client` appears only in the counts below. Logs may hold error codes and counts.

IPs stay in memory only, for the limits of §3.1, §7.2 and §11.10 and for the usage allowance below. They MUST NOT reach logs or disk, on the relay or on a proxy.

Room data lives in memory and in the state of §14, and it is deleted with the room under §11.8.

### Metrics

Metrics are counts only. They include rooms, participants, connections, rooms created, accepted joins and resumes, inbound messages, refused upgrades by reason, error replies by code, and participants per `<app>` name.

Per-app series exist only for the 50 names with the highest counts in that UTC day's usage record, `(other)` excluded. Ties go to the name that sorts first. Every other participant counts under `(other)`.

### Usage record

Public relays keep counts per UTC day and per `<app>` of accepted `create`, `join` and `resume`. The record holds no IPs, participant names, titles, room codes or tokens. A day is deleted 400 days later.

Each IPv4 address or IPv6 /64 adds at most 3 names per day:

- A new app name from an address that has added 3 counts under `(other)`.
- A name already recorded counts under itself, from any address.

Which addresses added names is kept in memory only, never on disk, and cleared at the end of the day.

Expired days and the per-address memory are dropped within a minute of their deadline. They leave `usage.json` at the next save, so within two minutes, whatever the traffic.

A relay MAY cap the names per day. Names beyond the cap count under `(other)`.

## 16. Client behaviour

This section states what a client MUST do on the wire. `SDK.md` §5, §6 and §11 define the algorithms and values behind it, for clock sync, relay choice and following the room.

### 16.1 Lifecycle

1. Connect. To create, connect to a relay chosen as §5.4 says. To join, connect to the relay that owns the code's first symbol. To resume, connect back to the relay the party joined on. Both follow §5.4. Start clock sync at once (§16.2).
2. Send `create` or `join`, and receive `joined`. It carries the code, the peer ID and token, the snapshot and the media.
3. While in the room, send `status` (§16.3), receive `room`, and follow the room (§16.4).
4. After a dropped connection, reconnect to the same relay and `resume` with the code, peer ID and token within `resumeMs`, 300000 under §13. The rules for resuming follow this list.
5. Send `leave`, or receive `closed`.

A client MUST NOT resume after a close with status 4000, which §7.3 describes, after `closed`, or after `leave`.

The answer to a `resume` decides what happens next:

- After `resume_failed` or `client_blocked`, the membership is over.
- Any other `error` answering it, such as `rate_limited` or an unknown code, refuses only that attempt. The client MAY retry within `resumeMs`.

### 16.2 Clock sync

A client estimates relay time from `ping` and `pong` on every connection, as `SDK.md` §5 defines.

The `rttMs` in `status` is the client's best round trip. That is the lowest `pong` round trip among the samples its clock uses, where a round trip is the receipt time minus the send time.

### 16.3 Status

A member MUST send `status` at these times:

- Right after `joined`.
- After every change of `epoch`, `player`, `match` or `offsetMs`.
- At least every 15 s.

The `epoch` it sends is the room epoch of the last `joined` or accepted `room` that the client processed. §9.2 says which `room` messages a client accepts. `ready` means the player plays that epoch's media.

### 16.4 Following the room

The target at relay time `t` is `P(t) + offsetMs` (§12.2), as a position in the client's own file. In other words, the local position is the room position plus the user's offset for a differing file. A `matched` file plays at offset 0 unless the user sets one. The relay never applies offsets.

A client reports `ready` only when its player can play at once and is close enough to the target that following it needs no seek. It reports `loading` while getting there, as §7.5 defines.

`SDK.md` §11 defines how SDKs follow the target, with their thresholds.

### 16.5 Matching

A client reports `match` as §7.5 defines. `SDK.md` §9 defines how SDKs find, rank and choose files, and `SDK.md` §9.3 defines when two descriptors name the same title.

## Appendix A. Error codes

These are the codes that the `error` message of §8.5 carries.

|code|answers|meaning|section|
|---|---|---|---|
|`invalid_message`|any message|malformed JSON or envelope (`re` null), a shape violation, a binary message (`re` null), `play`/`pause`/`seek`/`rate` without media, `transferHost`/`kick` naming no participant|§6.2|
|`unsupported`|a message of unknown `type`|this relay doesn't implement the type|§4|
|`rate_limited`|any message|the connection's message bucket is empty|§3.1|
|`rate_limited`|`create`, `join`|the address group created its limit of rooms, for a `join` only when it would make a room|§11.10, §7.2|
|`rate_limited`|`join`|the join allowance of the address group or of the relay is used up|§7.2|
|`relay_full`|`create`, `join`|the relay holds `maxRooms` rooms. After `create`, the client creates on another relay.|§11.10, §5.4, §7.2|
|`room_not_found`|`join`|no room with this code, and the relay made none: the `join` lacks `create`, or the relay doesn't make codes like this one|§7.2|
|`room_full`|`join`|`maxParticipants` reached|§7.2|
|`room_locked`|`join`|the room is locked|§7.2|
|`forbidden`|controller and host-only commands|the sender lacks the role|§11.3|
|`not_in_room`|room commands|the connection is outside a room|§6.2|
|`already_in_room`|`create`, `join`, `resume`|the connection is a member|§6.2|
|`resume_failed`|`resume`|unknown room or participant, or wrong token|§7.3|
|`client_blocked`|`create`, `join`, `resume`|this relay refuses the app|§11.9|

## Appendix B. WebSocket close codes and HTTP refusals

The relay closes a connection with these WebSocket close codes, and refuses an upgrade with these HTTP statuses.

|code|sent by|meaning|client action|
|---|---|---|---|
|1001|relay|the relay is shutting down|reconnect and `resume`|
|1007|relay|an inbound text message wasn't valid UTF-8|fix the sender, then reconnect and resume|
|1009|relay|an inbound message exceeded `maxMessageBytes`|fix the sender, then reconnect and resume|
|4000|relay|replaced: a `resume` on another connection took over this participant|MUST NOT resume|
|HTTP 400|relay, at upgrade|behind a trusted proxy, the first `X-Forwarded-For` value isn't an IP address or carries a zone|fix the proxy|
|HTTP 429|relay, at upgrade|too many connections from this IP or its network under §3.1|retry later|
|HTTP 503|relay, at upgrade|the relay is at `maxConnections`|retry later or use another relay to create|

A connection can also end without a defined code: when the keepalive of §3.1 drops it, when the network drops it, or when the client does not read what the relay sends. The client then reconnects and resumes.

The WebSocket layer may also close with 1002 on framing errors, and answer HTTP 400 to a malformed handshake.

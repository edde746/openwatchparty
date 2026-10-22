# Scripted peer

`owp-peer` is a scripted participant built on the TypeScript SDK. It creates or joins a room and runs a list of steps, so that an app under test has someone else in the room.

It imports the SDK's TypeScript sources directly, so it needs this repository and Node 22.18+ (23.6+ on Node 23, any 24+). There is nothing to install or build. Run it from the repository root.

## Quick start

These two commands put a host and a guest in one room on a [local relay](../../relay/README.md).

1. Start the host. It prints `created <CODE> p1` and stays in the room until Ctrl-C:

   ```sh
   node conformance/peer/owp-peer.js --relay http://127.0.0.1:8080 --name Ann create \
     media '{"kind":"movie","title":"Heat","year":1995,"durationMs":600000}' stay
   ```

2. Start a guest with the code the host printed. This guest's file is 10 seconds shorter. The guest accepts the mismatch and checks that it stays in sync:

   ```sh
   node conformance/peer/owp-peer.js --relay http://127.0.0.1:8080 --name Bob --file 590000 --timeout 45 join K7MQ2X \
     accept wait me.match=mismatched phase=playing sleep 1500 wait me.drift=0~350 stay
   ```

`--timeout 45` leaves time for a guest that starts 0.35 to 2 seconds behind. Such a guest plays 4% faster and catches up in up to about 40 seconds.

This is an excerpt of the guest's output. The last line, `ended left`, comes after Ctrl-C:

```
joined K7MQ2X p2
media Heat kind=movie duration=600000
room rev=5 epoch=1 phase=playing position=0 rate=1 control=host locked=false host=Ann participants=2 waitingOn=0
step accept
mismatch delta=-10000
match mismatched delta=-10000
player ready
step wait me.match=mismatched phase=playing
step wait me.drift=0~350
step stay
ended left
```

## Usage and options

```
usage: owp-peer [options] create [steps…]
       owp-peer [options] join CODE [steps…]
```

`--help` prints the full usage, with the options, steps, conditions and exits, and then exits 0.

| Option | Default | What it does |
|---|---|---|
| `--relay URL\|public` | `public` | The relay base URL, or the SDK's public list. A `ws(s)://…/v1/ws` URL becomes its `http(s)://` base. |
| `--name NAME` | `Peer` | The display name, 1-32 characters. |
| `--client APP/VER` | `owp-peer/0.1.0` | The client name. |
| `--file MS\|none` | the media's duration | The file this peer finds for shared media: its duration, or none. |
| `--timeout S` | `30` | How many seconds `wait`, `accept`, `decline` and `end` may take. |
| `--json` | off | Prints events as JSON lines. |
| `--create` | off | With `join`: when no room has the code, makes the room with this peer as its host. |

`join` accepts any code form the SDK normalizes, such as lower case or `ct6-3qx`. With `--create`, it prints `joined` whether it joined the room or made it. A `host=` condition with the peer's name tells the two apart.

### How a script runs

The steps run in order, and after the last one the peer leaves. A refused command prints `error`, and the script goes on.

Command steps other than `end` don't wait for the relay. They send their message, and the script goes on at once. A step that depends on an earlier step's effect therefore needs an explicit `wait` on that effect first. For example:

- `media … wait phase=playing pause wait phase=paused` waits for playback before it pauses.
- `pause wait phase=paused seek 120000 wait position=120000` waits for the new position before an `expect` on it.

Without the `wait`, a `pause` right after `media` is refused with `error invalid_message`, because the party hasn't seen the media yet. An `expect` right after `seek` still sees the old position.

## Steps

Command steps act on the media, the player, playback or the room. The other steps wait or check.

### Media and the player

| Step | What it does |
|---|---|
| `media JSON\|@FILE [MS]` | As host, shares a descriptor ([SPEC.md](../../spec/SPEC.md) §10) from position MS, by default 0. |
| `file MS\|none` | Changes the file this peer finds. |
| `accept` / `decline` | Waits for the mismatch prompt, then answers it. |
| `attach` / `detach` | Attaches or detaches the simulated player, which is attached by default. |
| `stall MS` | Makes the player buffer for MS milliseconds while the script goes on. |

The descriptor for `media` is inline JSON, or `@FILE` for a file of your own. A bad descriptor is a usage error. The host then plays a simulated file of the descriptor's duration.

What `file` does depends on whether this peer has chosen one of its files:

- With none chosen, the peer searches again.
- With one chosen, `file MS` plays the new file at once, with no prompt.
- With one chosen, `file none` keeps the chosen file playing.

Before there is a file, `stall` prints `error no_player re=stall`.

### Party commands

| Step | What it does |
|---|---|
| `play`, `pause` | Plays or pauses. |
| `seek MS` | Seeks to MS in this peer's file. |
| `rate R` | Sets the rate, from 0.5 to 2.0. |
| `offset MS` | Sets this peer's offset in whole ms. The local position is the room's position plus the offset. |

### Room commands

| Step | What it does |
|---|---|
| `control host\|everyone` | Sets who controls playback. |
| `lock on\|off` | Locks or unlocks the room. |
| `transfer WHO` | Hands host to WHO. |
| `kick WHO` | Removes WHO from the room. |
| `clear` | Clears the media. |
| `end` | Ends the room, and waits for the relay to end it. |
| `leave` | Leaves the room. |

WHO is a name or a peer ID.

### Waiting and checking

| Step | What it does |
|---|---|
| `sleep MS` | Waits MS milliseconds. |
| `wait COND…` | Waits until all conditions hold at once, or fails after `--timeout`. |
| `expect COND…` | Checks that all conditions hold now, or fails. |
| `stay` | Stays until the room ends or the peer gets SIGINT or SIGTERM. |

## Conditions

A condition is `KEY=VALUE`. A number takes an optional `~tolerance`, so `position=61000~500` holds from 60500 to 61500.

Text that is missing reads as `none`. A number that is missing never holds.

### Room keys

| Key | Values | What it reads |
|---|---|---|
| `phase` | `idle paused waiting playing` | The room's phase. |
| `participants` | count | The participants, disconnected ones included. |
| `waitingOn` | count | How many participants the room waits on. |
| `position`, `rate` | number | The room's position and rate. |
| `control` | `host everyone` | Who controls playback. |
| `locked` | true or false | Whether the room is locked. |
| `host` | name, peer ID or `none` | The host. |
| `connection` | `idle connecting connected reconnecting ended` | This peer's connection. |
| `ended` | `left ended kicked expired replaced lost` | Why the party ended. |

### Participant keys

A participant key is `WHO.key`, where WHO is a name, a peer ID or `me`.

| Key | Values | What it reads |
|---|---|---|
| `player` | `idle loading ready buffering` | The participant's player. |
| `match` | `searching matched mismatched unavailable` | The participant's match. |
| `delta` | number | The file's duration minus the media's duration. |
| `offset` | number | The participant's offset. |
| `connected` | true or false | Whether the participant is connected. |

Only `me` has two more keys. `me.position` is this peer's player position. `me.drift` is how far the player is from the room, counting the offset: player − (room + offset).

## Player, output, exits

### The resolver and the player

The resolver returns one candidate with the duration from `--file` or the `file` step, or none. A candidate more than 1.5 seconds off the media's duration is a mismatch.

The simulated player runs on the local clock at its rate and stops at the end of the file. It carries out commands at once.

### Output

`owp-peer` prints one line per event on stdout, starting with `created` or `joined`. The other lines are:

- `room`, `status`, `participant joined|left` and `action <kind> by=…` for the room and its participants.
- `media <title>|none`, `match`, `mismatch` and `player` for the media and this peer's file and player.
- `connection`, only when the connection goes into or out of `reconnecting`.
- `error <code> re=… message=…` for an error, such as a refused command.
- `step` for each step, and `fail <step> reason=…` for a failed one, with the unmet conditions and their values.
- `ended <reason>` when the party ends.

`--json` prints the same events as JSON objects, such as `{"type":"created","code":"MQT5MH","peerId":"p1"}` and `{"type":"ended","reason":"left"}`.

### Exit status

| Exit | When |
|---|---|
| 0 | All steps ran. In `stay`, also when the party ended (other than `lost`) or a signal came. |
| 1 | A `wait`/`expect`/`accept`/`decline`/`end` failed, or a signal came outside `stay`. |
| 2 | A usage error: an option, step, condition, name, client, descriptor or the Node version. |
| 3 | The create/join failed, or the party ended unexpectedly. |

An end isn't unexpected after `leave`/`end`, or while a `wait` holds. That is why `wait ended=kicked` works.

## `scenario.sh`

`scenario.sh` runs a host and a guest against a running relay:

```sh
conformance/peer/scenario.sh http://127.0.0.1:8080
```

The script goes through these stages:

1. The guest joins.
2. The host shares the media.
3. Both start together.
4. The host pauses, seeks and plays again.
5. The host sets the rate to 1.5x.
6. The host ends the room.

The output is prefixed `host  | `/`guest | `. The script exits non-zero if either peer fails.

See the [TypeScript SDK](../../sdk/typescript/README.md), [SDK.md](../../spec/SDK.md) and the [integration guide](../../docs/integration.md).

# Relay conformance

`owp-conform` checks a relay against [SPEC.md](../../spec/SPEC.md) with `run`, and measures one under load with `load`. Run it after you change the relay, or against a relay you wrote yourself.

The suite has 69 scenarios in 9 groups. Each group is named by the SPEC.md section it checks, from `§3 Endpoints and transport` to `§15 Privacy`.

The tool is its own Go module in this directory. To build it and list the scenarios:

```sh
cd conformance/relay
go build -o bin/owp-conform ./cmd/owp-conform
./bin/owp-conform run --list
```

With no arguments, it prints the usage and exits 2:

```
usage:
  owp-conform run --relay-bin PATH [--timer-scale 0.1] [--ports 18150-18199] [--examples DIR] [--only NAME]
  owp-conform run --url ws[s]://HOST:PORT/v1/ws [--ca-file PEM] [--connect HOST:PORT] [--timer-scale 0.1]
                  [--trust-proxy] [--examples DIR] [--only NAME]
  owp-conform run --list
  owp-conform load --url ws[s]://HOST:PORT/v1/ws [--ca-file PEM] [--connect HOST:PORT] [--pidfile DIR/relay.pid]
                   [--ssh USER@HOST] [--clients 2000] [--room-size 4] [--ramp 500] [--duration 60s]
                   [--action-interval 10s] [--label X]
```

`run -h` and `load -h` print each mode's flags and exit 0. `run` takes exactly one of `--url` and `--relay-bin`. The [run flags](#run-flags) and the [load flags](#load-flags) are listed below.

## Checking a relay binary

In this mode the suite starts, restarts and stops the relay itself. CI runs it this way.

```sh
cd relay && go build -o /tmp/owp-relay ./cmd/owp-relay
cd ../conformance/relay && ./bin/owp-conform run --relay-bin /tmp/owp-relay --timer-scale 0.1
```

The suite runs the relay like this:

- It starts the relay with persistence, a deny list and `--trust-proxy`, in a temporary directory, on the first free port of `--ports`.
- The relay is ready when its first stdout line is `listening <addr>`. That line must come within 10 seconds.
- The suite stops the relay with SIGTERM, and the relay must exit within 10 seconds.

This mode also covers the checks that need process control: restarts and survival on disk, state free of tokens, deny-list reloads, and nothing identifying in the log or the state. `run --list` marks the scenarios that need it. Some of them start extra relays, which use 7 ports in all.

After a clean run, the suite removes the relay's files. On failure it keeps them and prints `relay files kept in DIR`.

A full run takes about 3.5 minutes. The `keepalive` scenario alone takes about 1 minute, because pings never scale.

## Checking a running relay

In this mode the suite tests a relay you started yourself. Start the relay with the same `--timer-scale` you pass the suite:

```sh
owp-relay --addr 127.0.0.1:8080 --timer-scale 0.1 --trust-proxy
./bin/owp-conform run --url ws://127.0.0.1:8080/v1/ws --timer-scale 0.1 --trust-proxy
```

Scenarios that need process control are skipped.

Every suite client always sends its own `X-Forwarded-For`. Pass `--trust-proxy` to tell the suite that the relay uses it, and the per-IP scenarios run. Without it, they are skipped.

With a `wss://` URL, the suite reads `/v1/info` and `/health` over `https://`. Two flags help with TLS, and nothing skips certificate verification:

- `--ca-file PEM` trusts the PEM root certificates in the file besides the system roots, such as those of a test CA like [Pebble](https://github.com/letsencrypt/pebble).
- `--connect HOST:PORT` dials that address for the `--url` host. That host stays the TLS server name and the `Host` header.

## Timing, examples and standing checks

The suite reads the limits and policy from `/v1/info` once, and times every scenario by them. With `--relay-bin`, `--timer-scale` is the scale the suite starts the relay with.

The `examples` scenario sends the files in `spec/examples` byte for byte. It sends each `descriptor` example as the `media` of a `setMedia`. The scenario expects this:

- Every client message and descriptor example in `invalid` gets the refusal SPEC.md §6.2 gives it.
- An example that is not UTF-8 gets close code 1007 instead, and one longer than `maxMessageBytes` gets close code 1009.
- Every `create`, `setMedia`, `descriptor` and `status` example in `valid` is accepted, stored and forwarded.

The scenario looks for `spec/examples` in the working directory and the directories above it. `--examples DIR` names another directory. If there is none, the scenario is skipped.

Throughout the run, the suite checks the ordering of snapshot `rev` on every connection.

## Output and exit status

The suite prints a line per scenario, grouped by section, and then a summary. This is a run with `--only codes`:

```
relay /tmp/owp-relay on ws://127.0.0.1:18150/v1/ws, files in /tmp/owp-conform-3455463094

§5 Relays, room codes and routing
  PASS codes                        30ms
  PASS typed_codes                  10ms

owp-conform: 2 passed, 0 failed, 0 skipped
```

| Exit | Meaning |
|---|---|
| 0 | Nothing failed. Skips are allowed. |
| 1 | A scenario failed, or the relay did not shut down cleanly. |
| 2 | A usage error, or the relay did not start. |
| 130 | You pressed Ctrl-C. |

## Run flags

| Flag | Default | What it does |
|---|---|---|
| `--relay-bin PATH` | | A relay binary to start, restart and stop, instead of `--url`. |
| `--url URL` | | The WebSocket URL of a running relay. |
| `--timer-scale` | `0.1` | The relay's timer scale. With `--relay-bin`, the suite starts the relay with it. |
| `--ports` | `18150-18199` | With `--relay-bin`: the port range for the relays it starts. |
| `--trust-proxy` | off | With `--url`: tells the suite the relay runs with `--trust-proxy`, so the per-IP scenarios run. |
| `--ca-file PEM` | | With `wss://`: also trusts the PEM root certificates in this file. |
| `--connect HOST:PORT` | | Dials this address for the `--url` host. |
| `--examples DIR` | | The `spec/examples` directory, if not the one the suite finds. |
| `--only NAME` | | Runs only the scenarios whose name contains `NAME`. |
| `--list` | | Lists the scenarios and exits. |

## Load mode

`load` measures a relay under many participants. A small run looks like this:

```sh
./bin/owp-conform load --url ws://127.0.0.1:8080/v1/ws --clients 8 --room-size 4 --ramp 50 --duration 3s --action-interval 1s
```

`load` fills rooms of `--room-size` with `--clients` participants, opening `--ramp` new connections per second. Hosts create the rooms, the others join, and the media is shared. While it runs:

- Each host cycles `seek`, `pause`, `seek`, `play`, taking one action about every `--action-interval` (±20 %).
- Each client pings every 5 seconds and sends `status` every 15 seconds.

After `--duration` of steady state, `load` reports join, ping and fan-out latencies and message rates, in lines like `join 8 ok, 0 failed … p50=0.2ms …`. It then prints one `RESULT {json}` line (see `--label`).

A few things to know:

- Clients send their own `X-Forwarded-For`. Use a relay started with `--trust-proxy`, or raise its per-IP limits.
- Runs take turns: each holds `/tmp/owp-bench.lock` while it runs.
- With `--pidfile PATH --ssh USER@HOST --ssh-key KEY`, it samples the relay's CPU and memory from `/proc` over SSH.
- It exits 1 on a failed join or an unexpected disconnect.

### Load flags

| Flag | Default | What it does |
|---|---|---|
| `--url URL` | | The relay's WebSocket URL. Required. |
| `--clients` | `2000` | Participants in all. |
| `--room-size` | `4` | Participants per room. |
| `--ramp` | `500` | New connections per second while ramping up. |
| `--duration` | `60s` | How long it measures the steady state. |
| `--action-interval` | `10s` | The mean time between host actions in each room. 0 means none. |
| `--pidfile PATH` | | The relay's pidfile on the SSH host, relative to the home directory. Without it, nothing is sampled. |
| `--ssh USER@HOST` | | The SSH destination of the relay host. Needed with `--pidfile`. |
| `--ssh-key KEY` | ssh's own | The SSH key. |
| `--label X` | | A label in the `RESULT` line. |
| `--ca-file PEM`, `--connect HOST:PORT` | | The same as for `run`. |

See [the relay reference](../../relay/README.md) for the relay flags used here, and [the vector tables](../vectors/README.md) for the client-side counterpart.

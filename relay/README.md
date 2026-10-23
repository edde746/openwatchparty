# Relay and self-hosting

`owp-relay` is the Open Watch Party relay. It is one Go binary that holds the rooms, runs their playback state machine, enforces the limits, saves state and serves the endpoints of [the protocol](../spec/SPEC.md).

Rooms live in memory. Give the relay a state directory and they survive restarts.

Use this guide when you run a relay: on your own machine while you build and test an app, or on a server for your users. [Self-hosting](#self-hosting) covers the server, and the sections after it are the reference.

## Run it locally

1. In `relay/`, start the relay:

   ```sh
   go run ./cmd/owp-relay --addr 127.0.0.1:8080
   ```

   It prints `listening 127.0.0.1:8080` when it is ready.

2. Point your app at it. The WebSocket is at `ws://127.0.0.1:8080/v1/ws`. An SDK takes the base URL instead: `Relays.at("http://127.0.0.1:8080")`.

3. Check that both listeners answer:

   ```sh
   curl http://127.0.0.1:8080/health
   curl http://127.0.0.1:9090/metrics
   ```

## Self-hosting

`relay/compose.yaml` runs one relay of your own from the published image `ghcr.io/edde746/owp-relay`. The relay terminates TLS itself, with a Let's Encrypt certificate that it gets and renews on its own.

A self-hosted relay serves any app. Its rooms are separate from the public relays' rooms.

### What you need

- Docker Compose.
- An A record for your hostname, and an AAAA record as well if the server has IPv6.
- TCP port 443 open. Port 80 can stay closed.

### Start it

1. Next to `compose.yaml`, create a `.env` file with your hostname:

   ```
   OWP_DOMAIN=relay.example.com
   ```

   Without it, Compose stops with `set OWP_DOMAIN in .env`.

2. Start the relay:

   ```sh
   docker compose up -d
   ```

   It listens on port 8443 in the container, published as port 443 on the host.

3. Point your apps at it. With its base URL, the SDK uses this one relay for everything ([SDK.md](../spec/SDK.md) §3):

   | SDK | Code |
   |---|---|
   | TypeScript, Dart | `Relays.at("https://relay.example.com")` |
   | Kotlin | `Relays.at(http, "https://relay.example.com")` |
   | Swift | `try Relays.at("https://relay.example.com")` |

### Check on it

On the host, these show the metrics and the logs:

```sh
curl http://127.0.0.1:9090/metrics
docker compose logs relay
```

Compose keeps the logs in 5 files of 10 MB each.

`compose.yaml` is sized for the default of 10000 connections, at about 60 KiB each. To allow more, set `OWP_MAX_CONNECTIONS`, then raise `mem_limit` (now 1 GiB) and `GOMEMLIMIT` in proportion.

### Update it

The image tag `1` tracks the latest `1.x.y` release. To update, pull the new image and restart:

```sh
docker compose pull && docker compose up -d
```

The rooms come back from the saved state.

Keep the `relay-state` volume across updates. A new volume orders a new certificate, and [Let's Encrypt allows 5 per exact set of names per 7 days](https://letsencrypt.org/docs/rate-limits/).

Don't back the volume up. Its snapshots carry names and descriptors that should vanish with their room. Losing the volume loses the live rooms, the daily usage counts and the ACME account key.

### Without Docker

Run the binary as a user who may bind port 443, with a state directory `DIR` of your choice:

```sh
owp-relay --addr :443 --tls-domains relay.example.com --state-dir DIR
```

## Configuration

Every flag has an environment variable. The variable replaces the default, and a flag on the command line wins over both. `/v1/info` reports the limits and policy in force.

The values follow these rules:

- Booleans take the values Go's `strconv.ParseBool` accepts, such as `true`, `false`, `1` and `0`.
- Each limit must be at least 1, and each rate must be positive.

It exits with one of these statuses:

| Exit | When |
|---|---|
| 0 | After `--help`, or after a clean shutdown. |
| 1 | It refused the configuration, or a listener failed. |
| 2 | A flag, an argument or an environment variable has an error. |

### General

| Flag | Variable | Default | What it does |
|---|---|---|---|
| `--addr` | `OWP_ADDR` | `:8080` | The public listener's address. |
| `--admin-addr` | `OWP_ADMIN_ADDR` | `127.0.0.1:9090` | The admin listener's address. Empty disables it. |
| `--id` | `OWP_ID` | `local` | The relay ID: 1-32 characters of `a-z 0-9 -`, starting with a letter or digit. |
| `--prefixes` | `OWP_PREFIXES` | all 31 | The first characters of the codes it creates: 1-31 distinct code symbols. |
| `--denylist` | `OWP_DENYLIST` | none | A file of app names to refuse, re-read when it changes. |
| `--state-dir` | `OWP_STATE_DIR` | none | Where it keeps snapshots, usage counts and ACME certificates. |
| `--trust-proxy` | `OWP_TRUST_PROXY` | `false` | Takes the client IP from the first `X-Forwarded-For` hop. |
| `--timer-scale` | `OWP_TIMER_SCALE` | `1` | Test mode: multiplies the scaled policy durations (SPEC.md §13.3). Must be positive. |

### Connection and room limits

| Flag | Variable | Default | What it does |
|---|---|---|---|
| `--max-connections` | `OWP_MAX_CONNECTIONS` | `10000` | Connections in total. Any more get 503. |
| `--max-participants` | `OWP_MAX_PARTICIPANTS` | `32` | Participants per room, disconnected ones included. From 1 to 32. |
| `--max-connections-per-ip` | `OWP_MAX_CONNECTIONS_PER_IP` | `10` | Connections per IPv4 address or IPv6 /64. Any more get 429. |
| `--max-rooms` | `OWP_MAX_ROOMS` | `10000` | Rooms the relay holds. Beyond it, a `create` or a `join` that would make a room gets `relay_full`. |
| `--max-rooms-per-ip` | `OWP_MAX_ROOMS_PER_IP` | `3` | Rooms that one IPv4 address or IPv6 /64 created and the relay still holds. |
| `--ipv6-factor-56` | `OWP_IPV6_FACTOR_56` | `4` | The per-IP multiplier for an IPv6 /56. |
| `--ipv6-factor-48` | `OWP_IPV6_FACTOR_48` | `16` | The per-IP multiplier for an IPv6 /48. |

The per-IP limits count by address group. An IPv4 address is a group on its own. An IPv6 address counts in its /64 at the limit as given (×1), and in its /56 and /48 at the limit times the factors. A request must fit every group it is in.

With `--tls-domains`, `--max-connections-per-ip` also counts the sockets that haven't upgraded to a WebSocket yet. A socket beyond the limit is closed at once.

### Message and join rates

These limits are token buckets. Each has a burst size and a refill per second.

| Flag | Variable | Default | What it does |
|---|---|---|---|
| `--message-burst` | `OWP_MESSAGE_BURST` | `30` | The size of each connection's message bucket. |
| `--messages-per-second` | `OWP_MESSAGES_PER_SECOND` | `10` | Its refill. A message that finds no token gets `rate_limited`. |
| `--join-burst` | `OWP_JOIN_BURST` | `20` | The size of each IP's `join` bucket. |
| `--joins-per-second` | `OWP_JOINS_PER_SECOND` | `1` | The per-IP `join` refill. |
| `--join-burst-total` | `OWP_JOIN_BURST_TOTAL` | `2000` | The size of the relay-wide `join` bucket. |
| `--joins-per-second-total` | `OWP_JOINS_PER_SECOND_TOTAL` | `200` | The relay-wide `join` refill. |

Only `join` uses the join buckets.

### TLS

| Flag | Variable | Default | What it does |
|---|---|---|---|
| `--tls-domains` | `OWP_TLS_DOMAINS` | none | Hostnames for ACME TLS on `--addr`. Needs `--state-dir`. |
| `--acme-directory` | `OWP_ACME_DIRECTORY` | `https://acme-v02.api.letsencrypt.org/directory` | The ACME directory. Must be `https://`. |
| `--acme-email` | `OWP_ACME_EMAIL` | none | The ACME account's contact. |

[TLS and proxies](#tls-and-proxies) explains how the relay serves TLS.

## Endpoints

The relay has two listeners: the public one on `--addr` and the admin one on `--admin-addr`.

| Listener | Endpoint | What it does |
|---|---|---|
| `--addr` | `GET /v1/ws` | The WebSocket. It accepts any `Origin`. |
| `--addr` | `GET /v1/info` | JSON with `protocol` 1, `id`, `limits` and `policy`, sent with CORS `*` and `Cache-Control: no-cache`. |
| `--addr` | `GET /health` | Answers 200 `ok`. |
| `--admin-addr` | `GET /metrics` | The [metrics](#metrics-usage-and-logs) as Prometheus text. |
| `--admin-addr` | `GET /admin/usage` | The daily usage counts as JSON. |

`/admin/usage` answers `{"id":…,"days":{"YYYY-MM-DD":{"<app>":count}}}`.

The admin listener is plain HTTP and unauthenticated, so keep it local. TLS wraps only `--addr`.

[SPEC.md](../spec/SPEC.md) Appendix B lists the close codes and the HTTP refusals at upgrade.

## Deny list file

The deny list file names the apps the relay refuses, one per line. An app's name is the part of its `client` before the first `/`.

```
# Apps this relay refuses, one per line
ExampleApp
```

The relay reads the file like this:

- It trims each line and matches names ASCII case-insensitively.
- It ignores blank lines and lines starting with `#`.
- It re-reads the file when it changes.
- A missing file is an empty list.
- If the file can't be read, it keeps the previous list and logs the error.

A `create`, `join` or `resume` from a listed app gets `client_blocked`. Participants already in rooms stay.

## State and restarts

With `--state-dir`, rooms survive a restart. Without it, nothing survives.

### What the relay saves

The relay creates the state directory with mode 0700 and keeps two files in it:

- `rooms.json` holds all rooms. It stores tokens only as SHA-256 hashes and holds no IPs, but names, clients and descriptors are in it.
- `usage.json` holds the daily usage counts.

The relay rewrites `rooms.json` within 4 seconds of a change, and every 4 seconds while a room plays. A crash therefore loses at most 5 seconds.

### Shutdown and restart

On SIGTERM or SIGINT, the relay shuts down in this order:

1. It stops accepting connections.
2. It saves its state.
3. It closes all connections with code 1001.
4. It waits up to 2 seconds for them to close.
5. It exits with status 0.

On restart, it restores every room created less than 24 hours ago:

- A room with media comes back `paused` at the saved position.
- A room without media comes back `idle`.
- Its participants come back disconnected, and they can resume.

A room is deleted once its last participant is gone. A restored room therefore lasts until its participants resume or their reservations run out.

## Metrics, usage and logs

The admin listener's `/metrics` serves these metrics:

| Metric | Type | What it reports |
|---|---|---|
| `owp_build_info{version}` | Gauge | The relay build. The value is always 1. |
| `owp_connections` | Gauge | Open WebSocket connections. |
| `owp_rooms` | Gauge | Rooms the relay holds. |
| `owp_participants{connected}` | Gauge | Participants in rooms, by whether a connection is attached. |
| `owp_participants_by_client{client}` | Gauge | Participants per app name: the day's top 50 names, and the rest as `(other)`. |
| `owp_rooms_created_total` | Counter | Rooms created. |
| `owp_joins_total` | Counter | Accepted joins. |
| `owp_resumes_total` | Counter | Accepted resumes. |
| `owp_messages_total` | Counter | WebSocket data messages received. |
| `owp_rejected_connections_total{reason}` | Counter | WebSocket upgrades refused by a connection limit. |
| `owp_errors_total{code}` | Counter | Error messages sent, by code. |

The usage counts record accepted creates, joins and resumes per UTC day and app. The relay keeps them for 400 days. They hold no IPs, names, codes or titles.

The relay prints the `listening` line to stdout and logs everything else to stderr, with UTC times. It never logs codes, tokens, names, clients, descriptors, message content or client addresses, and it doesn't log failed TLS handshakes.

## TLS and proxies

The relay can serve TLS itself, or it can sit behind a proxy that terminates TLS for it.

### Serving TLS itself

With `--tls-domains`, `--addr` serves TLS itself. The relay doesn't listen on port 80 or redirect to TLS, and it leaves plain HTTP unanswered.

Each name must be an ASCII DNS name with two or more labels. A wildcard, a trailing dot or an IP address is refused, and so is `localhost`.

The certificates come over [TLS-ALPN-01](https://www.rfc-editor.org/rfc/rfc8737) on TCP port 443. The name must therefore resolve to this server, and port 443 must reach `--addr`. The certificates live in `<state-dir>/acme`.

The relay renews the certificates automatically. It logs each certificate's expiry after it starts, and then daily. To test, point `--acme-directory` at a test CA, such as Let's Encrypt's [staging](https://letsencrypt.org/docs/staging-environment/) environment or [Pebble](https://github.com/letsencrypt/pebble).

At most 1024 sockets wait for a handshake or a request at once. Each address group holds no more than its `--max-connections-per-ip` share of them. A request that doesn't upgrade to a WebSocket ends its connection.

### Behind a proxy

Behind a proxy, leave `--tls-domains` unset. The proxy terminates TLS and forwards `/v1/ws` with the upgrade, `/v1/info` and `/health`, keeping the CORS header. The relay's 30-second pings fit within nginx's default timeout of 60 seconds.

Use `--trust-proxy` only if the proxy overwrites `X-Forwarded-For` with the client's address. In nginx, that is `proxy_set_header X-Forwarded-For $remote_addr;`, together with the [standard upgrade headers](https://nginx.org/en/docs/http/websocket.html). With `--trust-proxy`, a request whose first `X-Forwarded-For` value can't be parsed gets 400.

Proxies must not log IPs either (SPEC.md §15).

To run the Docker image behind a proxy, mount a volume at `/state` and set `OWP_STATE_DIR=/state`. The image runs as UID 65532.

## Build and test

You need Go 1.26 or later. To build a static binary, run this in `relay/`:

```sh
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=1.0.0" -o bin/owp-relay ./cmd/owp-relay
```

Without `-X main.version`, the version is `dev`.

To build the image, run this from the repository root. The image is distroless and runs as user 65532.

```sh
docker buildx build --platform linux/amd64,linux/arm64 --build-arg VERSION=1.0.0 -t ghcr.io/edde746/owp-relay relay
```

To check and test the relay:

```sh
cd relay
gofmt -l .
go vet ./...
go test -race -count=1 ./...
```

`gofmt -l .` must print nothing.

For the full protocol check, see the [conformance suite](../conformance/relay/README.md). See also the [integration guide](../docs/integration.md) and [SECURITY.md](../SECURITY.md).

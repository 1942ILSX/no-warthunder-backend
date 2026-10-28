# Bounded HTTP/3 and WebTransport probe

Run this client **off the server being measured**, against a deployment you own.
It creates at most 25 concurrent rooms, sends legal player inputs at 20 Hz,
cycles Trainee / Expert / Ace, and validates the qualified native-v3 policy IDs,
score multipliers, weighted snapshots, monotonic simulation ticks, and final
results. It never submits a leaderboard ID or record.

```sh
go run ./cmd/h3check -url https://game.example -transport wt \
  -rooms 25 -duration 30s -output /tmp/nwt-wt-report.json

go run ./cmd/h3check -url https://game.example -transport ws \
  -rooms 25 -duration 30s -output /tmp/nwt-ws-report.json

# Verify ordinary application paths also work over HTTP/3, without rooms.
go run ./cmd/h3check -url https://game.example -rooms 0 \
  -get / -get /api/levels -get /api/client-config

# Verify a CDN object over HTTP/3 without requesting game API endpoints.
go run ./cmd/h3check -url https://assets.example -rooms 0 -http-only \
  -get /no-warthunder/releases/RELEASE/assets/game-HASH.js

# A self-signed certificate is permitted only on a loopback test origin.
go run ./cmd/h3check -url https://127.0.0.1:18443 -insecure \
  -rooms 3 -duration 5s
```

`-rooms` is bounded to 0–25 and `-duration` to 1–60 seconds. A room that finishes
normally is replaced by another, up to 20 reservations per worker. A protocol or
connection failure stops that worker instead of retrying in a tight loop. Each
worker uses a separate HTTP connection pool plus a realtime connection, so the
25-room probe includes the connection pattern of separate browsers. With
`-transport wt`, both the room API and realtime connection must use HTTP/3;
there is no implicit TCP fallback hiding a failed QUIC test. The `ws` mode checks
the TCP path independently; it does not test the browser's fallback selection.
`-http-only` requires `-rooms=0` and at least one explicit `-get`; it skips the
normal `/healthz` and `/api/difficulties` checks, allowing the same bounded
HTTP/3 probe to validate CDN/static objects without requesting game endpoints.

JSON includes occupancy, seconds at requested occupancy, snapshot rate and gap
percentiles, acknowledged input sequences, payload bytes, per-slot errors, and
the observed HTTP and realtime protocols. Sessions observed for at least two
seconds must acknowledge player input and average at least 12 snapshots/second
(the target is 15). Frame sizes are bounded to 256 KiB;
GET checks are bounded to 4 MiB each. Reservation tokens are redacted from error
messages. An unclaimed reservation that fails during connection establishment
expires using the server's ordinary reservation timeout.

Payload bytes exclude transport/TLS framing. CPU and memory are deliberately
not inferred from client metrics: sample the server cgroup's CPU, memory,
throttling, and memory events while this command runs. A successful probe is
evidence of protocol behavior, not by itself proof of the 0.5-core / 256-MB
budget. It also does not emulate browser rendering or network impairment.

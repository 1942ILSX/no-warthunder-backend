# Historical scripted baseline — 2026-10-27

This report describes the initial scripted-only build. It is retained as
historical evidence; its model and verification limits do not describe the
current release. See [current validation](VALIDATION.md) for native-v3 results.

## Automated checks

- `go test -race ./...`: passed for game, level catalog, leaderboard and HTTP/WebSocket packages.
- `go vet ./...`: passed.
- Native macOS arm64 build: passed.
- Pure-Go Linux amd64 cross-compilation: passed; resulting executable is statically linked.
- Frontend `npm test`: 8 tests passed (keyboard priority, touch independence, reset behavior, Unicode display-ID validation).
- Frontend `npm run build`: passed TypeScript checking and Vite production bundling. Application JS is approximately 37.17 KB, 13.08 KB gzip; CSS approximately 28.68 KB, 7.05 KB gzip. Fonts are bundled separately and locally.

Game regressions cover deterministic replay, fixed observation shape/ranges, collision and projectile tunneling, destructible terrain, respawn protection, base/player defeat, completion score freezing and defeat precedence when the final kill coincides with objective destruction.

All ten original levels pass geometry, spawn-clearance, reachability, uniqueness and immutable-template tests. Public-input bots can win the first two levels without mutating game state.

Leaderboard checks cover ranking and speedrun ties, independent level boards, top-ten truncation, concurrent submissions, restart persistence, invalid IDs, corrupt-file preservation and persistence failure rollback. Transport checks cover token validation, exact origin checks, room capacity, duplicate controllers, input authority, stale-input clearing, disconnection/shutdown cleanup, opt-in receipt submission and receipt replay rejection.

## Real victory → record integration

`cmd/playtest` created a private server on loopback port 18083 using a temporary leaderboard file. A defender used only ordinary WebSocket inputs and state snapshots; no game state or score was injected.

- First attempt won level 1 with 6 kills.
- Server result: score 2,150; duration 36,000 ms.
- Explicit display-ID submission returned rank 1.
- HTTP leaderboard and reloaded on-disk record matched the server score/time.
- Extra client score field was rejected with HTTP 400.
- Receipt reuse was rejected with HTTP 410.
- The private server and temporary records were cleaned up. No QA ID was added to the application's normal leaderboard.

The current `go run ./cmd/playtest` retains this isolated workflow but now loads
the qualified Trainee model; it does not reproduce the old scripted matchup.

## 25-room load test

Host: Apple M5 Max, macOS arm64, Go 1.27.3. Server configuration: `GOMAXPROCS=1`, `GOMEMLIMIT=160MiB`. The compiled server and load generator ran as separate processes. The generator selected all ten levels, sent 15 inputs/second/room, replaced ended runs, read every snapshot and did not submit records.

| Metric | Observed |
|---|---:|
| Load duration | 60.00 s |
| Peak connected rooms | 25 |
| Average connected rooms | 24.85 |
| Time with all 25 connected | 53.57 s |
| Successful sessions, including replacements | 71 |
| API / WebSocket / protocol errors | 0 / 0 / 0 |
| State snapshots | 22,357 |
| Snapshots per connected-room-second | 14.99 |
| Received application payload | 34,097,309 bytes |
| Server cumulative CPU over 60.55 s sampling window | 3.46 CPU seconds |
| Mean server core fraction | 0.057 |
| Peak server RSS | 19.66 MiB |

The small gap between peak and average concurrency is due to room replacement after defeats and initial connection ramp-up. Payload bytes exclude HTTP/WebSocket/TCP/TLS overhead. The synthetic load bot is not a skill evaluation; its losses are expected.

Separate simulation-only microbenchmark: approximately 27,733 ns for a 25-room step batch, 0 B/op and 0 allocs/op. This excludes snapshots, serialization and networking and must not be presented as full-server capacity.

## Verification limits

- The local browser permission gate denied opening the preview. No alternative browser tool or screenshot workaround was used. Actual visual layout, mobile touch behavior and browser audio remain unverified by browser automation; source review, frontend tests/build and live API/WebSocket smoke checks passed.
- Docker CLI is installed but its daemon was unavailable. The Docker image was not executed, and the exact Linux `0.5 CPU / 256 MB` quota was not tested. A cross-compiled binary and Dockerfile are supplied. Local Apple CPU measurements do not guarantee a particular 3 GHz production CPU's performance.
- The included AI is `scripted-v1`. No trained model, training pipeline or learned capability has been tested. The future actor integration is described in `RL_POLICY.md`.

Run the provided load test again on the target deployment with its actual CPU/memory limits. Record container memory and tick/transport tail latency as well as average CPU; `GOMEMLIMIT` alone is not a hard memory limit.

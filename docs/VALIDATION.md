# Validation — native-v3, 2026-10-28

The runtime now uses the qualified Trainee, Expert and Ace actors. Historical
scripted-only measurements are kept in
[the initial baseline report](VALIDATION_SCRIPTED_2026-10-27.md).

## Current automated checks

- `go test ./...` and `go vet ./...` pass.
- Race checks pass for the server, leaderboard and shared model adapter.
- Model tests cover artifact validation, exact release pins, sampler behavior,
  Python/Go parity and zero per-decision allocations.
- Room tests cover selected-policy execution, fixed difficulty, one-time score
  multiplication, receipt authority, rejected client overrides and cleanup.
- Leaderboard tests cover legacy loading, metadata persistence, one Top 10 per
  level, weighted-score ranking and speedrun ties across difficulties.
- Frontend: 20 tests and TypeScript/Vite production build pass for the integrated
  difficulty UI and all three languages.

## Qualified actors

| Tier | Reference defender win rate | Target range | Categorical temperature |
| --- | ---: | ---: | ---: |
| Trainee | 74.50% | 70–90% | 1.0 |
| Expert | 48.75% | 40–60% | 0.3 |
| Ace | 26.75% | 10–30% | 0.5 |

Each actor was evaluated in 600 fresh matches across ten levels, including 400
primary reference-defender matches and 200 diagnostic matches. All aggregate,
per-level and per-opponent gates passed, with zero timeouts. These rates measure
fixed reference bots, not human players or human enjoyment. Weights and sampler
must remain paired; changing either invalidates these qualification results.
See [the model notes](../models/README.md) and the
[AI release summary](https://github.com/1942ILSX/no-warthunder-ai/blob/main/docs/release-native-v3.json).

## Local HTTP/WebSocket integration

A mixed 25-room smoke test ran for 18 seconds through the actual Vite proxy and
Go application. It used all ten levels and all three release actors:

| Tier | Rooms | State snapshots | Rooms with nonzero score |
| --- | ---: | ---: | ---: |
| Trainee | 9 | 2,430 | 8 |
| Expert | 8 | 2,160 | 6 |
| Ace | 8 | 2,160 | 5 |

Every reservation/welcome reported the expected policy version and multiplier.
All observed scores matched `floor(baseScore * multiplier)`. Invalid difficulty
and client-supplied multiplier requests were rejected, and a 26th room was
refused at capacity. There were no protocol errors. All test rooms were closed,
no ID was submitted, and the existing leaderboard file remained unchanged.
Browser visual layout and human play quality are not covered by this smoke test.

## Bounded target-host resource probe

A separate 30-second headless probe used actual cgroup limits of **0.5 CPU core,
192 MiB RAM, and no swap**. It exercised 25 real Go simulations at 30 Hz,
10 Hz AI decisions and 15 Hz JSON snapshots.

| Metric | Observed |
| --- | ---: |
| Mean CPU cores | 0.06857 |
| Peak process RSS | 9.703 MiB |
| Physics cadence per room | 29.9666 Hz |
| Tick-work p99 | 6.587 ms |
| Dropped ticks / throttled periods | 0 / 0 |
| Live policy decisions | 859.07 / s |
| Supplemental policy decisions | 139.87 / s |
| Mean / peak live enemies | 3.49 per room / 100 total |

Supplemental calls used real delayed observations and independent policy
memory; their outputs were discarded. They are throughput work, not additional
live enemies. The probe includes simulation, inference and JSON serialization
but excludes HTTP/WebSocket/TLS, socket buffers, persistence and other complete
application overhead. It did not install or deploy the application, and all
temporary probe resources were cleaned up without changing existing services.

This is not a sustained full-server capacity guarantee. A production claim for
25 rooms under the 0.5 CPU / 256 MB target still requires the complete server to
be measured under enforced limits. `GOMEMLIMIT` alone is only a soft GC target.
The Dockerfile includes the model assets, but its image has not been executed
as part of this release validation.

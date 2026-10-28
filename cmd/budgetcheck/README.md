# Standalone policy budget check

This command loads three strict tiny-actor artifacts and runs up to 25 independent
real Go games, paced at 30 Hz with the existing 10 Hz policy decisions and 200 ms
observation delay. It does not import the application server, listen on any port,
create HTTP/WebSocket sessions, write leaderboard data, or deploy anything.

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o bin/budgetcheck ./cmd/budgetcheck
GOMAXPROCS=1 GOMEMLIMIT=128MiB ./bin/budgetcheck \
  -trainee /path/to/trainee -expert /path/to/expert -ace /path/to/ace \
  -rooms 25 -duration 30s -scenario max-enemies -serialize=true
```

Add `-inference-floor 1000` to include at least 1,000 combined live and
supplemental MLP calls per simulated second while the actual room workload runs.
Only `0` (default, disabled) and `1000` are accepted. An integer accumulator at
each completed 30 Hz batch credits real live calls first, then performs the
remaining calls (at most 34 supplemental calls per batch). This preserves normal
10 Hz live decision bursts; if the loop misses its cadence, achieved calls per
wall second also fall and the report exposes that failure.

Supplemental calls use a fixed 32-entry ring of actual delayed observations
that the strict actor successfully evaluated during legal setup or live game
decisions. They use the same MLP and sampler with separate `PolicyMemory`, and
their actions are discarded. `mixed` profile rotates supplemental calls among
all three loaded actors; an explicit profile uses that actor only. For `mixed`
scenario startup before the first enemy spawns, a separate disposable real Game
runs at most 120 legal warmup ticks to obtain an observation; measured rooms stay
at their original starting state. This cost belongs to setup. No observation,
tank state, model weight, or live game action is fabricated or altered.

The measurement includes supplemental inference, observation capture and its
bookkeeping in CPU time and work-batch latency. Report fields separate
`live_policy_decisions`, `supplemental_policy_decisions` and
`combined_policy_decisions`, each with its per-second rate; the models also have
individual supplemental and invalid counters. `inference_floor_scheduled_target_calls`
tracks the accumulator target (which may include a partial final batch);
`valid_observations_captured_including_setup` and
`valid_observations_used_for_supplemental_inference` document the real sample
source. These count capture instances, not numerically distinct vectors.
Supplemental calls never recirculate into the source ring. Replacement pre-roll
inference is separately counted and does not substitute for live/floor calls.

This mode tests the actual room workload and full MLP throughput simultaneously.
It does **not** claim 100 real enemies when occupancy is lower: extra calls do
not add tank physics, projectiles, navigation, or construction of additional
observations. Retain actual occupancy alongside the combined throughput result.

The CLI requires `trained:true` and the matching difficulty in each artifact.
The normal strict loader validates schema, shape, checksum, finite weights and
mask compatibility. No sampler override is applied. Every report includes all
model metadata, including policy version, sampler and weights SHA256. Unit tests
use explicitly untrained fixtures through a private test-only helper; the CLI
has no flag to accept these as trained models.

`-duration` is bounded to 1–60 seconds and `-rooms` to 1–25. Initial setup is
separately bounded to 15 seconds. SIGINT/SIGTERM stops the probe. Use an existing
isolated CPU/memory-limited environment if quota verification is needed; this
program never changes cgroups, quotas, services or privileges.

`-setup-pause 30ms` optionally spreads initial room creation across time to reduce
startup CPU bursts on a shared host. It accepts 0–100 ms and defaults to zero.
A context-cancellable timer runs **before each initial room** (25 rooms at 30 ms
add 750 ms of deliberate setup time). The existing total 15-second setup deadline
still applies. The report includes `setup_pause_ms`, setup wall time and setup
CPU time. Measurement ticks and replacement pre-roll have no added pause;
resource limits and pressure-stop thresholds are independent and unchanged.

Scenarios:

- `mixed`: cycles original levels 1–10 and all three actors. New rooms start at
  the normal initial state. Finished runs are replaced immediately.
- `max-enemies`: cycles original levels 7–10, which allow four active enemies.
  Initial and replacement rooms use a bounded legal idle pre-roll to spawn four
  enemies and fill their normal observation warmup. No game internals or map/wave
  settings are mutated. Deaths and normal spawn timing can subsequently reduce
  occupancy, so the report exposes actual live counts and inference throughput.

`-profile mixed|trainee|expert|ace` selects the active room policy mix; all three
artifacts are still loaded. Initial pre-roll cost is reported separately.
Replacement pre-roll cost is included in measured CPU/wall time and identified
separately from live inference. A cheap public-snapshot defender supplies legal
input at 15 Hz. With `-serialize=true` (default), input JSON is encoded/decoded
and state snapshots are encoded at 15 Hz; resulting bytes are counted and discarded.
There is no socket or HTTP traffic. The defender/input encoder cost is included,
although these normally belong to the browser.

The final JSON reports process CPU time, mean CPU cores, lifetime peak RSS,
Go allocation/GC statistics, 30 Hz tick work and wake lateness, dropped/catch-up
ticks, model inference counts, JSON payload throughput, map/profile mix, room
outcomes and actual enemy occupancy. The paced loop uses at most six catch-up
steps, then records dropped debt. On Linux, it also reads the current cgroup v2
CPU/memory limits and counters, including throttling deltas, without writing
anything. Cgroup counters can include other processes in that group, and the
reported limit is the current level rather than every ancestor limit.

This verifies a substantial simulation/inference/serialization workload, not the
complete live server. It excludes HTTP/WebSocket/TLS, socket buffers, per-session
network goroutines, persistence and other application-process overhead. A local
Mac run, a low average CPU number, or `GOMEMLIMIT=128MiB` alone does not prove
compliance with an enforced Linux 0.5 CPU / 256 MiB limit. Keep the artifact hashes,
quota details, occupancy, cadence and tail latency with any result.

Short checks (fixture weights; not a trained-model capacity claim):

```sh
go test -race ./cmd/budgetcheck
go test ./cmd/budgetcheck -run '^$' -bench TwentyFive -benchtime=200ms -benchmem
```

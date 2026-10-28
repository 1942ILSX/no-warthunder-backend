# NO WarThunder — Go backend

An original browser tank-defense game built for 10–25 independent rooms. Go owns movement, collisions, enemy actions, projectiles, scoring, victory and elapsed time. The browser sends directional/fire input and renders snapshots.

The server loads three qualified pretrained models from `models/` at startup.
Players select Trainee, Expert or Ace before reserving a room. The model and
score multiplier stay fixed throughout that run; offline training lives in
[no-warthunder-ai](https://github.com/1942ILSX/no-warthunder-ai).

## Run locally

Requires Go 1.27. Ensure `go` is available on your PATH.

```sh
go run .
```

The API listens on `http://127.0.0.1:8080`. In the frontend project:

```sh
npm ci
npm run dev
```

Open `http://localhost:5173`. Vite proxies `/api` and WebSocket upgrades to Go.
There is no database service or training process to start.

For one-process production serving, build the frontend, then point Go at its output:

```sh
FRONTEND_DIR=../no-warthunder-frontend/dist go run .
```

Use a compiled backend executable in production (`go build -trimpath -o bin/battlecity .`), rather than keeping the Go build tool running.

## Gameplay and records

- All ten original missions are available immediately. Player: WASD / arrow keys, Space to fire; touch controls are available on small screens.
- Defend the relay and clear the entire enemy wave. Three lives, respawn protection, and a relay with three HP create recovery opportunities. Enemy scouts, strikers and heavies have different movement/fire/HP profiles.
- Score is computed by Go. Time is measured on the server from session start to victory. Browser timestamps and submitted scores are ignored/rejected.
- Trainee, Expert and Ace multiply the entire base score by 1.0, 1.5 and 2.0 respectively. Fractional points round down. All difficulties share the same ten-record board for each level; older records retain their original score without an inferred trained difficulty.
- Victory creates a random one-use receipt valid for ten minutes. The player explicitly chooses whether to submit a display ID.
- Each level keeps **only its ten highest-scoring runs**, ordered by score descending, completion time ascending, then completion timestamp ascending. Multiple runs with the same display ID are allowed. A display ID is a label, not an account.
- At most 100 records are persisted, with an atomic replacement file. There is no append-only run history. Abandoned/unsubmitted runs are not persisted; their short-lived receipts are held in bounded memory.
- A disconnect abandons an active run. There is no pause, and a run expires after fifteen minutes. Retrying creates a new room and timer.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `ADDR` | `127.0.0.1:8080` | Bind address; use `0.0.0.0:8080` in a container |
| `MAX_ROOMS` | `25` | Active/reserved room cap |
| `MODELS_DIR` | `models` | Directory containing the three pinned qualified artifacts; invalid or missing models fail startup |
| `LEADERBOARD_FILE` | `data/leaderboard.json` | Persistent top-ten file; one server process owns it |
| `FRONTEND_DIR` | unset | Optional compiled frontend directory containing `index.html` |
| `ALLOWED_ORIGINS` | `http://localhost:5173,http://127.0.0.1:5173` | Extra exact browser origins; same-origin is accepted automatically |
| `GOMEMLIMIT` | Go default | Runtime soft memory limit; start with `160MiB` under a 256 MB container cap |
| `GOMAXPROCS` | Go default | Start with `1` for the half-core deployment; confirm using load tests |

`GOMEMLIMIT` is a GC target, not a hard process/container limit. Set the hard CPU and memory limits in the hosting platform. Keep filesystem data on a persistent volume. For Internet hosting, use HTTPS/WSS through your platform's reverse proxy.

## Architecture

```text
internal/levels       Original immutable templates and geometry validation
internal/game         Deterministic headless simulation and policy boundary
internal/difficulty   Difficulty profiles, score multipliers and release pins
internal/policymlp    Pure-Go MLP inference and strict artifact loader
internal/server       HTTP/WebSocket rooms, fixed-step loop, completion receipts
internal/leaderboard  Bounded ranking and atomic persistence
internal/training     Offline batched worker with the same game rules
cmd/trainenv          Binary IPC bridge for the Python trainer
cmd/policycheck       Cross-language inference and sampler parity
cmd/budgetcheck       Bounded simulation/inference/JSON resource probe
cmd/loadtest          Optional real WebSocket concurrency exercise
```

Simulation: 30 Hz. Policy decisions: 10 Hz. State broadcasts: 15 Hz. A room owns one game instance and serializes its updates. Fixed arrays and shared policy weights keep memory predictable. JSON is deliberately used for the small first release; profiling should drive any future binary protocol migration.

The server's fixed-step simulation time is separate from ranked wall-clock time: catching up or dropping excess simulation debt under CPU contention cannot make a run appear faster on the leaderboard.

## Validation

```sh
go test -race ./...
go vet ./...
go test ./internal/game -bench . -benchmem
go run ./cmd/loadtest -url http://127.0.0.1:8080 -rooms 25 -duration 60s
go run ./cmd/playtest
```

Read [the protocol](PROTOCOL.md), [level design](LEVEL_DESIGN.md), [policy contract](RL_POLICY.md), and [load-test notes](LOAD_TEST.md). Measured performance and verification limits are recorded in [validation notes](VALIDATION.md).

## Container

The Dockerfile produces a pure-Go backend image including all three models at
`/models`. Mount the separately built frontend for one-origin serving:

```sh
docker build -t battlecity-backend .
docker run --rm --name battlecity -p 8080:8080 --cpus=0.5 --memory=256m \
  --mount type=volume,source=battlecity-records,target=/data \
  --mount type=bind,source=/absolute/path/to/frontend/dist,target=/public,readonly \
  -e FRONTEND_DIR=/public battlecity-backend
```

The container runs as an unprivileged user. The named volume inherits the `/data` directory ownership on its first creation. Multiple backend replicas must not share the JSON file; this release intentionally runs as one small instance.

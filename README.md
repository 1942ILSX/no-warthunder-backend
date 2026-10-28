# NO WarThunder — Backend

Server-authoritative tank defense with ten original levels, one player per room,
and three tiny pretrained RL opponents. The Go server owns movement, collisions,
enemy actions, scoring and completion receipts. It supports up to 25 rooms by
default and keeps only the ten best completed runs per level.

| Difficulty | Score multiplier | Model |
| --- | --- | --- |
| Trainee | ×1.0 | `nwt-trainee-v3` |
| Expert (default) | ×1.5 | `nwt-expert-v3` |
| Ace | ×2.0 | `nwt-ace-v3` |

The qualified [models](models/README.md) are included in this repository. Each
has 22,954 parameters and runs directly in Go; no Python service or inference
runtime is needed for gameplay. Weights are shared across rooms. Startup checks
the exact release hashes and categorical sampler settings.

## Run locally

Requires Go 1.27 or later, as declared in `go.mod`.

```sh
git clone https://github.com/1942ILSX/no-warthunder-backend.git
cd no-warthunder-backend
go run .
```

The backend listens on `http://127.0.0.1:8080`. Start the
[frontend](https://github.com/1942ILSX/no-warthunder-frontend) on port 5173 to play.
For a compiled server, run `go build -trimpath -o bin/no-warthunder .` and launch
`./bin/no-warthunder` from the repository root. The default `models/` and
`data/leaderboard.json` paths are relative to the working directory.

## Verify

```sh
go test ./...
go test -race ./internal/server ./internal/leaderboard ./internal/policymlp
go vet ./...
```

The resource target is 0.5 CPU core and 256 MB RAM for 25 rooms. A bounded
simulation/inference/JSON probe and local HTTP/WebSocket checks have passed;
they do not constitute a sustained full-server test under that production
quota. See [validation scope and results](docs/VALIDATION.md).

## Project

- [Frontend](https://github.com/1942ILSX/no-warthunder-frontend): browser rendering, controls and `en` / `zh_cn` / `jp` UI.
- [AI training](https://github.com/1942ILSX/no-warthunder-ai): offline self-play, evaluation and export; not needed to run the game.
- [GitHub Project](https://github.com/users/1942ILSX/projects/1).

Read the [development notes](docs/DEVELOPMENT.md), [local play guide](docs/LOCAL_PLAY.md),
[wire protocol](docs/PROTOCOL.md), [level design](docs/LEVEL_DESIGN.md),
[RL policy contract](docs/RL_POLICY.md), and [training protocol](docs/TRAINING_PROTOCOL.md).

Licensed under the [MIT License](LICENSE).

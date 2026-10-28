# Local play with trained difficulty models

From the backend repository root:

```sh
go run .
```

The server listens on `127.0.0.1:8080` and loads all three models from `models/`
before opening its listener. `MODELS_DIR` can point to another directory
containing the same qualified artifacts. Missing, modified, or unqualified
models are startup errors; the application does not silently switch difficulty
to a scripted policy. When launching a binary from another working directory,
set `MODELS_DIR` and `LEADERBOARD_FILE` to their intended paths.

From the frontend repository root:

```sh
npm run dev -- --port 5173 --strictPort
```

Open `http://127.0.0.1:5173`. Vite proxies `/api`, WebSocket connections, and
`/healthz` to the backend. The backend default is 25 simultaneous rooms.

Choose a level and a difficulty before starting. The room locks that model and
multiplier for the whole run: Trainee ×1.0, Expert ×1.5 (default), or Ace ×2.0.
The multiplier applies to the entire raw game score, including victory bonus;
half-points round down. Snapshot and final `score` values are already weighted,
and `baseScore` contains the raw game score. Clients must not multiply again.

Every level still has a single top ten across all difficulties. Final weighted
score sorts first; shorter duration breaks ties. Difficulty metadata accompanies
new records. Existing records retain their original scores and remain legacy
records without a trained difficulty label. The optional ID submission still
uses a server-issued completion token; clients cannot submit score, difficulty,
or multiplier overrides.

This local setup does not deploy anything to the target server.

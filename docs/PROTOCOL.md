# NO WarThunder — v2 wire contract

Coordinates use tile units and entity centres. The board is 24×24. Directions: 0=up, 1=right, 2=down, 3=left. Input move: 0=idle, 1=up, 2=right, 3=down, 4=left. Fire is independent. Backend simulation runs at 30 Hz, policy decisions at 10 Hz, state broadcasts at 15 Hz. The JSON transport is intended for up to 25 rooms. Version 2 adds difficulty metadata and weighted scores; the leaderboard file format remains version 1 with optional metadata for legacy compatibility.

## Levels

`GET /api/levels` → `{ "levels": Level[] }`.

Level: `{id:1..10,name:string,subtitle:string,description:string,difficulty:1..5,theme:string,grid:string[],playerSpawn:{x,y},base:{x,y},enemySpawns:{x,y}[],enemyCount:number,maxActive:number}`. Grid alphabet: `.` floor, `#` indestructible steel, `+` destructible brick, `~` water (blocks tanks, not bullets), `g` grass (draw over tanks). All layouts are original. Theme is a hex colour. Grid rows are immutable templates; each room copies the grid.

## Difficulty

`GET /api/difficulties` → `{defaultDifficulty:"expert",difficulties:[{id:"trainee",scoreMultiplier:1},{id:"expert",scoreMultiplier:1.5},{id:"ace",scoreMultiplier:2}]}`.

`Difficulty = "trainee" | "expert" | "ace"`. The level's numeric `difficulty`
rating describes the fixed map, independently of the selected opponent model.

## Session

`POST /api/rooms` body `{levelId:number,difficulty?:Difficulty}` → HTTP 201
`{roomId:string,token:string,wsUrl:string,difficulty:Difficulty,scoreMultiplier:number,policyVersion:string}`.
Omitting difficulty selects Expert; an unknown/empty string is rejected.
The room locks its selected model and multiplier until it ends.

The websocket URL is a relative path `/api/rooms/{roomId}/ws?token=...`. Only one connection controls a room. Simulation starts on connection. Capacity errors use HTTP 503 JSON `{code:string,error:string}`. Other API errors use the same shape. The stable `code` lets clients translate messages; `error` retains the English diagnostic for compatibility. Codes include `invalid_level`, `invalid_difficulty`, `policy_unavailable`, `rooms_full`, `run_capacity_full`, `room_creation_failed`, `server_unavailable`, `room_expired`, `room_in_use`, `invalid_submission`, `invalid_id`, `completion_expired`, `submission_in_progress`, `record_save_failed`, `origin_not_allowed`, `not_found`, and `method_not_allowed`.

Level IDs, scores, player IDs and protocol values are language-independent. Level metadata is canonical English; the frontend translates names, subtitles and descriptions by level ID for `en`, `zh_cn` and `jp`. Locale selection does not alter a room or its simulation.

Client WebSocket message: `{type:"input",seq:integer,move:0..4,fire:boolean}`. Sequence increases monotonically. Send at ≤20 Hz plus immediate input changes; server accepts a bounded rate. No client coordinates, score, time or collision outcomes are trusted.

Server greeting: `{type:"welcome",roomId:string,level:Level,state:State,difficulty:Difficulty,scoreMultiplier:number,policyVersion:string}`.

Server update: `{type:"state",state:State}`.

State: `{tick:number,timeMs:number,status:"playing"|"won"|"lost",grid:string[],player:Tank,enemies:Tank[],bullets:Bullet[],effects:Effect[],base:{x,y,hp,maxHp},score:number,baseScore:number,kills:number,totalEnemies:number,enemiesRemaining:number,lives:number,lastInputSeq:number}`.

`score` is already weighted by the backend: `floor(baseScore * scoreMultiplier)`.
The multiplier applies to kills and the complete victory bonus. The frontend
displays this value directly; it must not multiply it again. The deterministic
game/training core retains only the raw score; transport adapts a copy.

Tank: `{id:number,x:number,y:number,dir:0..3,hp:number,maxHp:number,kind:"player"|"scout"|"striker"|"heavy",shield:boolean}`. Bullet: `{id:number,x:number,y:number,dir:0..3,owner:"player"|"enemy"}`. Effect: `{id:number,x:number,y:number,kind:"hit"|"explosion"|"spawn",ttl:number}` (ttl in seconds).

Server final event: `{type:"finished",result:{status:"won"|"lost",levelId:number,score:number,baseScore:number,difficulty:Difficulty,scoreMultiplier:number,policyVersion:string,durationMs:number,kills:number,completionToken?:string}}`. A victory earns a single-use completion token, valid for 10 minutes. Player can decline submission. Closing or losing connection abandons an unfinished run. There is no pause; ranked elapsed time comes from the server monotonic wall clock. State.timeMs in transport snapshots uses this same clock; the headless engine separately tracks deterministic simulation time.

## Records

`GET /api/leaderboard?levelId=1` → `{levelId:1,entries:Record[]}`.

Record: `{id:string,score:number,durationMs:number,completedAt:string,difficulty?:Difficulty,scoreMultiplier?:number,baseScore?:number}`. The id is the player's chosen display ID (1–20 Unicode letters/digits, spaces, `_`, `-`, `.`; no account required). Multiple runs by the same ID are allowed. New records include difficulty metadata; legacy records omit it and retain their original score. A zero baseScore may be omitted by JSON encoding.

`POST /api/leaderboard` body `{completionToken:string,id:string}` → `{rank:number|null,recorded:boolean,entries:Record[]}`. Server derives level, score, duration and difficulty metadata from the token; no client-provided score, multiplier or difficulty is accepted. Results rank by weighted score descending, then durationMs ascending, then completedAt ascending. Each level has one Top 10 shared by all difficulties, at most 100 records total. Submission is explicit, and token reuse is rejected.

`GET /healthz` → `{status:"ok",activeRooms:number,maxRooms:number}`.

## Local development

Go server defaults to `127.0.0.1:8080`. Frontend Vite defaults to `http://localhost:5173`, proxy `/api` and `/healthz` to `http://127.0.0.1:8080`, including WebSocket upgrades. Production can serve frontend `dist` from Go via `FRONTEND_DIR`.

## Go package contract (implementation coordination)

`internal/levels`: `type Point struct { X,Y float64 }`; exported `Level` fields matching JSON above; `All() []Level`, `Get(id int) (Level,bool)`, `Validate(Level) error`.

`internal/game`: `New(level levels.Level, seed int64) *Game`; `(*Game).SetInput(Input)` with `Input{Seq uint64, Move int, Fire bool}`; `(*Game).Step(dt float64)`; `(*Game).Snapshot() State`; `TimeMS int64`, `LastInputSeq uint64`. Its core State contains raw `Score`; server transport adds `baseScore` and weights a copy. Status is a string. The room goroutine owns all Game accesses and calls `SetPolicy` with the selected shared actor before the first step. A bare `game.New` retains the scripted reference default for standalone tooling; application startup requires all qualified models.

# Offline training protocol v1

`go run ./cmd/trainenv` is an offline, rendering-free simulation worker. It never
starts HTTP, writes leaderboard records, or changes the running game server.
Long training is launched separately by the user. The learner and model export
are separate programs; this worker does not train weights.

## Framing and commands

stdin is one UTF-8 JSON command per line, with a `cmd` field. stdout is reserved
for protocol output: a JSON header line followed immediately by the exact raw
array bytes described by `arrays`, in descriptor order. There is **no separator**
after binary data. Each next response begins at the next byte. Diagnostics use
stderr. All multi-byte values are little-endian. Read exact byte counts; never
read binary payload with a line reader.

On startup the worker emits a header with `type:"hello"`, `protocol:1`,
`observation_version:2`, `normalization_version:2`, `enemy_slots:5`,
`observation_size:320`, `defender_observation_version:1`, `action_count:10`,
`critic_size:2688`, `arrays:[]`, and `infos:[]`.

For native artifact evaluation, start `trainenv -enemy-model /absolute/model-dir`.
The worker strictly validates and loads the trained artifact once before writing
the hello or creating worlds. The hello additionally contains
`enemy_policy:{implementation:"go-policymlp",metadata:{...}}`, where `metadata`
is the complete loaded artifact metadata, including `weightsSHA256`, the sampler
mode, temperature, and RNG identifier. This optional field appears only in the
hello; the policy cannot change during that worker's lifetime. Clients must
verify this identity against the artifact they requested. Missing/invalid or
`trained:false` artifacts fail before any protocol output. Omitting the flag
preserves the original protocol and scripted enemy policy.

Commands:

```json
{"cmd":"reset","configs":[{"level_id":1,"seed":42,"max_steps":9000,"defender_bot":"mixed"}]}
{"cmd":"step","enemy_actions":[[-1,-1,-1,-1,-1]],"defender_actions":[-1]}
{"cmd":"reset_indices","indices":[0],"configs":[{"level_id":2,"seed":43,"max_steps":600,"defender_bot":"guard"}]}
{"cmd":"close"}
```

`reset` replaces the whole batch (1–256 worlds). `reset_indices` replaces only
listed worlds; indices must be distinct. Its response contains the entire batch,
with zero rewards for unchanged worlds. Seed is a signed 64-bit integer; use
JSON integers that your language can represent exactly. Defaults: `level_id=1`,
`max_steps=9000`, `defender_bot="mixed"`. `max_steps` counts decision transitions,
not 30Hz physics ticks, and must be 1–9000. A terminal world remains terminal;
subsequent `step` returns the same final state and zero rewards until reset.

Each action is an integer -1 through 9. Enemy -1 runs the installed policy
(scripted by default, the loaded artifact with `-enemy-model`); defender -1 runs
the selected scripted bot. Actions 0–9 use `move*2+fire`. Enemy actions in inactive slots are ignored. Every
step supplies all N worlds and exactly five enemy actions per world. Invalid
commands produce `{type:"error",error:"...",arrays:[],infos:[]}` with no binary
payload and leave the batch usable. EOF/close cancels and joins all workers.

## Response arrays

Every successful reset/step/reset_indices header has `type:"transition"`, the
hello metadata, `arrays:[{name,dtype,shape,nbytes},...]`, and one `infos` object per
world. dtypes are `float32`, `uint8`, or `int32`.

| Name | Shape | Meaning |
|---|---|---|
| enemy_obs | [N,5,320] | Exact delayed production actor features, Observation v2 |
| enemy_mask | [N,5,10] | Legal action masks matching enemy_obs |
| enemy_active | [N,5] | Alive and past the two-decision warmup; actor-loss mask |
| enemy_ids | [N,5] | Stable tank identity; 0 means vacant; a reused slot gets a new ID |
| defender_obs | [N,320] | Defender schema v1 below; never use enemy weights on this input |
| defender_mask | [N,10] | Player action mask |
| defender_active | [N] | Player is alive and room is nonterminal |
| critic_obs | [N,2688] | Current centralized state; training only |
| enemy_reward | [N] | Room-level attacker team reward for the preceding transition |
| defender_reward | [N] | Room-level defender reward |
| terminated | [N] | Physical objective/lives defeat, cleared wave, or actual 900s timeout |
| truncated | [N] | Artificial training step limit |

Reset rewards are zero. Inactive enemy observations are zero except idle mask
feature 298; only action 0 is allowed, preventing an all-masked distribution.
Do not include inactive slots in actor losses. A shared team value is trained
once per room transition, including transitions with no active enemies. The
Python rollout must not end a room when an enemy dies or a new wave member
spawns. Use enemy_ids and active masks to avoid carrying per-agent state across
slot reuse. A final frame has no active actor slots but retains critic state.

`infos`: level_id, seed, outcome (`playing`, `enemy_win_base`, `enemy_win_lives`,
`defender_win`, `timeout`, `step_limit`), tick, decision_step, kills, score,
base_hp, lives, spawned, enemy_alive, enemy_stuck (count with stuckTime>=0.7s).

## Exact decision boundary

The hook is inside Game.Step at the existing 10Hz enemy decision point, after
current-tick player movement/fire and timer updates, before enemy movement/fire
and projectile resolution. It gathers the exact two-sample delayed observations
that normal Policy.Decide would receive, including its two initial idle samples.
It also yields when there are no enemies. Supplied enemy actions apply to this
same tick. Defender input applies beginning with the **next** physics tick,
because the current tick already applied the previously held player input.
Three 30Hz steps normally separate barriers; floating-point cadence is exactly
that of the existing Game.Step rather than a separate reconstructed clock.

The training wrapper runs the real Go core with dt=1/30. The hook suspends using
bounded channels; cancellation unblocks both observation send and action receive.
There is no inference RPC in production. With all enemy actions -1, the hook uses
the ordinary installed Policy path, including the artifact's exact Go MLP,
sampler, seeded per-tank RNG, enemy iteration order, and observation delay.
Immutable weights are shared across worlds and retained through full/selective
resets; newly spawned tanks receive independent production PolicyMemory.
Native evaluation must send -1 instead of sampling replacement actions in Python.

## Defender observation v1 (320 float32)

All values are finite in [-1,1]. Relative positions divide by24. The defender
observes public world state, not enemy policy memories or future actions.

| Slice | Meaning |
|---|---|
| 0:2 | Player x,y /24 |
| 2:6 | Facing one-hot N,E,S,W |
| 6 | Player HP |
| 7 | Current can-fire flag |
| 8:10 | Previous held move/4 and fire |
| 10 | Player shield |
| 11 | Elapsed /900, clipped |
| 12:208 | Same 7x7 terrain channels as enemy schema |
| 208:258 | Five enemy slots, 10 each: relative x,y, facing one-hot4, HP ratio, shield, kind/2 (scout0,striker1,heavy2), exists |
| 258:300 | Six nearest hostile bullets, 7 each: relative x,y, velocity x,y /10, hostile1, closest-approach time/3 clipped, exists |
| 300:304 | Base relative x,y, HP/3, alive |
| 304:307 | Lives/3, enemies remaining /total, spawned /total |
| 307:311 | N,E,S,W navigation cost to base, divided128, unreachable1 |
| 311:315 | Navigation cost to nearest enemy; absent1 |
| 315:319 | N,E,S,W nearby bullet danger after0.45-tile displacement |
| 319 | Remaining simulation-time fraction |

Defender navigation uses current public terrain, treats steel/water/brick as
blocked, ignores tank occupancy, and is a feature helper rather than learned
pathfinding. Bullet slots are nearest by squared distance, with ID tie-breaks.
Enemy slots follow the stable enemy_ids mapping even during enemy warmup.
Defender mask permits all movements; fire is masked when cooling, dead, or three
player shots are active. A dead/terminal defender permits idle only.

## Centralized critic state (2688 float32)

This is a training-only state, separate from either deployment actor input.
0:2304 is the complete 24x24 terrain, four channels per cell as above.
2304:2320 is player x,y, facing one-hot4, HP, shield, can-fire, held move/4,
held fire, lives/3, elapsed/900, then three reserved zeros.
2320:2328 is base x,y, HP/3, alive, then four reserved zeros.
2328:2408 is five 16-value enemy slots: x,y, facing one-hot4, HP ratio,
shield, kind/2, exists, active, then five reserved zeros.
2408:2664 is up to32 bullets by ID, eight values each: x,y, facing one-hot4,
owner (player0/enemy1), exists. Original levels cannot exceed this slot budget;
custom scenarios with more projectiles are truncated by ID.
2664:2688 is level_id/10, difficulty/5, total enemies/32, spawned/total,
remaining/total, kills/total, live enemies/5, lives/3, base HP/3, elapsed/900,
remaining simulation time, terminal flag, then twelve reserved zeros.

## Rewards and termination

Rewards are computed once per whole room transition from actual state changes.
Enemy shaping: +0.03 per base HP lost, +0.02 per player life lost, -0.005 per
enemy HP lost. Defender receives the opposite shaping. Enemy physical victory
(base destroyed or no player lives) gives +1/-1; cleared wave gives -1/+1.
Simultaneous final enemy death and objective loss follows Go's defeat precedence.
There is no reward for firing, moving, survival time, shield hits, or score.

The actual 900-second simulation timeout is an absorbing Game terminal:
`terminated=true`, outcome `timeout`, and both sides receive -0.25 instead of an
enemy victory. An artificial `max_steps` limit is `truncated=true` and adds no
terminal reward. Bootstrap critic values on artificial truncation, not actual
termination. Final actor masks become idle-only; on truncation the critic retains
the continuation-state active flags and terminal flag0. Never convert the core's
generic `lost` status into an enemy victory without checking base/lives/time.
Bounds on damage events keep shaping subordinate to terminal outcomes. Rewards
are not the public leaderboard scoring system.

`defender_bot` is `guard`, `hunter`, `wander`, or `mixed`. Mixed deterministically
selects one of the other three using the episode seed. Bots consume only public
state and legal input. They are bootstrap/evaluation opponents, not trained models.

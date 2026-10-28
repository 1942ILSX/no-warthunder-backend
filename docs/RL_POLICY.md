# Tiny RL actor — integration contract

The server ships three qualified **native-v3** actors for Trainee, Expert and
Ace, loaded from [models/](../models/README.md). Difficulty selection and score
multipliers are integrated into the room API. The offline Go training worker
exposes the same simulation and decision pipeline through
[training protocol v1](TRAINING_PROTOCOL.md). Training remains separate from
live rooms; `game.ScriptedPolicy` is retained for reference bots and bounded
invalid-observation fallback, not as a substitute for a missing release model.

## Replacement seam

`internal/game.Policy` exposes `Name() string` and
`Decide(Observation, *PolicyMemory) Action`. Install a policy with
`Game.SetPolicy(policy)`. The server owns each Game and serializes all calls.
Weights may be shared across rooms; the provided PolicyMemory belongs to one
spawned enemy and must never be shared between enemies or rooms.

The actor may read only its observation and memory. It must not read future
player input, network state, wall-clock time or the mutable Game. No inference
service is required. `internal/policymlp` implements this interface with a pure-Go MLP.

The shipped actor is `320 → 64 → 32 → 10`, using ReLU hidden activations.
It contains 22,954 parameters including biases, or 91,816 bytes of FP32 weights.
See [validation](VALIDATION.md) for the qualification scope. Use the same
observation contract for a larger teacher; a training-only critic may consume privileged
state. A critic is never shipped to the game server.

## Observation v2 (320 float32 values)

Coordinates are tile centres on a 24×24 board. Relative coordinates are divided
by 24, velocity by player speed / player projectile speed respectively. Entity
slots are zero-filled and carry an existence mask. Feature values are finite and
lie in [-1,1]. Any schema or normalization change requires a new version.
ObservationVersion and NormalizationVersion are both 2. Version 2 keeps the
320-value shape and clamps player velocity: turning can snap the player back to
a lane centre, producing an apparent displacement faster than normal movement.
A v1 artifact is not accepted as v2 without re-export and parity validation.

| Slice, zero based, end exclusive | Meaning |
| --- | --- |
| 0:2 | Self x, y / 24 |
| 2:6 | Self facing one-hot: north, east, south, west |
| 6 | Self HP / max HP |
| 7 | min(1, weapon cooldown / 2 seconds) |
| 8:10 | Previous held move / 4 and fire flag |
| 10 | Self spawn shield flag |
| 11 | min(1, simulation elapsed / 180 seconds) |
| 12:208 | 7×7 cells centred on floor(self position), row-major, four channels per cell: brick, steel, water, grass. Out of bounds = steel. Floor = all zero. |
| 208:210 | Visible player's relative x,y / 24 |
| 210:212 | Observed player velocity / 3.15, clipped to [-1,1] |
| 212 | Player HP |
| 213:217 | Player facing one-hot |
| 217:219 | Player visible and shield flags |
| 219:221 | Base relative x,y / 24 |
| 221:224 | Base HP / 3, base alive flag, remaining enemies / total wave |
| 224:266 | Six hostile projectile slots, seven values each: relative x,y / 24, velocity x,y / 10, hostile flag, min(1,time-to-closest-approach/3), existence mask |
| 266:290 | Four closest teammate slots, six values each: relative x,y / 24, facing / 3, HP ratio, role / 3, existence mask |
| 290:294 | Navigation cost from adjacent N,E,S,W cell to visible player; unavailable player = 1 |
| 294:298 | Navigation cost from adjacent N,E,S,W cell to base |
| 298:308 | Ten legal-action flags, also exposed as Observation.Mask |
| 308 | Personality role / 3 (0 base pressure, 1/2 hunters, 3 opportunistic flank) |
| 309:313 | Predicted projectile danger after a 0.45-tile N,E,S,W displacement; one if closest separation <0.5 tiles within0.6 seconds |
| 313:317 | First relevant object down N,E,S,W firing lane: player1, base0.9, shielded player0.4, brick0.3, otherwise0 |
| 317 | min(1, time trying to move without displacement / 2 seconds) |
| 318 | Nearest teammate distance / 24, clipped to1; absent =1 |
| 319 | Number of enemies already spawned / total wave |

Projectile priority is time to closest approach plus twice the closest
separation, using observed constant velocity; it does not predict player input.
Grass conceals the player from enemies more than3.5 tiles away. Hidden player
position, velocity, facing, navigation and firing-lane features are masked.

Navigation fields use room-shared weighted Dijkstra: floor/grass cost1, brick
cost5, steel/water blocked. Costs are divided by128 and clipped to0.95;
unreachable/outside =1. An adjacent tank-blocked movement adds0.2, clipped to1.
Fields are refreshed after terrain changes and player cell changes. These are
explicit feature engineering helpers, not learned behavior.

## Action v1 and cadence

`index = move * 2 + fire`, for ten categorical actions. Move0 is stationary;
move1/2/3/4 are north/east/south/west. Movement immediately sets facing even when
translation is blocked. Fire is processed in that facing. Idle retains facing.
Player bullets move10 tiles/s; enemy bullets5.8 tiles/s. Enemy speed/cooldown vary
by tank kind and level, and belong to the training environment, not the model.

The simulation runs at30Hz. Decisions run at10Hz and actions are held between
decisions. The actor receives a two-sample delayed observation (~200ms); a new
enemy stays idle while that history fills. The training wrapper must reproduce
this delay, action hold, collision geometry and spawn shield. Do not train at
instantaneous observation and add the delay only during deployment.

`ObservationFor(id)` exposes the **current pre-delay** encoder for an offline
adapter. Prefer driving the same Game core and policy injection hook to preserve
the real delayed pipeline. Go simulation is deterministic for a level, seed,
input sequence and delta sequence. Use fixed `dt=1/30` for training and ranked
play. The transport separately measures leaderboard duration using server
wall-clock time, so CPU throttling cannot shorten a recorded speedrun.

Movement actions remain valid at walls because turning to shoot the wall is
meaningful. Fire actions are masked when the observed weapon is cooling or has
an active projectile. Physical firing limits are rechecked against current
state, since the actor's observation is delayed. Invalid move output becomes
idle. The implemented inference adapter rejects invalid/nonfinite observations or
logits and uses a bounded scripted fallback; see its artifact contract below.

## Implemented loader, export format, and parity checker

`internal/policymlp` provides a strict artifact loader, pure-Go FP32 inference,
action-mask validation, deterministic argmax/categorical sampling, and a scripted
fallback for invalid decision inputs. `cmd/policycheck` exposes logits, actions,
and per-tank RNG state for cross-language parity checks, plus a local inference
benchmark. The authoritative format, validation limits, sampling algorithm, and
CLI examples are in the [model adapter contract](../internal/policymlp/README.md).

Artifacts contain `metadata.json` and little-endian FP32 `weights.bin`. Supported
actor shapes are `320→64→32→10` and `320→32→16→10`. Each layer stores row-major
`[output,input]` weights followed by its output bias. Metadata explicitly records
ObservationVersion2, NormalizationVersion2, the action encoding, 10Hz decisions,
two-decision observation delay, architecture, SHA256, training seed, policy
version, sampler settings, and training provenance. The loader validates these
fields rather than inferring compatibility from dimensions alone.

`difficulty` is required and must be exactly `trainee`, `expert`, or `ace`.
These are three independent model artifacts; difficulty is not a runtime
reaction-delay or physics modification. Synthetic/random parity fixtures are
explicitly marked `trained:false`. The parity tool accepts such fixtures for
verification; they are not trained opponents. `trained:true` records exporter
provenance and does not establish gameplay quality.

`internal/difficulty.LoadPolicies` checks all three release artifacts at startup,
including `trained:true`, the exact SHA256, policy version and categorical
sampler temperature (Trainee 1.0, Expert 0.3, Ace 0.5). A room shares the selected
immutable actor and owns its per-tank RNG. The models are feed-forward; no
recurrent state is used. Score multipliers live outside the simulation so they
cannot alter training rewards or compound across snapshots.

Release gates for future model changes:

1. Validate each trainee/expert/ace checkpoint against a frozen Python reference
   corpus, checking logits within a declared FP32 tolerance and checking legal
   actions, sampler state, and the unchanged observation delay. The tooling is
   implemented; new trained artifacts still require their own parity results.
2. Evaluate held-out opponents/maps, stuck rate, idle time, spawn fairness, and
   the intended difficulty ordering before selecting trained artifacts.
3. Before a production capacity claim, benchmark the complete
   25-room server under an enforced 0.5 CPU / 256 MB quota. Record p99 tick
   latency, RSS, container memory, allocations, and throttling. A local matrix
   multiplication benchmark does not satisfy this gate.

## Training design notes

The rendering-free Go simulation and batched binary IPC worker are implemented
in `internal/training` and `cmd/trainenv`. They retain the Go rules and observation
encoder rather than reimplementing physics in Python. An optional decision hook
yields at the actual production barrier, including zero-enemy decisions; explicit
reset/cancel/EOF releases suspended workers. See [the training protocol](TRAINING_PROTOCOL.md)
for distinct defender and centralized critic encodings and reward semantics.

The separate [no-warthunder-ai](https://github.com/1942ILSX/no-warthunder-ai)
project provides the Python/PyTorch training pipeline, including shared enemy
actors, a separate defender learner, and export and evaluation tooling.
The bundled native-v3 actors completed training, evaluation and parity checks.
Curriculum and historical opponent pools support the three independent
difficulty artifacts. Training may use a larger critic or teacher and parallel
environments; deployment requires only the exported actor. No online learning
occurs in a live room.

Score is not automatically an RL reward. Define training rewards independently:
team victory/defeat should dominate, damage/death and potential-based navigation
shaping should remain small, and terminal/timeout behavior must be explicit.
The simulation ends an unfinished run after15 simulated minutes; a training
curriculum may use shorter episodes, but must define whether that is a task
termination or a time-limit truncation and bootstrap value estimates correctly.
All projectiles in a simulation tick resolve before its terminal result. If the
last enemy dies in the same tick as base destruction or the player's final death,
defeat takes precedence and no completion bonus is awarded. Otherwise the last
kill immediately completes the level at the end of that tick.

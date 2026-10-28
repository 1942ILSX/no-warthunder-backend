# Future tiny RL actor — integration contract

The shipped opponent is **scripted-v1**, not a trained model. Offline training,
weights, distillation, PPO/MAPPO, a Python environment adapter and a model loader
are intentionally future work. The game is fully playable with the fallback.

## Replacement seam

`internal/game.Policy` exposes `Name() string` and
`Decide(Observation, *PolicyMemory) Action`. Install a policy with
`Game.SetPolicy(policy)`. The server owns each Game and serializes all calls.
Weights may be shared across rooms; the provided PolicyMemory belongs to one
spawned enemy and must never be shared between enemies or rooms.

The actor may read only its observation and memory. It must not read future
player input, network state, wall-clock time or the mutable Game. No inference
service is required. A small pure-Go MLP can implement this interface directly.

The baseline candidate is `320 → 64 → 32 → 10`, using ReLU hidden activations.
It contains 22,954 parameters including biases, or 91,816 bytes of FP32 weights.
This is a candidate, not a trained capability claim. Use the same observation
contract for a larger teacher; a training-only critic may consume privileged
state. A critic is never shipped to the game server.

## Observation v1 (320 float32 values)

Coordinates are tile centres on a 24×24 board. Relative coordinates are divided
by 24, velocity by player speed / player projectile speed respectively. Entity
slots are zero-filled and carry an existence mask. Feature values are finite and
lie in [-1,1]. Any schema or normalization change requires a new version.

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
| 210:212 | Observed player velocity / 3.15 |
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
idle. Future inference must reject nonfinite logits and fall back safely.

## Intended export format (loader not implemented yet)

Use a versioned metadata document and little-endian FP32 weights. Metadata must
include `observationVersion:1`, `inputSize:320`, `hiddenSizes:[64,32]`,
`outputSize:10`, `activation:"relu"`, `actionEncoding:"move*2+fire"`,
`decisionHz:10`, `observationDelayDecisions:2`, layer shapes, checksum, training
seed and policy version. Each layer stores row-major `[output,input]` weights
followed by output bias. Store normalization version explicitly; do not infer
it from tensor dimensions. Load and validate once at startup.

TODO when a real model exists:

1. Implement a bounded pure-Go inference adapter and strict artifact loader.
2. Export a frozen Python reference with a corpus of observations and expected
   logits; compare Go output within a declared numeric tolerance.
3. Verify action masking, sampling/argmax, delay and any recurrent state reset.
4. Keep a deterministic seeded sampler if sampling is used. Do not use global RNG.
5. Benchmark full25-room server workload within0.5CPU /256MB, not just matrix
   multiplication. Record p99 tick latency, RSS, container memory and allocations.
6. Evaluate held-out maps/opponents, stuck rate, idle time and spawn fairness.

## Training design notes

Use a rendering-free Go simulation with a batched Python bridge. Keep the Go
rules and observation encoder authoritative, so a Python reimplementation cannot
silently diverge. Start with curriculum tasks and shared enemy actor parameters,
then use separate attacker/defender policies and a historical opponent pool.
Training can use a larger critic or teacher and many parallel environments;
deployment requires only the actor. No online learning occurs in a live room.

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

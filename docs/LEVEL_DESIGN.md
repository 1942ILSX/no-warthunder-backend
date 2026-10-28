# Original arenas

All ten arenas were authored for this project. None reproduces a Battle City map. Their layouts are fixed, rather than generated per run, so players on the same leaderboard face the same geometry and wave budget. Every level is available from the main page; progression is a suggested order rather than an unlock requirement.

The board is 24×24 cells, with a closed steel perimeter. Coordinates refer to cell centres. Grid symbols are `.` floor, `#` steel, `+` brick, `~` water, and `g` grass. Water blocks tank movement but permits projectiles. Grass is visual cover, not an impassable wall. Brick removal permanently opens a route for that room only.

## Arena intent

| ID | Arena | Difficulty | Enemy budget / active | Intended decision |
|---|---|---|---|---|
| 01 | First Light | 1/5 | 6 / 2 | Learn movement and brick destruction in a spacious depot. A steel centre breaks the direct approach; broad flanks leave room to recover. |
| 02 | Switchyard | 1/5 | 8 / 2 | Switch between staggered longitudinal lanes. Steel tracks have deliberate gaps, and brick sidings offer optional shortcuts. |
| 03 | Canal Crossing | 2/5 | 8 / 3 | Defend offset bridges across two canals. Firing across water is useful, but returning to the relay requires a real crossing. |
| 04 | Glasshouse | 2/5 | 10 / 3 | Read movement through grass beds and keep more than one approach in view. A broken central greenhouse offers several exits instead of a single camping spot. |
| 05 | The Kilns | 3/5 | 10 / 3 | Make new doorways in four brick workshops. Steel furnace cores preserve cover after the outer walls disappear. |
| 06 | Split Current | 3/5 | 12 / 3 | Move between unequal lanes across a long spillway. The western relay makes the decision to cross strategically meaningful. |
| 07 | Crossfire | 4/5 | 12 / 4 | Use corners of offset steel islands to break overlapping firing lines. Each long approach has a lateral escape. |
| 08 | Relay Station | 4/5 | 14 / 4 | Orbit a central strongpoint while protecting an eastern relay. Brick screens reshape the flank routes during a wave. |
| 09 | Fault Line | 5/5 | 14 / 4 | Find broad breaches in a diagonal watercourse. Positioning matters because projectiles ignore the route restrictions imposed on tanks. |
| 10 | Last Broadcast | 5/5 | 16 / 4 | Combine outer flanks, a broken steel compound, and an inner brick courtyard. Six outer openings keep the final arena from becoming a one-door siege. |

## Geometry guarantees

- Each player/enemy spawn has a clear 3×3 floor apron. Tank centres sit on half-cell coordinates, and a tank of width 0.8 cells fits the narrowest traversable lane.
- Every enemy spawn can reach both the initial player position and the base along floor/grass **without** destroying brick. Demolition provides tactical alternatives; it is not required to make the scripted fallback playable.
- The three enemy spawn points are separated across the northern edge. Objectives and player spawns occupy different cells in the southern area, with base placement varying across levels.
- Layouts use deterministic authoring primitives in `internal/levels/layouts.go`. These primitives expand into fixed string rows; no random generation or copied external level data is used.
- `Validate` checks dimensions, tile alphabet, perimeter, coordinates, spawn aprons, metadata, wave limits, and connectivity. The package validates its built-in catalog during initialization.
- `All` and `Get` clone the mutable slices. A room that removes a brick cannot alter the catalog or another room.

## Balance and future RL

The catalog intentionally limits active enemies to 2–4 and total enemies to 6–16 for the initial release. These numbers are starting balance values, not a claim of completed human playtesting. Automated geometry tests confirm reachability; they cannot establish fun or eliminate every possible emergent firing pattern.

The future policy should receive this same grid and exact movement/collision semantics. Treat water separately from steel in projectile threat estimation, recompute navigation fields after brick destruction, and preserve grass visibility rules between training and production. Use a distinct evaluation map set for training experiments; keeping the ten ranked production maps fixed is not a reason to overfit the model to them.

Changing a ranked level's geometry, wave budget, movement balance, or scoring changes the challenge. Before such a release, explicitly decide whether to reset that level's ten records or introduce a new ruleset/leaderboard version. Do not silently compare incompatible runs.

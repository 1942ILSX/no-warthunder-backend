package game

import (
	"math"

	"BattleCityReforge/internal/levels"
)

func New(level levels.Level, seed int64) *Game {
	g := &Game{
		level: level, status: "playing", lives: 3, nextID: 2,
		random:     uint64(seed) ^ 0x9e3779b97f4a7c15,
		spawnTimer: 1.0, policy: ScriptedPolicy{}, navPlayerCell: -1,
		enemies: make([]tankState, 0, 8), bullets: make([]projectile, 0, 32),
		effects: make([]Effect, 0, 32), navDirty: true,
		base: Base{X: level.Base.X, Y: level.Base.Y, HP: 3, MaxHP: 3},
	}
	for y := 0; y < BoardSize; y++ {
		for x := 0; x < BoardSize; x++ {
			g.grid[y][x] = '#'
			if y < len(level.Grid) && x < len(level.Grid[y]) {
				g.grid[y][x] = level.Grid[y][x]
			}
		}
		g.gridRows[y] = string(g.grid[y][:])
	}
	g.player = tankState{Tank: Tank{ID: 1, X: level.PlayerSpawn.X, Y: level.PlayerSpawn.Y,
		Dir: 0, HP: 1, MaxHP: 1, Kind: "player", Shield: true}, shieldTime: 3, speed: PlayerSpeed, shotInterval: .3}
	g.addEffect(g.player.X, g.player.Y, "spawn", .8)
	return g
}

// SetPolicy installs a shared inference implementation. Pass nil to restore the
// explicitly scripted fallback. No pretrained weights are bundled with v1.
func (g *Game) SetPolicy(policy Policy) {
	if policy == nil {
		policy = ScriptedPolicy{}
	}
	g.policy = policy
	for i := range g.enemies {
		g.enemies[i].memory = PolicyMemory{}
	}
}

func (g *Game) PolicyName() string { return g.policy.Name() }

func (g *Game) SetInput(in Input) {
	if in.Seq < g.input.Seq || g.status != "playing" {
		return
	}
	if in.Move < 0 || in.Move > 4 {
		in.Move = 0
	}
	g.input = in
}

// ClearInput lets transport stop a tank on stale input without inventing a client
// sequence. The websocket controller should clear input after 500 ms of silence.
func (g *Game) ClearInput() { g.input.Move, g.input.Fire = 0, false }

// Step advances simulated time, never wall-clock time. Production passes 1/30.
// Ranked completion duration is measured separately by the server so CPU quota
// throttling cannot make a slow real-world run appear artificially fast.
func (g *Game) Step(dt float64) {
	if g.status != "playing" || dt <= 0 || math.IsNaN(dt) || math.IsInf(dt, 0) {
		return
	}
	// Bound integration work even when a caller supplies an accidental huge delta.
	if dt > .1 {
		dt = .1
	}
	g.tick++
	g.elapsed += dt
	g.ageEffects(dt)
	g.updateTankTimers(&g.player, dt)
	for i := range g.enemies {
		g.updateTankTimers(&g.enemies[i], dt)
	}
	if g.elapsed >= MaxSimulationSeconds {
		g.status = "lost"
		return
	}
	if g.player.HP <= 0 {
		g.respawnTimer -= dt
		if g.respawnTimer <= 0 && g.lives > 0 {
			g.respawnPlayer()
		}
	}
	oldX, oldY := g.player.X, g.player.Y
	if g.player.HP > 0 {
		g.player.action = Action{Move: g.input.Move, Fire: g.input.Fire}
		g.moveTank(&g.player, dt)
		if g.input.Fire {
			g.fire(&g.player)
		}
	}
	g.playerVelocity = [2]float64{(g.player.X - oldX) / dt, (g.player.Y - oldY) / dt}
	g.decisionTimer -= dt
	if g.decisionTimer <= 0 {
		g.decisionTimer += 1.0 / DecisionHz
		g.refreshNavigation()
		for i := range g.enemies {
			g.decide(&g.enemies[i])
		}
	}
	for i := range g.enemies {
		t := &g.enemies[i]
		if t.HP <= 0 {
			continue
		}
		x, y := t.X, t.Y
		g.moveTank(t, dt)
		if t.action.Move != 0 && math.Abs(t.X-x)+math.Abs(t.Y-y) < .0001 {
			t.stuckTime += dt
		} else {
			t.stuckTime = 0
		}
		if t.action.Fire {
			g.fire(t)
		}
	}
	g.moveBullets(dt)
	g.removeDeadEnemies()
	// Resolve all projectiles in this simulation tick before deciding victory.
	// Losing the final life/base in the same tick as the last kill is a defeat;
	// it never produces a completion token or a survival bonus.
	if g.base.HP <= 0 || g.lives <= 0 {
		g.status = "lost"
		return
	}
	if g.kills >= g.level.EnemyCount && g.level.EnemyCount > 0 {
		g.status = "won"
		// Completion and survival are fixed score components. Duration is only the
		// leaderboard tie-breaker, avoiding counting speed twice.
		g.score += 1000 + g.lives*250 + g.base.HP*100
		return
	}
	g.spawnTimer -= dt
	if g.spawnTimer <= 0 && g.spawned < g.level.EnemyCount && len(g.enemies) < g.level.MaxActive {
		if g.spawnEnemy() {
			g.spawnTimer = math.Max(1.5, 3.4-float64(g.level.Difficulty)*.25)
		} else {
			g.spawnTimer = .4
		}
	}
}

func (g *Game) Snapshot() State {
	if g.gridDirty {
		for y := range g.grid {
			g.gridRows[y] = string(g.grid[y][:])
		}
		g.gridDirty = false
	}
	s := State{Tick: g.tick, TimeMS: int64(math.Round(g.elapsed * 1000)), Status: g.status,
		Grid: append([]string(nil), g.gridRows[:]...), Player: g.player.Tank,
		Enemies: make([]Tank, 0, len(g.enemies)), Bullets: make([]Bullet, 0, len(g.bullets)),
		Effects: append([]Effect{}, g.effects...), Base: g.base, Score: g.score,
		Kills: g.kills, TotalEnemies: g.level.EnemyCount, EnemiesRemaining: max(0, g.level.EnemyCount-g.kills),
		Lives: g.lives, LastInputSeq: g.input.Seq}
	for _, t := range g.enemies {
		if t.HP > 0 {
			s.Enemies = append(s.Enemies, t.Tank)
		}
	}
	for _, b := range g.bullets {
		if !b.dead {
			s.Bullets = append(s.Bullets, b.Bullet)
		}
	}
	return s
}

func (g *Game) updateTankTimers(t *tankState, dt float64) {
	t.cooldown = math.Max(0, t.cooldown-dt)
	t.shieldTime = math.Max(0, t.shieldTime-dt)
	t.Shield = t.shieldTime > 0
}

func (g *Game) moveTank(t *tankState, dt float64) {
	if t.action.Move < 1 || t.action.Move > 4 || t.HP <= 0 {
		return
	}
	t.Dir = t.action.Move - 1
	dx, dy := directionX[t.Dir]*t.speed*dt, directionY[t.Dir]*t.speed*dt
	// Cardinal tanks gently align to lanes while turning near a cell centre. This
	// makes keyboard turns usable without introducing diagonal movement speed.
	if dx != 0 {
		centre := math.Floor(t.Y) + .5
		if math.Abs(t.Y-centre) < .23 && g.canOccupy(t.ID, t.X, centre) {
			t.Y = centre
		}
	} else {
		centre := math.Floor(t.X) + .5
		if math.Abs(t.X-centre) < .23 && g.canOccupy(t.ID, centre, t.Y) {
			t.X = centre
		}
	}
	steps := int(math.Ceil(math.Max(math.Abs(dx), math.Abs(dy)) / .1))
	if steps < 1 {
		steps = 1
	}
	for range steps {
		x, y := t.X+dx/float64(steps), t.Y+dy/float64(steps)
		if !g.canOccupy(t.ID, x, y) {
			break
		}
		t.X, t.Y = x, y
	}
}

func (g *Game) canOccupy(id int, x, y float64) bool {
	if x-TankRadius < 0 || y-TankRadius < 0 || x+TankRadius >= BoardSize || y+TankRadius >= BoardSize {
		return false
	}
	for cy := int(math.Floor(y - TankRadius)); cy <= int(math.Floor(y+TankRadius)); cy++ {
		for cx := int(math.Floor(x - TankRadius)); cx <= int(math.Floor(x+TankRadius)); cx++ {
			if isTankWall(g.grid[cy][cx]) {
				return false
			}
		}
	}
	if g.base.HP > 0 && overlaps(x, y, TankRadius, g.base.X, g.base.Y, .43) {
		return false
	}
	if id != 1 && g.player.HP > 0 && overlaps(x, y, TankRadius, g.player.X, g.player.Y, TankRadius) {
		return false
	}
	for i := range g.enemies {
		o := &g.enemies[i]
		if o.ID != id && o.HP > 0 && overlaps(x, y, TankRadius, o.X, o.Y, TankRadius) {
			return false
		}
	}
	return true
}

func isTankWall(tile byte) bool { return tile == '#' || tile == '+' || tile == '~' }

func overlaps(x1, y1, r1, x2, y2, r2 float64) bool {
	return math.Abs(x1-x2) < r1+r2 && math.Abs(y1-y2) < r1+r2
}

func (g *Game) fire(t *tankState) {
	if t.cooldown > 0 || t.HP <= 0 || len(g.bullets) >= 64 {
		return
	}
	count := 0
	for _, b := range g.bullets {
		if !b.dead && b.sourceID == t.ID {
			count++
		}
	}
	maxShots := 1
	if t.Kind == "player" {
		maxShots = 3
	}
	if count >= maxShots {
		return
	}
	owner, speed := "enemy", EnemyBulletSpeed
	if t.Kind == "player" {
		owner, speed = "player", PlayerBulletSpeed
	}
	b := projectile{Bullet: Bullet{ID: g.allocateID(), X: t.X + directionX[t.Dir]*.41,
		Y: t.Y + directionY[t.Dir]*.41, Dir: t.Dir, Owner: owner}, speed: speed, sourceID: t.ID}
	g.bullets = append(g.bullets, b)
	t.cooldown = t.shotInterval
}

func (g *Game) moveBullets(dt float64) {
	for i := range g.bullets {
		b := &g.bullets[i]
		if b.dead {
			continue
		}
		// Substeps shorter than either a wall or a tank prevent tunnelling, even
		// after a 100 ms integration step. Collision also checks the muzzle point.
		steps := max(1, int(math.Ceil(b.speed*dt/.09)))
		for step := 0; step <= steps; step++ {
			if step > 0 {
				b.X += directionX[b.Dir] * b.speed * dt / float64(steps)
				b.Y += directionY[b.Dir] * b.speed * dt / float64(steps)
			}
			if g.hitWorld(b) {
				break
			}
		}
	}
	live := g.bullets[:0]
	for _, b := range g.bullets {
		if !b.dead {
			live = append(live, b)
		}
	}
	g.bullets = live
}

func (g *Game) hitWorld(b *projectile) bool {
	if b.X < 0 || b.Y < 0 || b.X >= BoardSize || b.Y >= BoardSize {
		b.dead = true
		return true
	}
	x, y := int(b.X), int(b.Y)
	if tile := g.grid[y][x]; tile == '#' || tile == '+' {
		b.dead = true
		if tile == '+' {
			g.grid[y][x] = '.'
			g.gridDirty = true
			g.navDirty = true
		}
		g.addEffect(b.X, b.Y, "hit", .22)
		return true
	}
	if b.Owner == "enemy" && g.base.HP > 0 && overlaps(b.X, b.Y, .07, g.base.X, g.base.Y, .43) {
		b.dead = true
		g.base.HP--
		g.addEffect(g.base.X, g.base.Y, "explosion", .65)
		return true
	}
	if b.Owner == "enemy" && g.player.HP > 0 && overlaps(b.X, b.Y, .07, g.player.X, g.player.Y, TankRadius) {
		b.dead = true
		if !g.player.Shield {
			g.player.HP = 0
			g.lives--
			g.respawnTimer = 1.2
			g.addEffect(g.player.X, g.player.Y, "explosion", .7)
		}
		return true
	}
	if b.Owner == "player" {
		for i := range g.enemies {
			t := &g.enemies[i]
			if t.HP > 0 && overlaps(b.X, b.Y, .07, t.X, t.Y, TankRadius) {
				b.dead = true
				if !t.Shield {
					t.HP--
					if t.HP <= 0 {
						g.kills++
						g.score += enemyScore(t.Kind)
						g.addEffect(t.X, t.Y, "explosion", .65)
					} else {
						g.addEffect(t.X, t.Y, "hit", .25)
					}
				}
				return true
			}
		}
	}
	// Player and enemy projectiles can cancel. Same-team shots pass each other.
	for i := range g.bullets {
		o := &g.bullets[i]
		if o.ID != b.ID && !o.dead && o.Owner != b.Owner && overlaps(b.X, b.Y, .1, o.X, o.Y, .1) {
			o.dead = true
			b.dead = true
			g.addEffect(b.X, b.Y, "hit", .18)
			return true
		}
	}
	return false
}

func enemyScore(kind string) int {
	switch kind {
	case "heavy":
		return 200
	case "striker":
		return 150
	default:
		return 100
	}
}

func (g *Game) removeDeadEnemies() {
	live := g.enemies[:0]
	for _, t := range g.enemies {
		if t.HP > 0 {
			live = append(live, t)
		}
	}
	g.enemies = live
}

func (g *Game) spawnEnemy() bool {
	if len(g.level.EnemySpawns) == 0 {
		return false
	}
	start := int(g.nextRandom() % uint64(len(g.level.EnemySpawns)))
	for offset := range len(g.level.EnemySpawns) {
		p := g.level.EnemySpawns[(start+offset)%len(g.level.EnemySpawns)]
		if !g.canOccupy(-1, p.X, p.Y) {
			continue
		}
		kind, hp, speed, interval := "scout", 1, 1.42, 1.65
		if g.level.Difficulty >= 2 && g.spawned%3 == 1 {
			kind, speed, interval = "striker", 1.7, 1.35
		}
		if g.level.Difficulty >= 3 && g.spawned%4 == 3 {
			kind, hp, speed, interval = "heavy", 2, 1.12, 1.8
		}
		speed += float64(g.level.Difficulty-1) * .045
		t := tankState{Tank: Tank{ID: g.allocateID(), X: p.X, Y: p.Y, Dir: 2, HP: hp, MaxHP: hp, Kind: kind, Shield: true},
			shieldTime: .7, cooldown: 1.2, speed: speed, shotInterval: interval, role: g.spawned % 4}
		t.memory.Random = g.nextRandom()
		g.enemies = append(g.enemies, t)
		g.spawned++
		g.addEffect(p.X, p.Y, "spawn", .8)
		return true
	}
	return false
}

func (g *Game) respawnPlayer() {
	p := g.level.PlayerSpawn
	// Spawn protection and a small area clear guarantee a life is usable even if
	// enemies have occupied the original spawn. Reposition, don't award free kills.
	if !g.canOccupy(1, p.X, p.Y) {
		found := false
		for r := 1; r <= 4 && !found; r++ {
			for dy := -r; dy <= r && !found; dy++ {
				for dx := -r; dx <= r; dx++ {
					if g.canOccupy(1, p.X+float64(dx), p.Y+float64(dy)) {
						p.X += float64(dx)
						p.Y += float64(dy)
						found = true
						break
					}
				}
			}
		}
		if !found {
			g.respawnTimer = .2
			return
		}
	}
	g.player.X, g.player.Y = p.X, p.Y
	g.player.Dir = 0
	g.player.HP = 1
	g.player.Shield = true
	g.player.shieldTime = 3
	g.player.cooldown = .2
	g.addEffect(p.X, p.Y, "spawn", .8)
}

func (g *Game) addEffect(x, y float64, kind string, ttl float64) {
	if len(g.effects) >= 48 {
		return
	}
	g.effects = append(g.effects, Effect{ID: g.allocateID(), X: x, Y: y, Kind: kind, TTL: ttl})
}

func (g *Game) ageEffects(dt float64) {
	live := g.effects[:0]
	for _, e := range g.effects {
		e.TTL -= dt
		if e.TTL > 0 {
			live = append(live, e)
		}
	}
	g.effects = live
}

func (g *Game) allocateID() int { id := g.nextID; g.nextID++; return id }

func (g *Game) nextRandom() uint64 {
	g.random ^= g.random << 13
	g.random ^= g.random >> 7
	g.random ^= g.random << 17
	if g.random == 0 {
		g.random = 0x2545f4914f6cdd1d
	}
	return g.random
}

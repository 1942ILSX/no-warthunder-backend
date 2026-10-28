package game

import "math"

const (
	ObservationVersion = 1
	ObservationSize    = 320
	ActionCount        = 10
)

// Observation is the deployment input contract, not training-only privileged
// state. Features are normalized and finite, with entity existence masks.
// See docs/RL_POLICY.md for every offset. Values include the legal-action mask
// at 298:308; Mask is duplicated here to make validation explicit.
type Observation struct {
	Version int
	Values  [ObservationSize]float32
	Mask    [ActionCount]bool
}

// Action index is Move*2 + bool(Fire). Movement changes facing immediately;
// blocked movement still turns, so directions are not masked at walls.
type Action struct {
	Move int
	Fire bool
}

func (a Action) Index() int {
	n := a.Move * 2
	if a.Fire {
		n++
	}
	return n
}

func ActionFromIndex(index int) Action {
	if index < 0 || index >= ActionCount {
		return Action{}
	}
	return Action{Move: index / 2, Fire: index%2 == 1}
}

// PolicyMemory is private to one spawned tank. Shared model weights MUST NOT
// contain mutable room state. Hidden is reserved for an optional tiny recurrent
// actor; the initial intended model is a feed-forward MLP and leaves it zero.
type PolicyMemory struct {
	Decisions  uint64
	Random     uint64
	LastAction Action
	Hidden     [32]float32
}

// Policy is the replacement seam for a future pretrained actor. Decide must be
// bounded, synchronous, deterministic for a supplied memory/observation pair,
// and allocation-light. It cannot access sockets, player inputs, Game, or time.
// Implement Name and Decide in a future model adapter, validate weights once at
// load, and inject it via SetPolicy. No inference RPC is needed for a tiny MLP.
type Policy interface {
	Name() string
	Decide(Observation, *PolicyMemory) Action
}

// ScriptedPolicy is an honest playable fallback, NOT a trained RL model. It uses
// exactly the actor observation and the same delayed decision cadence. It is
// also a useful fixed opponent for future offline training and evaluation.
type ScriptedPolicy struct{}

func (ScriptedPolicy) Name() string { return "scripted-v1" }

func (ScriptedPolicy) Decide(o Observation, m *PolicyMemory) Action {
	v := &o.Values
	if m.Random == 0 {
		m.Random = 0x9e3779b97f4a7c15
	}
	m.Random ^= m.Random << 13
	m.Random ^= m.Random >> 7
	m.Random ^= m.Random << 17
	m.Decisions++
	role := int(math.Round(float64(v[308]) * 3))
	nav := 290
	// Base attackers pressure the objective; hunters fight the player; flankers
	// switch to the objective when the defender is far away or currently absent.
	playerDistance := math.Abs(float64(v[208])) + math.Abs(float64(v[209]))
	if role == 0 || v[217] == 0 || (role == 3 && playerDistance > .32) {
		nav = 294
	}
	bestDir := 0
	bestCost := float32(10)
	for dir := 0; dir < 4; dir++ {
		cost := v[nav+dir] + v[309+dir]*.8
		// A small deterministic tie-break prevents identical tanks stacking into
		// one lane. It is not online learning or a hidden difficulty adjustment.
		cost += float32((m.Random>>uint(dir*8))&255) / 255 * .012
		if m.LastAction.Move == dir+1 {
			cost -= .008
		}
		if cost < bestCost {
			bestCost = cost
			bestDir = dir
		}
	}
	// When another tank blocks a corridor, back off sideways before trying again.
	if v[317] > .35 {
		bestDir = int((m.Random >> 16) % 4)
	}
	a := Action{Move: bestDir + 1}
	// The tank faces along movement. A firing line to a real target is worth a
	// deliberate turn; bricks are only fired upon along the chosen route.
	for dir := 0; dir < 4; dir++ {
		if v[313+dir] >= .8 && v[309+dir] < .5 {
			a.Move = dir + 1
			// Briefly plant the tank only when it already faces the firing line.
			if v[2+dir] > 0 && m.Decisions%3 != 0 {
				a.Move = 0
			}
			a.Fire = o.Mask[a.Index()+1]
			m.LastAction = a
			return a
		}
	}
	a.Fire = v[313+bestDir] > .1 && o.Mask[a.Index()+1]
	m.LastAction = a
	return a
}

func (g *Game) decide(t *tankState) {
	current := g.observe(t)
	// Two samples are a ~200 ms observation delay at10Hz. Empty initial history
	// leaves a spawning tank idle, instead of granting an instant first reaction.
	if t.perceptionCount < 2 {
		t.perception[t.perceptionCount] = current
		t.perceptionCount++
		t.action = Action{}
		return
	}
	o := t.perception[0]
	t.perception[0], t.perception[1] = t.perception[1], current
	a := g.policy.Decide(o, &t.memory)
	if a.Move < 0 || a.Move > 4 {
		a = Action{}
	}
	if !o.Mask[a.Index()] {
		a.Fire = false
	}
	t.action = a
}

// ObservationFor returns the same current (pre-delay) feature encoding used by
// the policy pipeline. A training adapter must model the two-decision history
// exactly, rather than feeding instantaneous observations to a trained actor.
func (g *Game) ObservationFor(enemyID int) (Observation, bool) {
	g.refreshNavigation()
	for i := range g.enemies {
		if g.enemies[i].ID == enemyID {
			return g.observe(&g.enemies[i]), true
		}
	}
	return Observation{}, false
}

func (g *Game) observe(t *tankState) Observation {
	o := Observation{Version: ObservationVersion}
	v := &o.Values
	v[0] = float32(t.X / BoardSize)
	v[1] = float32(t.Y / BoardSize)
	v[2+t.Dir] = 1
	v[6] = float32(t.HP) / float32(max(1, t.MaxHP))
	v[7] = float32(math.Min(1, t.cooldown/2))
	v[8] = float32(t.action.Move) / 4
	if t.action.Fire {
		v[9] = 1
	}
	if t.Shield {
		v[10] = 1
	}
	v[11] = float32(math.Min(1, g.elapsed/180))
	cx, cy := int(t.X), int(t.Y)
	for dy := -3; dy <= 3; dy++ {
		for dx := -3; dx <= 3; dx++ {
			index := 12 + ((dy+3)*7+dx+3)*4
			tile := byte('#')
			x, y := cx+dx, cy+dy
			if inside(x, y) {
				tile = g.grid[y][x]
			}
			switch tile {
			case '+':
				v[index] = 1
			case '#':
				v[index+1] = 1
			case '~':
				v[index+2] = 1
			case 'g':
				v[index+3] = 1
			}
		}
	}
	visible := g.player.HP > 0
	// Grass conceals a stationary/far defender. A nearby defender or one standing
	// outside grass is visible. No observation reveals pending keyboard input.
	if visible && g.tileAt(g.player.X, g.player.Y) == 'g' && math.Hypot(g.player.X-t.X, g.player.Y-t.Y) > 3.5 {
		visible = false
	}
	if visible {
		v[208] = float32((g.player.X - t.X) / BoardSize)
		v[209] = float32((g.player.Y - t.Y) / BoardSize)
		v[210] = float32(g.playerVelocity[0] / PlayerSpeed)
		v[211] = float32(g.playerVelocity[1] / PlayerSpeed)
		v[212] = float32(g.player.HP)
		v[213+g.player.Dir] = 1
		v[217] = 1
		if g.player.Shield {
			v[218] = 1
		}
	}
	v[219] = float32((g.base.X - t.X) / BoardSize)
	v[220] = float32((g.base.Y - t.Y) / BoardSize)
	v[221] = float32(g.base.HP) / 3
	if g.base.HP > 0 {
		v[222] = 1
	}
	v[223] = float32(max(0, g.level.EnemyCount-g.kills)) / float32(max(1, g.level.EnemyCount))
	// Insertion-sort six relevant bullets by time to closest approach, weighted
	// by closest separation. Fixed arrays avoid per-decision heap allocation.
	var candidates [6]*projectile
	var threat [6]float64
	for i := range threat {
		threat[i] = math.Inf(1)
	}
	for i := range g.bullets {
		b := &g.bullets[i]
		if b.dead || b.Owner != "player" {
			continue
		}
		tc, sep := closestApproach(b, t.X, t.Y)
		score := tc + sep*2
		for at := range candidates {
			if score < threat[at] {
				for j := len(candidates) - 1; j > at; j-- {
					candidates[j], threat[j] = candidates[j-1], threat[j-1]
				}
				candidates[at], threat[at] = b, score
				break
			}
		}
	}
	for i, b := range candidates {
		if b != nil {
			at := 224 + i*7
			v[at] = float32((b.X - t.X) / BoardSize)
			v[at+1] = float32((b.Y - t.Y) / BoardSize)
			v[at+2] = float32(directionX[b.Dir] * b.speed / PlayerBulletSpeed)
			v[at+3] = float32(directionY[b.Dir] * b.speed / PlayerBulletSpeed)
			v[at+4] = 1
			tc, _ := closestApproach(b, t.X, t.Y)
			v[at+5] = float32(math.Min(1, tc/3))
			v[at+6] = 1
		}
	}
	var allies [4]*tankState
	var distances [4]float64
	for i := range distances {
		distances[i] = math.Inf(1)
	}
	for i := range g.enemies {
		a := &g.enemies[i]
		if a.ID == t.ID || a.HP <= 0 {
			continue
		}
		d := math.Hypot(a.X-t.X, a.Y-t.Y)
		for at := range allies {
			if d < distances[at] {
				for j := len(allies) - 1; j > at; j-- {
					allies[j], distances[j] = allies[j-1], distances[j-1]
				}
				allies[at], distances[at] = a, d
				break
			}
		}
	}
	for i, a := range allies {
		if a != nil {
			at := 266 + i*6
			v[at] = float32((a.X - t.X) / BoardSize)
			v[at+1] = float32((a.Y - t.Y) / BoardSize)
			v[at+2] = float32(a.Dir) / 3
			v[at+3] = float32(a.HP) / float32(a.MaxHP)
			v[at+4] = float32(a.role) / 3
			v[at+5] = 1
		}
	}
	for dir := 0; dir < 4; dir++ {
		x, y := cx+int(directionX[dir]), cy+int(directionY[dir])
		v[290+dir] = 1
		v[294+dir] = 1
		if inside(x, y) {
			if visible {
				v[290+dir] = pathFeature(g.navPlayer[y][x])
			}
			v[294+dir] = pathFeature(g.navBase[y][x])
			// Occupied lanes are costly, but not forbidden: teammates can move.
			if !g.canOccupy(t.ID, t.X+directionX[dir]*.45, t.Y+directionY[dir]*.45) {
				v[290+dir] = min(1, v[290+dir]+.2)
				v[294+dir] = min(1, v[294+dir]+.2)
			}
		}
		for _, b := range candidates {
			if b != nil {
				tc, sep := closestApproach(b, t.X+directionX[dir]*.45, t.Y+directionY[dir]*.45)
				if tc < .6 && sep < .5 {
					v[309+dir] = 1
				}
			}
		}
		v[313+dir] = g.aimFeature(t, dir, visible)
	}
	canFire := t.cooldown <= 0
	for _, b := range g.bullets {
		if !b.dead && b.sourceID == t.ID {
			canFire = false
			break
		}
	}
	for action := 0; action < ActionCount; action++ {
		o.Mask[action] = action%2 == 0 || canFire
		if o.Mask[action] {
			v[298+action] = 1
		}
	}
	v[308] = float32(t.role) / 3
	v[317] = float32(math.Min(1, t.stuckTime/2))
	v[318] = 1
	if allies[0] != nil {
		v[318] = float32(math.Min(1, distances[0]/BoardSize))
	}
	v[319] = float32(g.spawned) / float32(max(1, g.level.EnemyCount))
	return o
}

func closestApproach(b *projectile, x, y float64) (float64, float64) {
	vx, vy := directionX[b.Dir]*b.speed, directionY[b.Dir]*b.speed
	t := math.Max(0, ((x-b.X)*vx+(y-b.Y)*vy)/(b.speed*b.speed))
	return t, math.Hypot(b.X+vx*t-x, b.Y+vy*t-y)
}

func (g *Game) aimFeature(t *tankState, dir int, playerVisible bool) float32 {
	for d := .45; d < BoardSize; d += .12 {
		x, y := t.X+directionX[dir]*d, t.Y+directionY[dir]*d
		if x < 0 || y < 0 || x >= BoardSize || y >= BoardSize {
			return 0
		}
		switch g.grid[int(y)][int(x)] {
		case '#':
			return 0
		case '+':
			return .3
		}
		if playerVisible && overlaps(x, y, .04, g.player.X, g.player.Y, TankRadius) {
			if g.player.Shield {
				return .4
			}
			return 1
		}
		if g.base.HP > 0 && overlaps(x, y, .04, g.base.X, g.base.Y, .43) {
			return .9
		}
	}
	return 0
}

func (g *Game) tileAt(x, y float64) byte {
	if x < 0 || y < 0 || x >= BoardSize || y >= BoardSize {
		return '#'
	}
	return g.grid[int(y)][int(x)]
}
func inside(x, y int) bool { return x >= 0 && y >= 0 && x < BoardSize && y < BoardSize }
func pathFeature(cost float32) float32 {
	if cost >= 10000 {
		return 1
	}
	return min(.95, cost/128)
}

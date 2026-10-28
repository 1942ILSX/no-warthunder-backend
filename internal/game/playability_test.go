package game_test

import (
	"math"
	"testing"

	"BattleCityReforge/internal/game"
	"BattleCityReforge/internal/levels"
)

// legalDefender is intentionally a test-only scripted player. It consumes only
// the exact public State sent to a browser, and produces legal Input. It cannot
// mutate Game, inspect policy memory, remove enemies or alter scores/health.
// Finding wins with it catches unreachable goals, deadlocked controls and wave
// completion regressions without pretending to train a policy.
type legalDefender struct{ sequence uint64 }

func (bot *legalDefender) input(s game.State) game.Input {
	bot.sequence++
	in := game.Input{Seq: bot.sequence, Fire: true}
	p := s.Player
	if p.HP <= 0 {
		return in
	}
	// Shoot an aligned enemy before pursuing. Faster player shots can intercept
	// enemy fire head-on. This basic test player has no predictive dodge skill.
	for _, e := range s.Enemies {
		if dir, ok := firingDirection(s, p.X, p.Y, e.X, e.Y); ok {
			if p.Dir != dir {
				in.Move = dir + 1
			}
			return in
		}
	}
	// BFS all walkable cells, keeping the predecessor for the first action.
	const n = 24
	var distance [n * n]int
	var previous [n * n]int
	for i := range distance {
		distance[i] = -1
		previous[i] = -1
	}
	start := int(p.Y)*n + int(p.X)
	distance[start] = 0
	queue := make([]int, 1, n*n)
	queue[0] = start
	dxs := [4]int{0, 1, 0, -1}
	dys := [4]int{-1, 0, 1, 0}
	for head := 0; head < len(queue); head++ {
		cell := queue[head]
		x, y := cell%n, cell/n
		for d := 0; d < 4; d++ {
			nx, ny := x+dxs[d], y+dys[d]
			if nx < 1 || ny < 1 || nx >= n-1 || ny >= n-1 {
				continue
			}
			next := ny*n + nx
			if distance[next] >= 0 || !walkable(s, nx, ny) {
				continue
			}
			distance[next] = distance[cell] + 1
			previous[next] = cell
			queue = append(queue, next)
		}
	}
	best := -1
	bestScore := math.Inf(1)
	for _, e := range s.Enemies {
		for _, cell := range queue {
			x, y := float64(cell%n)+.5, float64(cell/n)+.5
			if _, ok := firingDirection(s, x, y, e.X, e.Y); !ok {
				continue
			}
			rangeToEnemy := math.Hypot(x-e.X, y-e.Y)
			if rangeToEnemy < 1.2 || rangeToEnemy > 12 {
				continue
			}
			score := float64(distance[cell]) + .15*rangeToEnemy + .1*math.Hypot(e.X-s.Base.X, e.Y-s.Base.Y)
			if score < bestScore {
				best = cell
				bestScore = score
			}
		}
	}
	if best < 0 {
		// No enemy firing lane yet: head into the arena while firing through brick.
		tx, ty := int(s.Base.X), 15
		for _, cell := range queue {
			score := math.Hypot(float64(cell%n-tx), float64(cell/n-ty)) + .04*float64(distance[cell])
			if score < bestScore {
				best = cell
				bestScore = score
			}
		}
	}
	if best < 0 {
		return in
	}
	if best == start {
		// Re-centre to make the anticipated firing line real.
		dx, dy := float64(best%n)+.5-p.X, float64(best/n)+.5-p.Y
		if math.Abs(dx) > .12 {
			if dx > 0 {
				in.Move = 2
			} else {
				in.Move = 4
			}
		} else if math.Abs(dy) > .12 {
			if dy > 0 {
				in.Move = 3
			} else {
				in.Move = 1
			}
		}
		return in
	}
	for previous[best] != start && previous[best] >= 0 {
		best = previous[best]
	}
	dx, dy := best%n-start%n, best/n-start/n
	if dx > 0 {
		in.Move = 2
	} else if dx < 0 {
		in.Move = 4
	} else if dy > 0 {
		in.Move = 3
	} else {
		in.Move = 1
	}
	return in
}

func walkable(s game.State, x, y int) bool {
	tile := s.Grid[y][x]
	if tile != '.' && tile != 'g' {
		return false
	}
	if math.Abs(float64(x)+.5-s.Base.X) < .8 && math.Abs(float64(y)+.5-s.Base.Y) < .8 {
		return false
	}
	for _, e := range s.Enemies {
		if math.Abs(float64(x)+.5-e.X) < .7 && math.Abs(float64(y)+.5-e.Y) < .7 {
			return false
		}
	}
	return true
}

func firingDirection(s game.State, x, y, tx, ty float64) (int, bool) {
	dx, dy := tx-x, ty-y
	dir := -1
	if math.Abs(dx) < .34 {
		if dy < 0 {
			dir = 0
		} else {
			dir = 2
		}
	} else if math.Abs(dy) < .34 {
		if dx < 0 {
			dir = 3
		} else {
			dir = 1
		}
	}
	if dir < 0 {
		return 0, false
	}
	dxs := [4]float64{0, 1, 0, -1}
	dys := [4]float64{-1, 0, 1, 0}
	length := math.Max(math.Abs(dx), math.Abs(dy))
	for distance := .45; distance < length-.25; distance += .15 {
		xx, yy := int(x+dxs[dir]*distance), int(y+dys[dir]*distance)
		if xx < 0 || yy < 0 || xx >= 24 || yy >= 24 {
			return 0, false
		}
		tile := s.Grid[yy][xx]
		if tile == '#' || tile == '+' {
			return 0, false
		}
	}
	return dir, true
}

func TestLegalPublicInputCanWinOpeningLevels(t *testing.T) {
	for _, levelID := range []int{1, 2} {
		level, _ := levels.Get(levelID)
		wins := 0
		for seed := int64(1); seed <= 5; seed++ {
			g := game.New(level, seed)
			bot := legalDefender{}
			var state game.State
			for frame := 0; frame < 30*180; frame++ {
				if frame%3 == 0 {
					state = g.Snapshot()
					if state.Status != "playing" {
						break
					}
					g.SetInput(bot.input(state))
				}
				g.Step(1.0 / 30)
			}
			state = g.Snapshot()
			if state.Status == "won" {
				wins++
			}
			t.Logf("level=%d seed=%d status=%s kills=%d/%d duration=%dms score=%d lives=%d base=%d", levelID, seed, state.Status, state.Kills, state.TotalEnemies, state.TimeMS, state.Score, state.Lives, state.Base.HP)
		}
		if wins == 0 {
			t.Errorf("public-input defender could not win level%d", levelID)
		}
	}
}

package training

import (
	"BattleCityReforge/internal/game"
	"math"
)

type defenderBot struct {
	kind      string
	random    uint64
	decisions uint64
	move      int
}

func newDefenderBot(kind string, seed int64) *defenderBot {
	if kind == "mixed" {
		kind = []string{"guard", "hunter", "wander"}[uint64(seed)%3]
	}
	return &defenderBot{kind: kind, random: uint64(seed) ^ 0x9e3779b97f4a7c15}
}
func (b *defenderBot) decide(s game.State) game.Action {
	b.decisions++
	if s.Player.HP <= 0 {
		return game.Action{}
	}
	if b.kind == "wander" {
		if b.decisions%8 == 1 {
			b.random ^= b.random << 13
			b.random ^= b.random >> 7
			b.random ^= b.random << 17
			b.move = int(b.random % 5)
		}
		return game.Action{Move: b.move, Fire: true}
	}
	p := s.Player
	for _, enemy := range s.Enemies {
		if dir, ok := firingDirection(s, p.X, p.Y, enemy.X, enemy.Y); ok {
			move := 0
			if p.Dir != dir {
				move = dir + 1
			}
			return game.Action{Move: move, Fire: true}
		}
	}
	const n = game.BoardSize
	var distance, previous [n * n]int
	for i := range distance {
		distance[i], previous[i] = -1, -1
	}
	start := int(p.Y)*n + int(p.X)
	var queue [n * n]int
	queue[0] = start
	tail := 1
	distance[start] = 0
	dx, dy := [4]int{0, 1, 0, -1}, [4]int{-1, 0, 1, 0}
	for head := 0; head < tail; head++ {
		cell := queue[head]
		x, y := cell%n, cell/n
		for dir := 0; dir < 4; dir++ {
			nx, ny := x+dx[dir], y+dy[dir]
			if nx < 1 || nx >= n-1 || ny < 1 || ny >= n-1 {
				continue
			}
			next := ny*n + nx
			if distance[next] >= 0 || !botWalkable(s, nx, ny) {
				continue
			}
			distance[next] = distance[cell] + 1
			previous[next] = cell
			queue[tail] = next
			tail++
		}
	}
	best, bestScore := -1, math.Inf(1)
	for _, enemy := range s.Enemies {
		for _, cell := range queue[:tail] {
			x, y := float64(cell%n)+.5, float64(cell/n)+.5
			if _, ok := firingDirection(s, x, y, enemy.X, enemy.Y); !ok {
				continue
			}
			rangeTo := math.Hypot(x-enemy.X, y-enemy.Y)
			if rangeTo < 1.2 || rangeTo > 12 {
				continue
			}
			score := float64(distance[cell]) + .15*rangeTo
			if b.kind == "guard" {
				score += .45*math.Hypot(enemy.X-s.Base.X, enemy.Y-s.Base.Y) + .3*math.Max(0, math.Hypot(x-s.Base.X, y-s.Base.Y)-6)
			}
			if score < bestScore {
				best, bestScore = cell, score
			}
		}
	}
	if best < 0 {
		for _, cell := range queue[:tail] {
			x, y := float64(cell%n)+.5, float64(cell/n)+.5
			score := math.Hypot(x-s.Base.X, y-(s.Base.Y-4)) + .04*float64(distance[cell])
			if score < bestScore {
				best, bestScore = cell, score
			}
		}
	}
	a := game.Action{Fire: true}
	if best < 0 {
		return a
	}
	if best == start {
		x, y := float64(best%n)+.5-p.X, float64(best/n)+.5-p.Y
		if math.Abs(x) > .12 {
			if x > 0 {
				a.Move = 2
			} else {
				a.Move = 4
			}
		} else if math.Abs(y) > .12 {
			if y > 0 {
				a.Move = 3
			} else {
				a.Move = 1
			}
		}
		return a
	}
	for previous[best] != start && previous[best] >= 0 {
		best = previous[best]
	}
	x, y := best%n-start%n, best/n-start/n
	if x > 0 {
		a.Move = 2
	} else if x < 0 {
		a.Move = 4
	} else if y > 0 {
		a.Move = 3
	} else {
		a.Move = 1
	}
	return a
}
func botWalkable(s game.State, x, y int) bool {
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
	vx, vy := [4]float64{0, 1, 0, -1}, [4]float64{-1, 0, 1, 0}
	for d := .45; d < math.Max(math.Abs(dx), math.Abs(dy)); d += .1 {
		cx, cy := int(x+vx[dir]*d), int(y+vy[dir]*d)
		if cx < 0 || cy < 0 || cx >= game.BoardSize || cy >= game.BoardSize {
			return 0, false
		}
		tile := s.Grid[cy][cx]
		if tile == '#' || tile == '+' {
			return 0, false
		}
	}
	return dir, true
}

package main

import (
	"BattleCityReforge/internal/game"
	"math"
)

// patrolDefender is a cheap deterministic traffic/gameplay driver. It reads only
// public snapshots, turns toward clear firing lines and seeks nearby enemies.
// Its CPU is included in this standalone probe, although real clients supply
// their own input. This is deliberately not a skill/calibration opponent.
type patrolDefender struct {
	phase     int
	decisions int
}

var dirX = [4]float64{0, 1, 0, -1}
var dirY = [4]float64{-1, 0, 1, 0}

func (b *patrolDefender) action(s game.State) game.Action {
	b.decisions++
	p := s.Player
	if p.HP <= 0 {
		return game.Action{}
	}
	targetX, targetY := s.Base.X, math.Max(2.5, s.Base.Y-7)
	nearest := math.Inf(1)
	for _, enemy := range s.Enemies {
		dx, dy := enemy.X-p.X, enemy.Y-p.Y
		dir := -1
		if math.Abs(dx) < .3 {
			if dy < 0 {
				dir = 0
			} else {
				dir = 2
			}
		} else if math.Abs(dy) < .3 {
			if dx < 0 {
				dir = 3
			} else {
				dir = 1
			}
		}
		if dir >= 0 && clearShot(s, p.X, p.Y, dir, math.Max(math.Abs(dx), math.Abs(dy))) {
			move := 0
			if p.Dir != dir {
				move = dir + 1
			}
			return game.Action{Move: move, Fire: true}
		}
		if distance := math.Abs(dx) + math.Abs(dy); distance < nearest {
			nearest = distance
			targetX, targetY = enemy.X, enemy.Y
		}
	}
	best, bestCost := -1, math.Inf(1)
	for turn := 0; turn < 4; turn++ {
		dir := (turn + b.phase + (b.decisions / 90)) % 4
		x, y := p.X+dirX[dir]*.4, p.Y+dirY[dir]*.4
		if !walkable(s, x, y) {
			continue
		}
		cost := math.Abs(x-targetX) + math.Abs(y-targetY)
		if dir == p.Dir {
			cost -= .08
		}
		for _, bullet := range s.Bullets {
			if bullet.Owner == "enemy" && math.Hypot(x-bullet.X, y-bullet.Y) < 1.1 {
				cost += 2
			}
		}
		if cost < bestCost {
			best, bestCost = dir, cost
		}
	}
	if best < 0 {
		best = (b.decisions/12 + b.phase) % 4
	}
	return game.Action{Move: best + 1, Fire: true}
}
func walkable(s game.State, x, y float64) bool {
	r := game.TankRadius
	if x-r < 1 || y-r < 1 || x+r >= 23 || y+r >= 23 {
		return false
	}
	for cy := int(y - r); cy <= int(y+r); cy++ {
		for cx := int(x - r); cx <= int(x+r); cx++ {
			tile := s.Grid[cy][cx]
			if tile == '#' || tile == '+' || tile == '~' {
				return false
			}
		}
	}
	if math.Abs(x-s.Base.X) < .8 && math.Abs(y-s.Base.Y) < .8 {
		return false
	}
	for _, enemy := range s.Enemies {
		if math.Abs(x-enemy.X) < .72 && math.Abs(y-enemy.Y) < .72 {
			return false
		}
	}
	return true
}
func clearShot(s game.State, x, y float64, dir int, distance float64) bool {
	for d := .45; d < distance; d += .25 {
		cx, cy := int(x+dirX[dir]*d), int(y+dirY[dir]*d)
		if cx < 0 || cy < 0 || cx >= 24 || cy >= 24 {
			return false
		}
		if tile := s.Grid[cy][cx]; tile == '#' || tile == '+' {
			return false
		}
	}
	return true
}

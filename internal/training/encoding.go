package training

import (
	"math"
	"sort"

	"BattleCityReforge/internal/game"
	"BattleCityReforge/internal/levels"
)

var directionX = [4]float64{0, 1, 0, -1}
var directionY = [4]float64{-1, 0, 1, 0}

func unit(v float64) float32 { return float32(max(-1, min(1, v))) }
func flag(v bool) float32 {
	if v {
		return 1
	}
	return 0
}
func kindValue(kind string) float32 {
	if kind == "heavy" {
		return 1
	}
	if kind == "striker" {
		return .5
	}
	return 0
}
func terrain(v []float32, tile byte) {
	switch tile {
	case '+':
		v[0] = 1
	case '#':
		v[1] = 1
	case '~':
		v[2] = 1
	case 'g':
		v[3] = 1
	}
}
func indexedEnemies(frame game.DecisionFrame) [game.DecisionSlots]*game.Tank {
	var slots [game.DecisionSlots]*game.Tank
	for slot, id := range frame.EnemyIDs {
		for i := range frame.State.Enemies {
			if id != 0 && frame.State.Enemies[i].ID == id {
				slots[slot] = &frame.State.Enemies[i]
				break
			}
		}
	}
	return slots
}

func DefenderObservation(t Transition) ([game.ObservationSize]float32, [game.ActionCount]bool, bool) {
	var v [game.ObservationSize]float32
	var mask [game.ActionCount]bool
	f, s := t.Frame, t.Frame.State
	p := s.Player
	v[0], v[1] = unit(p.X/24), unit(p.Y/24)
	v[2+p.Dir] = 1
	v[6] = float32(p.HP)
	v[7] = flag(f.CanFire)
	v[8], v[9] = float32(f.Input.Move)/4, flag(f.Input.Fire)
	v[10], v[11] = flag(p.Shield), unit(float64(s.TimeMS)/900000)
	cx, cy := int(p.X), int(p.Y)
	for dy := -3; dy <= 3; dy++ {
		for dx := -3; dx <= 3; dx++ {
			x, y := cx+dx, cy+dy
			tile := byte('#')
			if x >= 0 && y >= 0 && x < 24 && y < 24 {
				tile = s.Grid[y][x]
			}
			at := 12 + ((dy+3)*7+dx+3)*4
			terrain(v[at:at+4], tile)
		}
	}
	nearestX, nearestY := -1, -1
	nearest := math.Inf(1)
	for slot, e := range indexedEnemies(f) {
		if e == nil {
			continue
		}
		at := 208 + slot*10
		v[at], v[at+1] = unit((e.X-p.X)/24), unit((e.Y-p.Y)/24)
		v[at+2+e.Dir] = 1
		v[at+6] = float32(e.HP) / float32(max(1, e.MaxHP))
		v[at+7] = flag(e.Shield)
		v[at+8] = kindValue(e.Kind)
		v[at+9] = 1
		d := math.Hypot(e.X-p.X, e.Y-p.Y)
		if d < nearest {
			nearest, nearestX, nearestY = d, int(e.X), int(e.Y)
		}
	}
	bullets := make([]game.Bullet, 0, len(s.Bullets))
	for _, b := range s.Bullets {
		if b.Owner == "enemy" {
			bullets = append(bullets, b)
		}
	}
	sort.Slice(bullets, func(i, j int) bool {
		a, b := bullets[i], bullets[j]
		da, db := (a.X-p.X)*(a.X-p.X)+(a.Y-p.Y)*(a.Y-p.Y), (b.X-p.X)*(b.X-p.X)+(b.Y-p.Y)*(b.Y-p.Y)
		if da == db {
			return a.ID < b.ID
		}
		return da < db
	})
	for i, b := range bullets[:min(6, len(bullets))] {
		at := 258 + i*7
		v[at], v[at+1] = unit((b.X-p.X)/24), unit((b.Y-p.Y)/24)
		v[at+2], v[at+3] = unit(directionX[b.Dir]*game.EnemyBulletSpeed/10), unit(directionY[b.Dir]*game.EnemyBulletSpeed/10)
		tc, _ := bulletApproach(b, p.X, p.Y)
		v[at+4], v[at+5], v[at+6] = 1, unit(tc/3), 1
		for dir := 0; dir < 4; dir++ {
			tc, sep := bulletApproach(b, p.X+directionX[dir]*.45, p.Y+directionY[dir]*.45)
			if tc < .6 && sep < .5 {
				v[315+dir] = 1
			}
		}
	}
	v[300], v[301] = unit((s.Base.X-p.X)/24), unit((s.Base.Y-p.Y)/24)
	v[302], v[303] = float32(s.Base.HP)/3, flag(s.Base.HP > 0)
	v[304] = float32(s.Lives) / 3
	v[305] = float32(s.EnemiesRemaining) / float32(max(1, s.TotalEnemies))
	v[306] = float32(f.Spawned) / float32(max(1, s.TotalEnemies))
	baseNav := navigation(s.Grid, int(s.Base.X), int(s.Base.Y))
	enemyNav := navigation(s.Grid, nearestX, nearestY)
	for dir := 0; dir < 4; dir++ {
		x, y := cx+int(directionX[dir]), cy+int(directionY[dir])
		v[307+dir], v[311+dir] = 1, 1
		if x >= 0 && y >= 0 && x < 24 && y < 24 {
			v[307+dir] = baseNav[y*24+x]
			v[311+dir] = enemyNav[y*24+x]
		}
	}
	v[319] = max(0, 1-v[11])
	active := !t.Done() && p.HP > 0
	if !active {
		mask[0] = true
	} else {
		for a := range mask {
			mask[a] = a%2 == 0 || f.CanFire
		}
	}
	return v, mask, active
}

func bulletApproach(b game.Bullet, x, y float64) (float64, float64) {
	vx, vy := directionX[b.Dir]*game.EnemyBulletSpeed, directionY[b.Dir]*game.EnemyBulletSpeed
	t := max(0, ((x-b.X)*vx+(y-b.Y)*vy)/(game.EnemyBulletSpeed*game.EnemyBulletSpeed))
	return t, math.Hypot(b.X+vx*t-x, b.Y+vy*t-y)
}
func navigation(grid []string, tx, ty int) [24 * 24]float32 {
	var cost [24 * 24]float32
	for i := range cost {
		cost[i] = 1
	}
	if tx < 0 || ty < 0 || tx >= 24 || ty >= 24 {
		return cost
	}
	var queue [24 * 24]int
	root := ty*24 + tx
	queue[0] = root
	cost[root] = 0
	tail := 1
	for head := 0; head < tail; head++ {
		cell := queue[head]
		x, y := cell%24, cell/24
		for dir := 0; dir < 4; dir++ {
			nx, ny := x+int(directionX[dir]), y+int(directionY[dir])
			if nx < 0 || ny < 0 || nx >= 24 || ny >= 24 {
				continue
			}
			next := ny*24 + nx
			tile := grid[ny][nx]
			if cost[next] != 1 || tile == '#' || tile == '~' || tile == '+' {
				continue
			}
			cost[next] = min(.95, cost[cell]+1.0/128)
			queue[tail] = next
			tail++
		}
	}
	return cost
}

func CriticObservation(t Transition) [CriticSize]float32 {
	var v [CriticSize]float32
	f, s := t.Frame, t.Frame.State
	for y, row := range s.Grid {
		for x := range row {
			at := (y*24 + x) * 4
			terrain(v[at:at+4], row[x])
		}
	}
	at := 2304
	p := s.Player
	v[at], v[at+1] = unit(p.X/24), unit(p.Y/24)
	v[at+2+p.Dir] = 1
	v[at+6] = float32(p.HP)
	v[at+7] = flag(p.Shield)
	v[at+8], v[at+9], v[at+10] = flag(f.CanFire), float32(f.Input.Move)/4, flag(f.Input.Fire)
	v[at+11], v[at+12] = float32(s.Lives)/3, unit(float64(s.TimeMS)/900000)
	v[2320], v[2321], v[2322], v[2323] = unit(s.Base.X/24), unit(s.Base.Y/24), float32(s.Base.HP)/3, flag(s.Base.HP > 0)
	for slot, e := range indexedEnemies(f) {
		if e == nil {
			continue
		}
		at = 2328 + slot*16
		v[at], v[at+1] = unit(e.X/24), unit(e.Y/24)
		v[at+2+e.Dir] = 1
		v[at+6] = float32(e.HP) / float32(max(1, e.MaxHP))
		v[at+7] = flag(e.Shield)
		v[at+8], v[at+9], v[at+10] = kindValue(e.Kind), 1, flag(t.CriticActive[slot])
	}
	bullets := append([]game.Bullet(nil), s.Bullets...)
	sort.Slice(bullets, func(i, j int) bool { return bullets[i].ID < bullets[j].ID })
	for i, b := range bullets[:min(32, len(bullets))] {
		at = 2408 + i*8
		v[at], v[at+1] = unit(b.X/24), unit(b.Y/24)
		v[at+2+b.Dir] = 1
		v[at+6], v[at+7] = flag(b.Owner == "enemy"), 1
	}
	level, _ := levels.Get(t.Info.LevelID)
	total := float32(max(1, s.TotalEnemies))
	at = 2664
	v[at], v[at+1], v[at+2] = float32(t.Info.LevelID)/10, float32(level.Difficulty)/5, float32(s.TotalEnemies)/32
	v[at+3], v[at+4], v[at+5] = float32(f.Spawned)/total, float32(s.EnemiesRemaining)/total, float32(s.Kills)/total
	v[at+6], v[at+7], v[at+8] = float32(len(s.Enemies))/5, float32(s.Lives)/3, float32(s.Base.HP)/3
	v[at+9] = unit(float64(s.TimeMS) / 900000)
	v[at+10] = max(0, 1-v[at+9])
	v[at+11] = flag(t.Terminated)
	return v
}

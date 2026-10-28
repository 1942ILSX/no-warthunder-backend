// Package levels contains the ten original, hand-authored Reforged arenas.
// There is no runtime random map generation: a level ID always identifies the
// same layout, enemy budget, and spawn positions so leaderboard runs compare
// like with like. Returned slices are copies and may safely be modified by rooms.
package levels

import (
	"fmt"
	"math"
)

const Size = 24

type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type Level struct {
	ID          int      `json:"id"`
	Name        string   `json:"name"`
	Subtitle    string   `json:"subtitle"`
	Description string   `json:"description"`
	Difficulty  int      `json:"difficulty"`
	Theme       string   `json:"theme"`
	Grid        []string `json:"grid"`
	PlayerSpawn Point    `json:"playerSpawn"`
	Base        Point    `json:"base"`
	EnemySpawns []Point  `json:"enemySpawns"`
	EnemyCount  int      `json:"enemyCount"`
	MaxActive   int      `json:"maxActive"`
}

var catalog = makeCatalog()

// All returns catalog entries in display order. Both slices within every level
// are copied so callers cannot accidentally change future rooms or API results.
func All() []Level {
	result := make([]Level, len(catalog))
	for i, level := range catalog {
		result[i] = clone(level)
	}
	return result
}

func Get(id int) (Level, bool) {
	if id < 1 || id > len(catalog) {
		return Level{}, false
	}
	return clone(catalog[id-1]), true
}

func clone(level Level) Level {
	level.Grid = append([]string(nil), level.Grid...)
	level.EnemySpawns = append([]Point(nil), level.EnemySpawns...)
	return level
}

// Validate checks the assumptions the simulation and future RL observation
// encoder rely on. In particular, every enemy can reach both objectives without
// breaking a brick. Brick routes add choices; they never make a room unwinnable
// when a conservative policy has not yet learned demolition.
func Validate(level Level) error {
	if level.ID < 1 || level.ID > 10 {
		return fmt.Errorf("level ID must be between 1 and 10")
	}
	if level.Name == "" || level.Subtitle == "" || level.Description == "" {
		return fmt.Errorf("level %d has incomplete display metadata", level.ID)
	}
	if level.Difficulty < 1 || level.Difficulty > 5 {
		return fmt.Errorf("level %d has invalid difficulty", level.ID)
	}
	if len(level.Theme) != 7 || level.Theme[0] != '#' {
		return fmt.Errorf("level %d has invalid theme", level.ID)
	}
	for _, c := range level.Theme[1:] {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return fmt.Errorf("level %d has invalid theme", level.ID)
		}
	}
	if level.EnemyCount < 1 || level.EnemyCount > 32 || level.MaxActive < 1 || level.MaxActive > 5 || level.MaxActive > level.EnemyCount {
		return fmt.Errorf("level %d has invalid wave limits", level.ID)
	}
	if len(level.Grid) != Size {
		return fmt.Errorf("level %d must have %d rows", level.ID, Size)
	}
	for y, row := range level.Grid {
		if len(row) != Size {
			return fmt.Errorf("level %d row %d must have %d cells", level.ID, y, Size)
		}
		for x := range row {
			if !validTile(row[x]) {
				return fmt.Errorf("level %d cell (%d,%d) has invalid tile", level.ID, x, y)
			}
			if (x == 0 || y == 0 || x == Size-1 || y == Size-1) && row[x] != '#' {
				return fmt.Errorf("level %d perimeter must be steel", level.ID)
			}
		}
	}
	if len(level.EnemySpawns) == 0 || len(level.EnemySpawns) > 5 {
		return fmt.Errorf("level %d needs 1–5 enemy spawn points", level.ID)
	}
	points := append([]Point{level.PlayerSpawn, level.Base}, level.EnemySpawns...)
	seen := make(map[Point]bool, len(points))
	for i, point := range points {
		if !finite(point.X) || !finite(point.Y) || point.X < 1.5 || point.Y < 1.5 || point.X > Size-1.5 || point.Y > Size-1.5 {
			return fmt.Errorf("level %d point %d is out of bounds", level.ID, i)
		}
		if math.Mod(point.X, 1) != .5 || math.Mod(point.Y, 1) != .5 {
			return fmt.Errorf("level %d point %d must be a cell centre", level.ID, i)
		}
		if seen[point] {
			return fmt.Errorf("level %d has overlapping spawn/objective points", level.ID)
		}
		seen[point] = true
		if level.Grid[int(point.Y)][int(point.X)] != '.' {
			return fmt.Errorf("level %d point %d must be on clear floor", level.ID, i)
		}
		// Every tank spawn has a full 3×3 floor apron. The base is an objective,
		// not a tank spawn, and therefore needs only its own clear floor cell.
		if i != 1 {
			for y := int(point.Y) - 1; y <= int(point.Y)+1; y++ {
				for x := int(point.X) - 1; x <= int(point.X)+1; x++ {
					if x <= 0 || y <= 0 || x >= Size-1 || y >= Size-1 || level.Grid[y][x] != '.' {
						return fmt.Errorf("level %d point %d needs a 3×3 floor apron", level.ID, i)
					}
				}
			}
		}
	}
	reachable := flood(level.Grid, level.PlayerSpawn)
	for _, point := range append([]Point{level.Base}, level.EnemySpawns...) {
		if !reachable[int(point.Y)][int(point.X)] {
			return fmt.Errorf("level %d has an objective/spawn disconnected by permanent obstacles or bricks", level.ID)
		}
	}
	return nil
}

func validTile(tile byte) bool {
	return tile == '.' || tile == '#' || tile == '+' || tile == '~' || tile == 'g'
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func flood(grid []string, start Point) [Size][Size]bool {
	var visited [Size][Size]bool
	type cell struct{ x, y int }
	queue := []cell{{int(start.X), int(start.Y)}}
	visited[int(start.Y)][int(start.X)] = true
	for head := 0; head < len(queue); head++ {
		current := queue[head]
		for _, delta := range [...]cell{{0, -1}, {1, 0}, {0, 1}, {-1, 0}} {
			x, y := current.x+delta.x, current.y+delta.y
			if x < 0 || y < 0 || x >= Size || y >= Size || visited[y][x] {
				continue
			}
			if tile := grid[y][x]; tile == '.' || tile == 'g' {
				visited[y][x] = true
				queue = append(queue, cell{x, y})
			}
		}
	}
	return visited
}

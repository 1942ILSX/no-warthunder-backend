package levels

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestCatalogHasTenValidDistinctArenas(t *testing.T) {
	all := All()
	if len(all) != 10 {
		t.Fatalf("got %d levels, want exactly 10", len(all))
	}
	seenLayouts := map[string]bool{}
	seenNames := map[string]bool{}
	for i, level := range all {
		if level.ID != i+1 {
			t.Errorf("level at %d has ID %d", i, level.ID)
		}
		if err := Validate(level); err != nil {
			t.Errorf("level %d: %v", level.ID, err)
		}
		key := strings.Join(level.Grid, "\n")
		if seenLayouts[key] || seenNames[level.Name] {
			t.Errorf("level %d duplicates another arena", level.ID)
		}
		seenLayouts[key], seenNames[level.Name] = true, true
		if i > 0 && level.Difficulty < all[i-1].Difficulty {
			t.Errorf("level %d difficulty goes backwards", level.ID)
		}
		if level.EnemyCount < 6 || level.EnemyCount > 16 || level.MaxActive > 4 {
			t.Errorf("level %d exceeds the intended first-release combat budget", level.ID)
		}
	}
}

func TestCatalogIsIsolatedFromRoomMutations(t *testing.T) {
	original, _ := Get(1)
	changed, _ := Get(1)
	changed.Grid[3] = strings.Repeat("+", Size)
	changed.EnemySpawns[0].X = 99
	all := All()
	all[0].Grid[4] = strings.Repeat("~", Size)
	all[0].EnemySpawns[1].Y = 99
	all[0].Name = "changed"
	fresh, _ := Get(1)
	if strings.Join(fresh.Grid, "") != strings.Join(original.Grid, "") || fresh.Name != original.Name {
		t.Fatal("catalog was changed through a returned level")
	}
	for i, point := range original.EnemySpawns {
		if fresh.EnemySpawns[i] != point {
			t.Fatal("catalog spawn points were changed through returned slices")
		}
	}
}

func TestGetRejectsUnknownIDs(t *testing.T) {
	for _, id := range []int{-100, -1, 0, 11, 100} {
		if _, ok := Get(id); ok {
			t.Errorf("Get(%d) unexpectedly succeeded", id)
		}
	}
}

func TestEverySpawnCanReachBothObjectivesWithoutDemolition(t *testing.T) {
	for _, level := range All() {
		for _, spawn := range level.EnemySpawns {
			reachable := flood(level.Grid, spawn)
			for _, target := range []Point{level.PlayerSpawn, level.Base} {
				if !reachable[int(target.Y)][int(target.X)] {
					t.Errorf("level %d spawn %+v cannot reach target %+v", level.ID, spawn, target)
				}
			}
		}
	}
}

func TestValidateRejectsBrokenLevelContracts(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Level)
	}{
		{"unknown ID", func(l *Level) { l.ID = 11 }},
		{"missing description", func(l *Level) { l.Description = "" }},
		{"difficulty", func(l *Level) { l.Difficulty = 0 }},
		{"theme", func(l *Level) { l.Theme = "#zzzzzz" }},
		{"wave budget", func(l *Level) { l.MaxActive = l.EnemyCount + 1 }},
		{"missing row", func(l *Level) { l.Grid = l.Grid[:Size-1] }},
		{"short row", func(l *Level) { l.Grid[5] = "..." }},
		{"unknown tile", func(l *Level) { replaceTile(l, 3, 5, 'x') }},
		{"open perimeter", func(l *Level) { replaceTile(l, 0, 5, '.') }},
		{"no enemy spawns", func(l *Level) { l.EnemySpawns = nil }},
		{"NaN spawn", func(l *Level) { l.PlayerSpawn.X = math.NaN() }},
		{"infinite spawn", func(l *Level) { l.PlayerSpawn.Y = math.Inf(1) }},
		{"outside spawn", func(l *Level) { l.PlayerSpawn.X = -1.5 }},
		{"uncentred spawn", func(l *Level) { l.PlayerSpawn.X = 8.2 }},
		{"overlapping base", func(l *Level) { l.Base = l.PlayerSpawn }},
		{"blocked spawn", func(l *Level) { replaceTile(l, int(l.PlayerSpawn.X), int(l.PlayerSpawn.Y), '#') }},
		{"tight spawn apron", func(l *Level) { replaceTile(l, int(l.EnemySpawns[0].X)+1, int(l.EnemySpawns[0].Y), '+') }},
		{"disconnected objectives", func(l *Level) { l.Grid[19] = strings.Repeat("#", Size) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			level, _ := Get(1)
			tc.edit(&level)
			if err := Validate(level); err == nil {
				t.Fatal("invalid level unexpectedly passed validation")
			}
		})
	}
}

func TestJSONUsesProtocolFieldNames(t *testing.T) {
	level, _ := Get(1)
	data, err := json.Marshal(level)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "name", "subtitle", "description", "difficulty", "theme", "grid", "playerSpawn", "base", "enemySpawns", "enemyCount", "maxActive"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("missing protocol field %q", key)
		}
	}
	if string(fields["playerSpawn"]) != `{"x":8.5,"y":21.5}` {
		t.Errorf("unexpected coordinate encoding: %s", fields["playerSpawn"])
	}
}

func replaceTile(level *Level, x, y int, tile byte) {
	row := []byte(level.Grid[y])
	row[x] = tile
	level.Grid[y] = string(row)
}

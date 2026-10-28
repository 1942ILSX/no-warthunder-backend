package game

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"BattleCityReforge/internal/levels"
)

func fixture() levels.Level {
	grid := make([]string, BoardSize)
	for y := range grid {
		if y == 0 || y == BoardSize-1 {
			grid[y] = strings.Repeat("#", BoardSize)
		} else {
			grid[y] = "#" + strings.Repeat(".", BoardSize-2) + "#"
		}
	}
	return levels.Level{ID: 1, Name: "Test arena", Difficulty: 1, Grid: grid, PlayerSpawn: levels.Point{X: 12.5, Y: 20.5},
		Base: levels.Point{X: 12.5, Y: 22.5}, EnemySpawns: []levels.Point{{X: 2.5, Y: 2.5}}, EnemyCount: 1, MaxActive: 1}
}

func quietGame() *Game { g := New(fixture(), 42); g.spawnTimer = 1e9; g.decisionTimer = 1e9; return g }
func advance(g *Game, frames int) {
	for range frames {
		g.Step(1.0 / SimulationHz)
	}
}

func TestPlayerMovementCollidesWithSteelBrickAndWater(t *testing.T) {
	for _, tile := range []byte{'#', '+', '~'} {
		t.Run(string(tile), func(t *testing.T) {
			g := quietGame()
			g.grid[19][12] = tile
			g.SetInput(Input{Seq: 1, Move: 1})
			advance(g, 45)
			if g.player.Y < 20+TankRadius-1e-6 {
				t.Fatalf("tank crossed %c at y=%v", tile, g.player.Y)
			}
			if g.player.Dir != 0 {
				t.Fatal("blocked movement must still face north")
			}
		})
	}
}

func TestPlayerMovementGrassAndTankCollision(t *testing.T) {
	g := quietGame()
	g.grid[19][12] = 'g'
	g.SetInput(Input{Seq: 1, Move: 1})
	advance(g, 15)
	if g.player.Y >= 19.5 {
		t.Fatalf("grass should be passable, y=%v", g.player.Y)
	}
	g = quietGame()
	g.enemies = append(g.enemies, tankState{Tank: Tank{ID: 2, X: 12.5, Y: 18.5, HP: 1, MaxHP: 1, Kind: "scout"}})
	g.SetInput(Input{Seq: 1, Move: 1})
	advance(g, 30)
	if g.player.Y < 18.5+TankRadius*2-1e-6 {
		t.Fatal("tanks overlapped")
	}
}

func TestBulletDestroysBrickWithoutTunnelling(t *testing.T) {
	g := quietGame()
	g.grid[19][12] = '+'
	g.bullets = append(g.bullets, projectile{Bullet: Bullet{ID: 90, X: 12.5, Y: 20.05, Dir: 0, Owner: "player"}, speed: 30, sourceID: 1})
	g.Step(.1)
	if g.grid[19][12] != '.' {
		t.Fatal("fast bullet tunneled through brick")
	}
	if len(g.bullets) != 0 {
		t.Fatal("brick impact must consume projectile")
	}
	if g.Snapshot().Grid[19][12] != '.' {
		t.Fatal("snapshot did not reflect destroyed brick")
	}
}

func TestWaterDoesNotStopBulletAndSteelDoes(t *testing.T) {
	g := quietGame()
	g.grid[19][12] = '~'
	g.grid[18][12] = '#'
	g.bullets = append(g.bullets, projectile{Bullet: Bullet{ID: 90, X: 12.5, Y: 20.05, Dir: 0, Owner: "player"}, speed: 30, sourceID: 1})
	g.Step(.1)
	if len(g.bullets) != 0 || g.grid[19][12] != '~' || g.grid[18][12] != '#' {
		t.Fatal("incorrect water/steel projectile semantics")
	}
}

func TestKillCompletesWaveAndScoreFreezes(t *testing.T) {
	g := quietGame()
	g.spawned = 1
	g.enemies = append(g.enemies, tankState{Tank: Tank{ID: 2, X: 12.5, Y: 18.5, HP: 1, MaxHP: 1, Kind: "scout"}})
	g.bullets = append(g.bullets, projectile{Bullet: Bullet{ID: 90, X: 12.5, Y: 19.1, Dir: 0, Owner: "player"}, speed: 10, sourceID: 1})
	g.Step(.1)
	s := g.Snapshot()
	if s.Status != "won" || s.Kills != 1 || s.Score != 2150 || s.EnemiesRemaining != 0 {
		t.Fatalf("unexpected victory: %+v", s)
	}
	advance(g, 100)
	if !reflect.DeepEqual(s, g.Snapshot()) {
		t.Fatal("terminal game must not continue changing")
	}
}

func TestHeavyNeedsTwoHits(t *testing.T) {
	g := quietGame()
	g.spawned = 1
	g.enemies = append(g.enemies, tankState{Tank: Tank{ID: 2, X: 12.5, Y: 18.5, HP: 2, MaxHP: 2, Kind: "heavy"}})
	for hit := 1; hit <= 2; hit++ {
		g.bullets = append(g.bullets, projectile{Bullet: Bullet{ID: 90 + hit, X: 12.5, Y: 19.1, Dir: 0, Owner: "player"}, speed: 10, sourceID: 1})
		g.Step(.1)
		if hit == 1 && (len(g.enemies) != 1 || g.enemies[0].HP != 1 || g.kills != 0) {
			t.Fatal("heavy died after one hit")
		}
	}
	if g.status != "won" || g.score != 2250 {
		t.Fatal("heavy kill not scored")
	}
}

func TestSimultaneousFinalKillAndObjectiveLossIsDefeat(t *testing.T) {
	for _, finalLoss := range []string{"base", "player"} {
		t.Run(finalLoss, func(t *testing.T) {
			g := quietGame()
			g.spawned = 1
			g.enemies = append(g.enemies, tankState{Tank: Tank{ID: 2, X: 3.5, Y: 3.5, HP: 1, MaxHP: 1, Kind: "scout"}})
			g.bullets = append(g.bullets, projectile{Bullet: Bullet{ID: 90, X: 3.5, Y: 4.1, Dir: 0, Owner: "player"}, speed: 10, sourceID: 1})
			x, y := g.base.X, g.base.Y
			if finalLoss == "base" {
				g.base.HP = 1
			} else {
				g.lives = 1
				g.player.Shield = false
				g.player.shieldTime = 0
				x, y = g.player.X, g.player.Y
			}
			g.bullets = append(g.bullets, projectile{Bullet: Bullet{ID: 91, X: x, Y: y - .6, Dir: 2, Owner: "enemy"}, speed: 10, sourceID: 2})
			g.Step(.1)
			if g.kills != 1 || g.status != "lost" || g.score != 100 {
				t.Fatalf("simultaneous final loss incorrectly awarded victory: kills=%d status=%s score=%d", g.kills, g.status, g.score)
			}
		})
	}
}

func TestPlayerLivesRespawnAndShield(t *testing.T) {
	g := quietGame()
	g.player.Shield = false
	g.player.shieldTime = 0
	hit := func() {
		g.bullets = append(g.bullets, projectile{Bullet: Bullet{ID: g.allocateID(), X: g.player.X, Y: g.player.Y - .6, Dir: 2, Owner: "enemy"}, speed: 10, sourceID: 90})
		g.Step(.1)
	}
	hit()
	if g.lives != 2 || g.player.HP != 0 {
		t.Fatal("player damage did not consume exactly one life")
	}
	advance(g, 38)
	if g.player.HP != 1 || !g.player.Shield {
		t.Fatal("player failed to respawn with shield")
	}
	hit()
	if g.lives != 2 || g.player.HP != 1 {
		t.Fatal("spawn shield failed")
	}
	for g.lives > 0 {
		g.player.shieldTime = 0
		g.player.Shield = false
		hit()
		if g.lives > 0 {
			advance(g, 38)
		}
	}
	if g.status != "lost" {
		t.Fatal("no lives must end the run")
	}
}

func TestBaseDamageIsAuthoritativeAndFriendlyFireSafe(t *testing.T) {
	g := quietGame()
	shot := func(owner string) {
		g.bullets = append(g.bullets, projectile{Bullet: Bullet{ID: g.allocateID(), X: g.base.X, Y: g.base.Y - .7, Dir: 2, Owner: owner}, speed: 10, sourceID: 90})
		g.Step(.1)
	}
	shot("player")
	if g.base.HP != 3 {
		t.Fatal("player should not accidentally destroy own base")
	}
	for range 3 {
		shot("enemy")
	}
	if g.base.HP != 0 || g.status != "lost" {
		t.Fatalf("base destruction failed: HP %d, status %s", g.base.HP, g.status)
	}
}

func TestInputsCannotGoBackwardsAndCanBeCleared(t *testing.T) {
	g := quietGame()
	g.SetInput(Input{Seq: 3, Move: 2, Fire: true})
	g.SetInput(Input{Seq: 2, Move: 4})
	if g.input.Move != 2 {
		t.Fatal("accepted stale input")
	}
	g.ClearInput()
	if g.input.Move != 0 || g.input.Fire || g.input.Seq != 3 {
		t.Fatal("clear changed sequence")
	}
	g.SetInput(Input{Seq: 3, Move: 1})
	if g.input.Move != 1 {
		t.Fatal("equal sequence must allow transport idle clearing")
	}
}

func TestDeterministicReplayAndSnapshotIsolation(t *testing.T) {
	level, _ := levels.Get(3)
	a, b := New(level, 991), New(level, 991)
	for frame := 0; frame < 1200; frame++ {
		in := Input{Seq: uint64(frame + 1), Move: (frame / 70) % 5, Fire: frame%4 != 0}
		a.SetInput(in)
		b.SetInput(in)
		a.Step(1.0 / 30)
		b.Step(1.0 / 30)
		if frame%61 == 0 {
			aa, _ := json.Marshal(a.Snapshot())
			bb, _ := json.Marshal(b.Snapshot())
			if string(aa) != string(bb) {
				t.Fatalf("replay diverged at frame %d", frame)
			}
		}
	}
	s := a.Snapshot()
	original := a.Snapshot().Grid[0]
	s.Grid[0] = "mutated"
	if a.Snapshot().Grid[0] != original {
		t.Fatal("snapshot aliases live grid")
	}
}

func TestObservationSchemaFiniteAndInputPrivate(t *testing.T) {
	g := New(fixture(), 42)
	advance(g, 35)
	if len(g.enemies) == 0 {
		t.Fatal("expected enemy spawn")
	}
	o, ok := g.ObservationFor(g.enemies[0].ID)
	if !ok {
		t.Fatal("observation unavailable")
	}
	if o.Version != 1 || len(o.Values) != 320 {
		t.Fatal("observation schema changed without version update")
	}
	for i, v := range o.Values {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) || v < -1.001 || v > 1.001 {
			t.Fatalf("invalid feature %d=%v", i, v)
		}
	}
	g.SetInput(Input{Seq: 999, Move: 4, Fire: true})
	after, _ := g.ObservationFor(g.enemies[0].ID)
	if o != after {
		t.Fatal("actor observed unapplied private player input")
	}
	for index := 0; index < 10; index++ {
		if ActionFromIndex(index).Index() != index {
			t.Fatalf("action mapping %d", index)
		}
	}
}

func TestPolicyHasPerceptionDelayAndInvalidActionsAreSafe(t *testing.T) {
	g := quietGame()
	g.enemies = append(g.enemies, tankState{Tank: Tank{ID: 2, X: 2.5, Y: 2.5, HP: 1, MaxHP: 1, Kind: "scout"}, speed: 1})
	g.SetPolicy(invalidPolicy{})
	for range 5 {
		g.decide(&g.enemies[0])
	}
	if g.enemies[0].action != (Action{}) {
		t.Fatal("invalid actor output must become idle")
	}
	if g.enemies[0].memory.Decisions != 3 {
		t.Fatal("first two decisions must fill perception delay")
	}
}

type invalidPolicy struct{}

func (invalidPolicy) Name() string { return "invalid-test" }
func (invalidPolicy) Decide(_ Observation, m *PolicyMemory) Action {
	m.Decisions++
	return Action{Move: 99, Fire: true}
}

func TestEveryOriginalLevelRunsAndProducesValidState(t *testing.T) {
	for _, level := range levels.All() {
		t.Run(level.Name, func(t *testing.T) {
			g := New(level, 73)
			advance(g, 1800)
			s := g.Snapshot()
			if len(s.Grid) != 24 || s.TotalEnemies != level.EnemyCount || len(s.Enemies) > level.MaxActive {
				t.Fatal("invalid state")
			}
			if _, err := json.Marshal(s); err != nil {
				t.Fatal(err)
			}
			for _, tank := range s.Enemies {
				if tank.X < 0 || tank.Y < 0 || tank.X >= 24 || tank.Y >= 24 {
					t.Fatal("enemy outside map")
				}
			}
		})
	}
}

func TestScriptedFallbackPressuresAnUnattendedDefender(t *testing.T) {
	level, _ := levels.Get(1)
	g := New(level, 73)
	advance(g, 30*180)
	if g.base.HP == 3 && g.lives == 3 {
		t.Fatal("fallback never threatened the player or base in three minutes")
	}
}

func BenchmarkTwentyFiveRooms(b *testing.B) {
	level, _ := levels.Get(10)
	var rooms [25]*Game
	for i := range rooms {
		rooms[i] = New(level, int64(i+1))
		advance(rooms[i], 400)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for _, g := range rooms {
			if g.status != "playing" {
				g.status = "playing"
				g.lives = 3
				g.base.HP = 3
				g.elapsed = 30
				g.player.HP = 1
				g.player.shieldTime = 1e6
			}
			g.Step(1.0 / 30)
		}
	}
}

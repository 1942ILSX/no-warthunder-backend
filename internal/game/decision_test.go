package game

import (
	"math"
	"reflect"
	"testing"

	"BattleCityReforge/internal/levels"
)

func TestObservationV2ClampsLaneSnapVelocity(t *testing.T) {
	g := New(fixture(), 42)
	advance(g, 35)
	g.SetInput(Input{Seq: 1, Move: 2})
	advance(g, 2)
	g.SetInput(Input{Seq: 2, Move: 1})
	g.Step(1.0 / SimulationHz)
	if math.Abs(g.playerVelocity[0]/PlayerSpeed) <= 1 {
		t.Fatal("fixture must exercise a perpendicular snap faster than movement")
	}
	o, ok := g.ObservationFor(g.enemies[0].ID)
	if !ok || o.Version != 2 || NormalizationVersion != 2 || o.Values[210] != -1 {
		t.Fatalf("velocity normalization: %+v", o.Values[208:212])
	}
	for i, v := range o.Values {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) || v < -1 || v > 1 {
			t.Fatalf("feature %d outside normalized range: %v", i, v)
		}
	}
}

func TestOfflineBarrierScriptedParityAndWarmup(t *testing.T) {
	level, _ := levels.Get(7)
	ordinary, offline := New(level, 123), New(level, 123)
	seen := map[int]int{}
	barriers, empty := 0, 0
	offline.SetDecisionHook(func(frame DecisionFrame) DecisionActions {
		barriers++
		if len(frame.State.Enemies) == 0 {
			empty++
		}
		for slot, id := range frame.EnemyIDs {
			if id == 0 {
				if frame.Active[slot] || !frame.Observations[slot].Mask[0] {
					t.Fatal("vacant slot must be idle-only")
				}
				continue
			}
			seen[id]++
			if frame.Active[slot] != (seen[id] > 2) {
				t.Fatalf("wrong warmup for id %d decision %d", id, seen[id])
			}
		}
		return DecisionActions{Enemy: [DecisionSlots]int{-1, -1, -1, -1, -1}}
	})
	for tick := 0; tick < 900; tick++ {
		input := Input{Seq: uint64(tick + 1), Move: (tick / 47) % 5, Fire: true}
		ordinary.SetInput(input)
		offline.SetInput(input)
		ordinary.Step(1.0 / SimulationHz)
		offline.Step(1.0 / SimulationHz)
		if !reflect.DeepEqual(ordinary.Snapshot(), offline.Snapshot()) {
			t.Fatalf("training hook diverged at physics tick %d", tick)
		}
	}
	if barriers < 30 || empty == 0 || len(seen) == 0 {
		t.Fatalf("insufficient barrier coverage: %d/%d/%d", barriers, empty, len(seen))
	}
}

func TestOfflineSlotsKeepIdentityAndResetWarmup(t *testing.T) {
	g := quietGame()
	g.enemies = []tankState{{Tank: Tank{ID: 2, X: 2.5, Y: 2.5, HP: 1, MaxHP: 1}}, {Tank: Tank{ID: 3, X: 5.5, Y: 2.5, HP: 1, MaxHP: 1}}}
	var latest DecisionFrame
	g.SetDecisionHook(func(f DecisionFrame) DecisionActions { latest = f; return DecisionActions{} })
	for range 3 {
		g.runDecisionHook()
	}
	if !latest.Active[0] || !latest.Active[1] {
		t.Fatal("warmup did not finish")
	}
	g.enemies = g.enemies[1:]
	g.enemies = append(g.enemies, tankState{Tank: Tank{ID: 4, X: 2.5, Y: 2.5, HP: 1, MaxHP: 1}})
	g.runDecisionHook()
	if latest.EnemyIDs != ([5]int{4, 3}) || latest.Active[0] || !latest.Active[1] {
		t.Fatalf("slot reuse did not reset identity/history: ids=%v active=%v", latest.EnemyIDs, latest.Active)
	}
}

func TestOfflineDefenderInputBeginsNextPhysicsTick(t *testing.T) {
	g := New(fixture(), 3)
	g.SetInput(Input{Seq: 1, Move: 2})
	x, y := g.player.X, g.player.Y
	calls := 0
	g.SetDecisionHook(func(f DecisionFrame) DecisionActions {
		calls++
		if math.Abs(f.State.Player.X-(x+PlayerSpeed/SimulationHz)) > 1e-8 || f.State.Player.Y != y {
			t.Fatal("barrier moved before player integration")
		}
		return DecisionActions{Defender: &Input{Seq: 2, Move: 3}}
	})
	g.Step(1.0 / SimulationHz)
	if g.player.Y != y {
		t.Fatal("new defender action applied too early")
	}
	g.Step(1.0 / SimulationHz)
	if math.Abs(g.player.Y-(y+PlayerSpeed/SimulationHz)) > 1e-8 || calls != 1 {
		t.Fatal("defender action did not apply next tick")
	}
}

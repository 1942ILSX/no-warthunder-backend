package game

// DecisionSlots covers every level accepted by levels.Validate. This fixed bound
// is independent of wave size: a replacement tank reuses a vacant slot with a
// fresh ID, policy memory, and delayed-observation history.
const DecisionSlots = 5

// DecisionFrame is an offline control boundary at the real production decision
// point. State/Input are current public state, while Observations are delayed.
// The callback must not call Step or mutate the Game while it is suspended.
type DecisionFrame struct {
	Observations [DecisionSlots]Observation
	Active       [DecisionSlots]bool
	EnemyIDs     [DecisionSlots]int
	Stuck        [DecisionSlots]float64
	State        State
	Input        Input
	CanFire      bool
	Spawned      int
}

// DecisionActions uses -1 for the normal installed Policy, and 0..9 for supplied
// categorical actions. Defender input starts next physics tick: this tick's
// player movement has already happened. Abort is only for discarding an offline
// world, not pausing or cancelling a production run.
type DecisionActions struct {
	Enemy    [DecisionSlots]int
	Defender *Input
	Abort    bool
}

type DecisionHook func(DecisionFrame) DecisionActions

// SetDecisionHook is an optional offline training seam. A nil hook preserves the
// ordinary Policy path. Install it before the first Step; live server rooms never
// use this hook. The owner is responsible for cancellation of a blocking hook.
func (g *Game) SetDecisionHook(hook DecisionHook) {
	g.decisionHook = hook
	g.decisionSlots = [DecisionSlots]int{}
}

func IdleObservation() Observation {
	o := Observation{Version: ObservationVersion}
	o.Mask[0] = true
	o.Values[298] = 1
	return o
}

func (g *Game) runDecisionHook() bool {
	var frame DecisionFrame
	var tanks [DecisionSlots]*tankState
	// Keep existing IDs in the same slot, releasing only tanks that have died.
	for slot, id := range g.decisionSlots {
		for i := range g.enemies {
			if id != 0 && g.enemies[i].ID == id && g.enemies[i].HP > 0 {
				tanks[slot] = &g.enemies[i]
				break
			}
		}
		if tanks[slot] == nil {
			g.decisionSlots[slot] = 0
		}
	}
	for i := range g.enemies {
		t := &g.enemies[i]
		if t.HP <= 0 {
			continue
		}
		found := false
		for _, id := range g.decisionSlots {
			found = found || id == t.ID
		}
		if found {
			continue
		}
		for slot, id := range g.decisionSlots {
			if id == 0 {
				g.decisionSlots[slot], tanks[slot] = t.ID, t
				break
			}
		}
	}
	for slot, t := range tanks {
		frame.Observations[slot] = IdleObservation()
		if t == nil {
			continue
		}
		frame.EnemyIDs[slot] = t.ID
		frame.Stuck[slot] = t.stuckTime
		if o, ready := g.delayedObservation(t); ready {
			frame.Observations[slot], frame.Active[slot] = o, true
		}
	}
	frame.State = g.Snapshot()
	frame.Input, frame.Spawned = g.input, g.spawned
	frame.CanFire = g.player.HP > 0 && g.player.cooldown <= 0
	shots := 0
	for _, bullet := range g.bullets {
		if !bullet.dead && bullet.sourceID == g.player.ID {
			shots++
		}
	}
	frame.CanFire = frame.CanFire && shots < 3
	control := g.decisionHook(frame)
	if control.Abort {
		return false
	}
	// Preserve production iteration order, including ScriptedPolicy RNG state.
	for i := range g.enemies {
		t := &g.enemies[i]
		for slot, id := range frame.EnemyIDs {
			if id != t.ID || !frame.Active[slot] {
				continue
			}
			o := frame.Observations[slot]
			var action Action
			if control.Enemy[slot] == -1 {
				action = g.policy.Decide(o, &t.memory)
			} else {
				action = ActionFromIndex(control.Enemy[slot])
				t.memory.Decisions++
				t.memory.LastAction = action
			}
			g.applyDecision(t, o, action)
			break
		}
	}
	if control.Defender != nil {
		g.SetInput(*control.Defender)
	}
	return true
}

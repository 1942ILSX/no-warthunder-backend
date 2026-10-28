package training

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"BattleCityReforge/internal/game"
)

func baselineActions() Actions { return Actions{Enemy: [5]int{-1, -1, -1, -1, -1}, Defender: -1} }

func TestStepLimitBootstrapAndPersistentTerminal(t *testing.T) {
	w, err := NewWorld(Config{Seed: 3, MaxSteps: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	initial := w.Current()
	if initial.Info.Tick != 1 || initial.Info.DecisionStep != 0 || initial.Frame.Active != ([5]bool{}) {
		t.Fatalf("wrong first barrier: %+v", initial.Info)
	}
	end, err := w.Step(baselineActions())
	if err != nil {
		t.Fatal(err)
	}
	if end.Terminated || !end.Truncated || end.Info.Outcome != "step_limit" || end.EnemyReward != 0 || end.DefenderReward != 0 || end.Info.DecisionStep != 1 {
		t.Fatalf("bad artificial cutoff: %+v", end.Info)
	}
	critic := CriticObservation(end)
	if critic[2675] != 0 {
		t.Fatal("truncation must preserve nonterminal critic state")
	}
	again, _ := w.Step(baselineActions())
	if !reflect.DeepEqual(end, again) {
		t.Fatal("terminal world advanced before reset")
	}
}

func TestDamageAndTerminalRewards(t *testing.T) {
	before := game.State{Base: game.Base{HP: 3}, Lives: 3, Enemies: []game.Tank{{ID: 2, HP: 2}}}
	after := game.State{Base: game.Base{HP: 2}, Lives: 2, Enemies: []game.Tank{{ID: 2, HP: 1}, {ID: 3, HP: 2}}}
	attack, defend := reward(before, after, "playing")
	if math.Abs(float64(attack-.045)) > 1e-6 || attack != -defend {
		t.Fatalf("damage shaping %v %v", attack, defend)
	}
	for _, outcome := range []string{"enemy_win_base", "enemy_win_lives"} {
		a, d := reward(before, after, outcome)
		if math.Abs(float64(a-1.045)) > 1e-6 || a != -d {
			t.Fatalf("physical win %s: %v %v", outcome, a, d)
		}
	}
	a, d := reward(before, before, "timeout")
	if a != -.25 || d != -.25 {
		t.Fatal("timeout rewarded as a victory")
	}
	a, d = reward(before, before, "step_limit")
	if a != 0 || d != 0 {
		t.Fatal("artificial truncation must not create a reward")
	}
	a, d = reward(before, before, "defender_win")
	if a != -1 || d != 1 {
		t.Fatal("wave clear reward")
	}
	a, d = reward(before, before, "playing")
	if a != 0 || d != 0 {
		t.Fatal("unchanged HP (including shield hits) must not reward")
	}
}

func TestBatchResetValidationAndCancellation(t *testing.T) {
	var batch Batch
	_, err := batch.Reset([]Config{{Seed: 1, MaxSteps: 20}, {LevelID: 2, Seed: 2, MaxSteps: 20}})
	if err != nil {
		t.Fatal(err)
	}
	states, err := batch.Step([]Actions{baselineActions(), baselineActions()})
	if err != nil {
		t.Fatal(err)
	}
	old := states[1].Info
	bad := baselineActions()
	bad.Enemy[4] = 99
	if _, err = batch.Step([]Actions{baselineActions(), bad}); err == nil {
		t.Fatal("accepted invalid actions")
	}
	if batch.worlds[0].current.Info.DecisionStep != 1 {
		t.Fatal("partial mutation before validation")
	}
	if _, err = batch.ResetIndices([]int{0, 0}, []Config{{}, {}}); err == nil {
		t.Fatal("accepted duplicate indices")
	}
	states, err = batch.ResetIndices([]int{0}, []Config{{LevelID: 3, Seed: 8, MaxSteps: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if states[0].Info.LevelID != 3 || states[0].Info.DecisionStep != 0 || states[1].Info != old {
		t.Fatal("reset_indices advanced or reset an unaffected room")
	}
	done := make(chan struct{})
	go func() { batch.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("close leaked a worker waiting at its action barrier")
	}
}

func TestDeterministicRolloutsAndFiniteEncoders(t *testing.T) {
	var a, b Batch
	defer a.Close()
	defer b.Close()
	configs := make([]Config, 10)
	actions := make([]Actions, 10)
	for i := range configs {
		configs[i] = Config{LevelID: i + 1, Seed: int64(80 + i), MaxSteps: 120}
		actions[i] = baselineActions()
	}
	if _, err := a.Reset(configs); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reset(configs); err != nil {
		t.Fatal(err)
	}
	activeSeen := false
	for step := 0; step < 80; step++ {
		x, err := a.Step(actions)
		if err != nil {
			t.Fatal(err)
		}
		y, err := b.Step(actions)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(x, y) {
			t.Fatalf("same seeds diverged at decision %d", step)
		}
		for _, state := range x {
			for slot, obs := range state.Frame.Observations {
				activeSeen = activeSeen || state.Frame.Active[slot]
				checkFinite(t, obs.Values[:])
				for k, m := range obs.Mask {
					if obs.Values[298+k] != flag(m) {
						t.Fatal("mask and features differ")
					}
				}
			}
			def, mask, active := DefenderObservation(state)
			checkFinite(t, def[:])
			if !active && !mask[0] {
				t.Fatal("inactive defender all masked")
			}
			critic := CriticObservation(state)
			checkFinite(t, critic[:])
		}
	}
	if !activeSeen {
		t.Fatal("no eligible enemies exercised")
	}
}
func checkFinite(t *testing.T, values []float32) {
	t.Helper()
	for i, v := range values {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) || v < -1 || v > 1 {
			t.Fatalf("feature %d=%v", i, v)
		}
	}
}

func readResponse(t *testing.T, r *bufio.Reader) Header {
	t.Helper()
	line, err := r.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var h Header
	if err = json.Unmarshal(line, &h); err != nil {
		t.Fatal(err)
	}
	for i := range h.Arrays {
		a := &h.Arrays[i]
		size := 1
		for _, n := range a.Shape {
			size *= n
		}
		if a.DType != "uint8" {
			size *= 4
		}
		if a.NBytes != size {
			t.Fatalf("invalid descriptor: %+v", a)
		}
		a.data = make([]byte, a.NBytes)
		if _, err = io.ReadFull(r, a.data); err != nil {
			t.Fatal(err)
		}
	}
	return h
}
func TestBinaryProtocolAndErrorRecovery(t *testing.T) {
	input := strings.Join([]string{
		`{"cmd":"reset","configs":[{"level_id":1,"seed":42,"max_steps":2}]}`,
		`{"cmd":"step","enemy_actions":[[0]],"defender_actions":[0]}`,
		`{"cmd":"step","enemy_actions":[[-1,-1,-1,-1,-1]],"defender_actions":[-1]}`,
		`{"cmd":"reset_indices","indices":[0],"configs":[{"level_id":2,"seed":43}]}`,
		`{"cmd":"close"}`,
	}, "\n")
	var output bytes.Buffer
	if err := Serve(strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(&output)
	if h := readResponse(t, r); h.Type != "hello" || h.Protocol != 1 || h.ObservationVersion != 2 || h.CriticSize != 2688 {
		t.Fatalf("bad hello: %+v", h)
	}
	if h := readResponse(t, r); h.Type != "transition" || len(h.Arrays) != 12 || h.Infos[0].Tick != 1 {
		t.Fatal("bad reset response")
	}
	if h := readResponse(t, r); h.Type != "error" || len(h.Arrays) != 0 {
		t.Fatal("bad framed error")
	}
	if h := readResponse(t, r); h.Type != "transition" || h.Infos[0].DecisionStep != 1 {
		t.Fatal("error mutated batch")
	}
	if h := readResponse(t, r); h.Infos[0].LevelID != 2 || h.Infos[0].DecisionStep != 0 {
		t.Fatal("bad indexed reset")
	}
	if h := readResponse(t, r); h.Type != "closed" {
		t.Fatal("bad close")
	}
	if _, err := r.ReadByte(); err != io.EOF {
		t.Fatal("stray stdout bytes after protocol")
	}
}
func TestEOFClosesSuspendedWorkers(t *testing.T) {
	done := make(chan error, 1)
	go func() { done <- Serve(strings.NewReader("{\"cmd\":\"reset\",\"configs\":[{},{}]}\n"), io.Discard) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("EOF left a pending simulation barrier")
	}
}

// This is a bounded deterministic simulation regression, not a learning run.
// Neither side moves or fires, exposing the real core's absorbing 900s timeout.
func TestActualGameTimeoutIsTerminalAndPenalizesBothSides(t *testing.T) {
	w, err := NewWorld(Config{LevelID: 1, Seed: 9, MaxSteps: 9000})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	var result Transition
	for decision := 0; decision < 9001; decision++ {
		result, err = w.Step(Actions{})
		if err != nil {
			t.Fatal(err)
		}
		if result.Done() {
			break
		}
	}
	if !result.Terminated || result.Truncated || result.Info.Outcome != "timeout" || result.EnemyReward != -.25 || result.DefenderReward != -.25 {
		t.Fatalf("incorrect actual timeout: info=%+v reward=%v/%v flags=%v/%v", result.Info, result.EnemyReward, result.DefenderReward, result.Terminated, result.Truncated)
	}
	if result.Frame.State.Base.HP != 3 || result.Frame.State.Lives != 3 {
		t.Fatal("idle scenario ended through damage")
	}
}

func TestCancellationAlsoUnblocksPendingObservationSend(t *testing.T) {
	w, err := NewWorld(Config{Seed: 11, MaxSteps: 10})
	if err != nil {
		t.Fatal(err)
	}
	// Resume the initial barrier, then intentionally do not receive the next one.
	w.send(baselineActions())
	done := make(chan struct{})
	go func() { w.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("close leaked a worker publishing an observation")
	}
}

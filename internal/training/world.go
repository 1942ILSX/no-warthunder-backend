// Package training owns offline simulation workers. It is never imported by the
// production HTTP server and cannot create records or change live rooms.
package training

import (
	"context"
	"fmt"

	"BattleCityReforge/internal/game"
	"BattleCityReforge/internal/levels"
)

const (
	CriticSize = 2688
	MaxBatch   = 256
)

type Config struct {
	LevelID     int    `json:"level_id"`
	Seed        int64  `json:"seed"`
	MaxSteps    int    `json:"max_steps"`
	DefenderBot string `json:"defender_bot"`
}

func (c Config) normalized() (Config, error) {
	if c.LevelID == 0 {
		c.LevelID = 1
	}
	if _, ok := levels.Get(c.LevelID); !ok {
		return c, fmt.Errorf("level_id must be 1..10")
	}
	if c.MaxSteps == 0 {
		c.MaxSteps = 9000
	}
	if c.MaxSteps < 1 || c.MaxSteps > 9000 {
		return c, fmt.Errorf("max_steps must be 1..9000")
	}
	if c.DefenderBot == "" {
		c.DefenderBot = "mixed"
	}
	switch c.DefenderBot {
	case "mixed", "guard", "hunter", "wander":
	default:
		return c, fmt.Errorf("unknown defender_bot %q", c.DefenderBot)
	}
	return c, nil
}

type Actions struct {
	Enemy    [game.DecisionSlots]int
	Defender int
}

func (a Actions) validate() error {
	if a.Defender < -1 || a.Defender >= game.ActionCount {
		return fmt.Errorf("defender action must be -1..9")
	}
	for _, action := range a.Enemy {
		if action < -1 || action >= game.ActionCount {
			return fmt.Errorf("enemy action must be -1..9")
		}
	}
	return nil
}

type Info struct {
	LevelID      int    `json:"level_id"`
	Seed         int64  `json:"seed"`
	Outcome      string `json:"outcome"`
	Tick         uint64 `json:"tick"`
	DecisionStep int    `json:"decision_step"`
	Kills        int    `json:"kills"`
	Score        int    `json:"score"`
	BaseHP       int    `json:"base_hp"`
	Lives        int    `json:"lives"`
	Spawned      int    `json:"spawned"`
	EnemyAlive   int    `json:"enemy_alive"`
	EnemyStuck   int    `json:"enemy_stuck"`
}

type Transition struct {
	CriticActive   [game.DecisionSlots]bool // preserve continuation state on artificial truncation
	Frame          game.DecisionFrame
	EnemyReward    float32
	DefenderReward float32
	Terminated     bool
	Truncated      bool
	Info           Info
}

func (t Transition) Done() bool { return t.Terminated || t.Truncated }

type World struct {
	config  Config
	policy  game.Policy // immutable shared policy; mutable memory remains per tank
	cancel  context.CancelFunc
	done    chan struct{}
	events  chan Transition
	actions chan Actions
	current Transition // owned only by the batch/IPC goroutine
}

func NewWorld(config Config) (*World, error) {
	return NewWorldWithPolicy(config, nil)
}

// NewWorldWithPolicy installs an offline evaluation policy before the first
// simulation tick. A nil policy retains the ordinary scripted fallback.
// Supplied policies must permit concurrent calls with separate PolicyMemory.
func NewWorldWithPolicy(config Config, policy game.Policy) (*World, error) {
	config, err := config.normalized()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &World{config: config, policy: policy, cancel: cancel, done: make(chan struct{}), events: make(chan Transition), actions: make(chan Actions)}
	go w.run(ctx)
	w.current = <-w.events
	return w, nil
}

func (w *World) Close()              { w.cancel(); <-w.done }
func (w *World) Current() Transition { return w.current }

func (w *World) send(action Actions) {
	if !w.current.Done() {
		w.actions <- action
	}
}
func (w *World) receive() Transition {
	if w.current.Done() {
		w.current.EnemyReward, w.current.DefenderReward = 0, 0
		return w.current
	}
	next := <-w.events
	next.EnemyReward, next.DefenderReward = reward(w.current.Frame.State, next.Frame.State, next.Info.Outcome)
	w.current = next
	return next
}
func (w *World) Step(action Actions) (Transition, error) {
	if err := action.validate(); err != nil {
		return Transition{}, err
	}
	w.send(action)
	return w.receive(), nil
}

func (w *World) run(ctx context.Context) {
	defer close(w.done)
	level, _ := levels.Get(w.config.LevelID)
	g := game.New(level, w.config.Seed)
	g.SetPolicy(w.policy)
	bot := newDefenderBot(w.config.DefenderBot, w.config.Seed)
	step := 0
	var last game.DecisionFrame
	truncated := false
	emit := func(frame game.DecisionFrame, outcome string, terminated, cut bool) bool {
		info := Info{LevelID: level.ID, Seed: w.config.Seed, Outcome: outcome, Tick: frame.State.Tick,
			DecisionStep: step, Kills: frame.State.Kills, Score: frame.State.Score, BaseHP: frame.State.Base.HP,
			Lives: frame.State.Lives, Spawned: frame.Spawned, EnemyAlive: len(frame.State.Enemies)}
		for _, stuck := range frame.Stuck {
			if stuck >= .7 {
				info.EnemyStuck++
			}
		}
		criticActive := frame.Active
		if terminated {
			criticActive = [game.DecisionSlots]bool{}
		}
		if terminated || cut {
			deactivate(&frame)
		}
		select {
		case w.events <- Transition{CriticActive: criticActive, Frame: frame, Terminated: terminated, Truncated: cut, Info: info}:
			return true
		case <-ctx.Done():
			return false
		}
	}
	g.SetDecisionHook(func(frame game.DecisionFrame) game.DecisionActions {
		last = frame
		if step >= w.config.MaxSteps {
			truncated = true
			emit(frame, "step_limit", false, true)
			return game.DecisionActions{Abort: true}
		}
		if !emit(frame, "playing", false, false) {
			return game.DecisionActions{Abort: true}
		}
		select {
		case action := <-w.actions:
			step++
			defender := game.ActionFromIndex(action.Defender)
			if action.Defender == -1 {
				defender = bot.decide(frame.State)
			}
			if !frame.CanFire {
				defender.Fire = false
			}
			if frame.State.Player.HP <= 0 {
				defender = game.Action{}
			}
			input := game.Input{Seq: uint64(step), Move: defender.Move, Fire: defender.Fire}
			return game.DecisionActions{Enemy: action.Enemy, Defender: &input}
		case <-ctx.Done():
			return game.DecisionActions{Abort: true}
		}
	})
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		g.Step(1.0 / game.SimulationHz)
		if truncated {
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
		if g.Status() != "playing" {
			s := g.Snapshot()
			outcome := "timeout"
			if s.Base.HP <= 0 {
				outcome = "enemy_win_base"
			} else if s.Lives <= 0 {
				outcome = "enemy_win_lives"
			} else if s.Status == "won" {
				outcome = "defender_win"
			}
			last.State, last.Spawned, last.CanFire = s, s.Kills+len(s.Enemies), false
			// Retain only live identities in a final frame; no new actor decision runs.
			for slot, id := range last.EnemyIDs {
				found := false
				for _, enemy := range s.Enemies {
					found = found || enemy.ID == id
				}
				if !found {
					last.EnemyIDs[slot], last.Stuck[slot] = 0, 0
				}
			}
			emit(last, outcome, true, false)
			return
		}
	}
}

func deactivate(frame *game.DecisionFrame) {
	for slot := range frame.Active {
		frame.Active[slot] = false
		frame.Observations[slot] = game.IdleObservation()
	}
}

func reward(before, after game.State, outcome string) (float32, float32) {
	enemy := float32(max(0, before.Base.HP-after.Base.HP))*.03 + float32(max(0, before.Lives-after.Lives))*.02
	for _, previous := range before.Enemies {
		hp := 0
		for _, current := range after.Enemies {
			if current.ID == previous.ID {
				hp = current.HP
				break
			}
		}
		enemy -= float32(max(0, previous.HP-hp)) * .005
	}
	defender := -enemy
	switch outcome {
	case "enemy_win_base", "enemy_win_lives":
		enemy++
		defender--
	case "defender_win":
		enemy--
		defender++
	case "timeout":
		enemy -= .25
		defender -= .25
	}
	return enemy, defender
}

// Batch serializes its public calls, but steps independent worlds concurrently.
// It validates an entire command before mutating any world.
type Batch struct {
	worlds []*World
	policy game.Policy
}

// NewBatchWithPolicy shares immutable model weights across every room and
// retains that policy across both full and selective resets. The zero Batch
// value continues to use the default scripted policy.
func NewBatchWithPolicy(policy game.Policy) *Batch { return &Batch{policy: policy} }

func (b *Batch) Close() {
	for _, w := range b.worlds {
		w.Close()
	}
	b.worlds = nil
}
func (b *Batch) Len() int { return len(b.worlds) }
func (b *Batch) Reset(configs []Config) ([]Transition, error) {
	if len(configs) < 1 || len(configs) > MaxBatch {
		return nil, fmt.Errorf("batch size must be 1..%d", MaxBatch)
	}
	for _, config := range configs {
		if _, err := config.normalized(); err != nil {
			return nil, err
		}
	}
	b.Close()
	for _, config := range configs {
		w, err := NewWorldWithPolicy(config, b.policy)
		if err != nil {
			b.Close()
			return nil, err
		}
		b.worlds = append(b.worlds, w)
	}
	return b.snapshot(true), nil
}
func (b *Batch) ResetIndices(indices []int, configs []Config) ([]Transition, error) {
	if len(indices) == 0 || len(indices) != len(configs) {
		return nil, fmt.Errorf("indices/configs must have equal nonzero lengths")
	}
	seen := make(map[int]bool, len(indices))
	for i, index := range indices {
		if index < 0 || index >= len(b.worlds) || seen[index] {
			return nil, fmt.Errorf("invalid or duplicate world index")
		}
		seen[index] = true
		if _, err := configs[i].normalized(); err != nil {
			return nil, err
		}
	}
	for i, index := range indices {
		b.worlds[index].Close()
		w, err := NewWorldWithPolicy(configs[i], b.policy)
		if err != nil {
			return nil, err
		}
		b.worlds[index] = w
	}
	return b.snapshot(true), nil
}
func (b *Batch) Step(actions []Actions) ([]Transition, error) {
	if len(b.worlds) == 0 || len(actions) != len(b.worlds) {
		return nil, fmt.Errorf("actions must match nonempty batch")
	}
	for _, action := range actions {
		if err := action.validate(); err != nil {
			return nil, err
		}
	}
	for i, w := range b.worlds {
		w.send(actions[i])
	}
	result := make([]Transition, len(b.worlds))
	for i, w := range b.worlds {
		result[i] = w.receive()
	}
	return result, nil
}
func (b *Batch) snapshot(zeroRewards bool) []Transition {
	result := make([]Transition, len(b.worlds))
	for i, w := range b.worlds {
		result[i] = w.current
		if zeroRewards {
			result[i].EnemyReward, result[i].DefenderReward = 0, 0
		}
	}
	return result
}

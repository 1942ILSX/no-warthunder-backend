package main

import (
	"context"
	"errors"
	"fmt"

	"BattleCityReforge/internal/game"
	"BattleCityReforge/internal/levels"
)

const observationCorpusCapacity = 32

type capturedObservation struct {
	observation game.Observation
	used        bool
}

// Every entry was accepted by the strict actor at a real delayed decision.
// Fixed storage bounds memory; a new capture replaces the oldest entry.
type observationCorpus struct {
	entries        [observationCorpusCapacity]capturedObservation
	next, size     int
	cursor         int
	captured, used uint64
}

func (c *observationCorpus) capture(o game.Observation) {
	c.entries[c.next] = capturedObservation{observation: o}
	c.next = (c.next + 1) % len(c.entries)
	c.size = min(c.size+1, len(c.entries))
	c.captured++
}

func (c *observationCorpus) sample() (game.Observation, bool) {
	if c.size == 0 {
		return game.Observation{}, false
	}
	entry := &c.entries[c.cursor%c.size]
	c.cursor = (c.cursor + 1) % c.size
	if !entry.used {
		entry.used = true
		c.used++
	}
	return entry.observation, true
}

type inferenceFill struct {
	corpus       observationCorpus
	memories     [3]game.PolicyMemory // Never passed to a live Game.
	calls        [3]uint64
	invalid      [3]uint64
	firstError   [3]string
	liveBaseline uint64
	target       uint64
	remainder    int
	actorCursor  int
}

func liveCalls(policies [3]*countedPolicy) uint64 {
	var total uint64
	for _, p := range policies {
		total += p.calls - p.warmupCalls
	}
	return total
}

func (w *workload) selectedFillActor() int {
	if w.config.Profile != "mixed" {
		for i, name := range profileNames {
			if name == w.config.Profile {
				return i
			}
		}
	}
	actor := w.fill.actorCursor % len(w.policies)
	w.fill.actorCursor = (w.fill.actorCursor + 1) % len(w.policies)
	return actor
}

// Normal mixed rooms begin before any enemy has spawned. Acquire a real delayed
// observation in one separate, disposable Game instead of fabricating a vector
// or advancing the measured rooms. This cost is included in reported setup.
func (w *workload) prepareInferenceFloor(ctx context.Context) error {
	if w.fill.corpus.size != 0 {
		return nil
	}
	level, _ := levels.Get(7)
	warmup := game.New(level, w.config.Seed)
	actor := w.selectedFillActor()
	w.fill.actorCursor = 0
	policy := w.policies[actor]
	previous := policy.warming
	policy.warming = true
	defer func() { policy.warming = previous }()
	warmup.SetPolicy(policy)
	for tick := 0; tick < 120; tick++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		warmup.Step(1.0 / game.SimulationHz)
		if w.fill.corpus.size > 0 {
			return nil
		}
		if warmup.Status() != "playing" {
			break
		}
	}
	return errors.New("inference floor could not acquire a validated real delayed observation in 120 legal warmup ticks")
}

// The integer accumulator follows completed 30 Hz batches, never an unbounded
// wall-clock debt. Actual 10 Hz inference bursts count toward the cumulative
// target; supplemental calls cover only the remainder. No valid tick can need
// more than ceil(1000/30)=34 supplemental calls after the previous floor was met.
// Replacement pre-roll calls cost real CPU but do not substitute for live calls.
func (w *workload) fillInferenceFloor(ctx context.Context) error {
	f := w.fill
	f.remainder += w.config.InferenceFloor
	f.target += uint64(f.remainder / game.SimulationHz)
	f.remainder %= game.SimulationHz
	combined := liveCalls(w.policies) - f.liveBaseline
	for _, calls := range f.calls {
		combined += calls
	}
	for combined < f.target {
		if err := ctx.Err(); err != nil {
			return err
		}
		observation, ok := f.corpus.sample()
		if !ok {
			return errors.New("inference floor has no real validated observation")
		}
		actor := w.selectedFillActor()
		f.calls[actor]++
		// Same strict MLP + sampler path as live inference, separate memory,
		// deliberately discarded action. Do not call countedPolicy.Decide here:
		// supplemental observations must never recirculate into the real corpus.
		_, _, err := w.policies[actor].model.Evaluate(observation, &f.memories[actor])
		if err != nil {
			f.invalid[actor]++
			if f.firstError[actor] == "" {
				f.firstError[actor] = err.Error()
			}
			return fmt.Errorf("%s supplemental inference rejected captured observation: %w", profileNames[actor], err)
		}
		combined++
	}
	return nil
}

package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"BattleCityReforge/internal/game"
	"BattleCityReforge/internal/policymlp"
)

func fixtureArtifacts(t testing.TB) [3]string {
	t.Helper()
	var paths [3]string
	for i, name := range profileNames {
		paths[i] = t.TempDir()
		weights := make([]byte, ((320+1)*32+(32+1)*16+(16+1)*10)*4)
		// Explicit untrained fixture: move south so three spawn pads can admit
		// the fourth tank through the real spawn pipeline.
		binary.LittleEndian.PutUint32(weights[len(weights)-40+6*4:], math.Float32bits(1))
		digest := sha256.Sum256(weights)
		metadata := policymlp.Metadata{SchemaVersion: 1, ModelName: "budgetcheck-fixture", PolicyVersion: "test-only", Difficulty: name, TrainingSeed: 42, Trained: false,
			ObservationVersion: game.ObservationVersion, NormalizationVersion: game.NormalizationVersion, InputSize: 320, HiddenSizes: []int{32, 16}, OutputSize: 10, Activation: "relu", ActionEncoding: policymlp.ActionEncoding,
			DecisionHz: 10, ObservationDelayDecisions: 2, WeightsFile: "weights.bin", WeightsFormat: policymlp.WeightsFormat, WeightsLayout: policymlp.WeightsLayout, WeightsSHA256: hex.EncodeToString(digest[:]),
			Sampler: policymlp.Sampler{Mode: "argmax", Temperature: 1, RNG: policymlp.SamplerRNG}}
		data, err := json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(paths[i], "metadata.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(paths[i], "weights.bin"), weights, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return paths
}
func testPolicies(t testing.TB) [3]*countedPolicy {
	t.Helper()
	p, err := loadPolicies(fixtureArtifacts(t), true)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBoundedFlagsAndStrictArtifacts(t *testing.T) {
	base := []string{"-trainee", "a", "-expert", "b", "-ace", "c"}
	for _, tail := range [][]string{{"-duration", "61s"}, {"-duration", "0s"}, {"-rooms", "26"}, {"-rooms", "0"}, {"-scenario", "unknown"}, {"-profile", "unknown"}, {"-inference-floor", "999"}, {"-inference-floor", "1001"}, {"-inference-floor", "-1"}, {"-setup-pause", "-1ms"}, {"-setup-pause", "100000001ns"}, {"extra"}} {
		if _, err := parseConfiguration(append(append([]string{}, base...), tail...), io.Discard); err == nil {
			t.Fatalf("accepted %v", tail)
		}
	}
	if _, err := parseConfiguration(base, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, floor := range []string{"0", "1000"} {
		if _, err := parseConfiguration(append(append([]string{}, base...), "-inference-floor", floor), io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	for _, pause := range []string{"0", "30ms", "100ms"} {
		if _, err := parseConfiguration(append(append([]string{}, base...), "-setup-pause", pause), io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	paths := fixtureArtifacts(t)
	if _, err := loadPolicies(paths, false); err == nil || !strings.Contains(err.Error(), "explicitly untrained") {
		t.Fatal("normal CLI path must reject untrained fixtures")
	}
	paths[1] = paths[0]
	if _, err := loadPolicies(paths, true); err == nil || !strings.Contains(err.Error(), "declares difficulty") {
		t.Fatal("profile identity was not checked")
	}
}
func TestPrefillUsesRealGameAndRetainsAllActorIdentities(t *testing.T) {
	p := testPolicies(t)
	c := configuration{Rooms: 3, Scenario: "max-enemies", Profile: "mixed", Serialize: true, Seed: 9}
	w, err := newWorkload(context.Background(), c, p)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range w.rooms {
		if r.profile != i || r.level != 7+i || len(r.state.Enemies) != 4 || r.state.Tick < 30 {
			t.Fatalf("room%d not legally prefilled: level%d enemies%d tick%d", i, r.level, len(r.state.Enemies), r.state.Tick)
		}
	}
	for tick := 0; tick < 12; tick++ {
		if err = w.tick(context.Background(), tick); err != nil {
			t.Fatal(err)
		}
	}
	if w.report.RoomPhysicsTicks != 36 || w.report.Snapshots != 18 || w.report.Inputs != 18 || w.report.SnapshotPayloadBytes == 0 {
		t.Fatalf("incorrect game/serialization cadence: %+v", w.report)
	}
	for _, model := range p {
		if model.calls <= model.warmupCalls || model.invalid != 0 {
			t.Fatal("live actor did not run valid inference")
		}
	}
}
func TestPacedMeasurementAndCancellation(t *testing.T) {
	c := configuration{Rooms: 2, Duration: 220 * time.Millisecond, Scenario: "max-enemies", Profile: "mixed", Serialize: true, Seed: 7, SetupPause: 2 * time.Millisecond}
	start := time.Now()
	result, err := measure(context.Background(), c, testPolicies(t))
	if err != nil {
		t.Fatal(err)
	}
	if result.ElapsedSeconds < .20 || time.Since(start) < c.Duration || result.TickBatches < 3 {
		t.Fatalf("not paced: %+v", result)
	}
	if result.LivePolicyDecisions == 0 || result.PeakRSSBytes <= 0 || result.CPUSeconds <= 0 || result.SnapshotPayloadBytes == 0 {
		t.Fatal("missing measured resources/workload")
	}
	if len(result.Models) != 3 || result.Models[0].Metadata.Trained {
		t.Fatal("test fixture provenance lost")
	}
	if result.SetupPauseMS != 2 || result.SetupSeconds < float64(c.Rooms)*c.SetupPause.Seconds() {
		t.Fatal("initial setup pause was not reported as setup cost")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = measure(ctx, c, testPolicies(t)); err == nil {
		t.Fatal("cancelled setup did not stop")
	}
}

func TestInitialSetupPausePreservesGameStateAndCancelsBeforeRoomWork(t *testing.T) {
	c := configuration{Rooms: 2, Scenario: "max-enemies", Profile: "mixed", Seed: 37, InferenceFloor: 1000}
	plain, err := newWorkload(context.Background(), c, testPolicies(t))
	if err != nil {
		t.Fatal(err)
	}
	c.SetupPause = 3 * time.Millisecond
	started := time.Now()
	paused, err := newWorkload(context.Background(), c, testPolicies(t))
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(started) < time.Duration(c.Rooms)*c.SetupPause {
		t.Fatal("initial room pauses were skipped")
	}
	for tick := 0; tick < 12; tick++ {
		for i := range plain.rooms {
			if !reflect.DeepEqual(plain.rooms[i].game.Snapshot(), paused.rooms[i].game.Snapshot()) {
				t.Fatal("wall-clock setup pauses changed deterministic game state")
			}
		}
		if err = plain.tick(context.Background(), tick); err != nil {
			t.Fatal(err)
		}
		if err = paused.tick(context.Background(), tick); err != nil {
			t.Fatal(err)
		}
	}
	policies := testPolicies(t)
	c.SetupPause = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if _, err = newWorkload(ctx, c, policies); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("setup pause did not respect cancellation: %v", err)
	}
	for _, p := range policies {
		if p.calls != 0 {
			t.Fatal("room inference started before the cancellable initial pause ended")
		}
	}
}
func TestCgroupCountersNeverBecomeQuotaCertification(t *testing.T) {
	before := map[string]any{"cpu_usage_usec": int64(100), "cpu_nr_throttled": int64(2)}
	after := map[string]any{"cpu_usage_usec": int64(160), "cpu_nr_throttled": int64(3), "cpu_max": "50000 100000"}
	delta := cgroupDelta(before, after, 1024)
	if delta["cpu_usage_usec_delta"] != int64(60) || delta["cpu_nr_throttled_delta"] != int64(1) || delta["sampled_peak_memory_current_bytes"] != int64(1024) {
		t.Fatal("bad cgroup deltas")
	}
}

func TestInferenceFloorUsesRealObservationsWithoutChangingGames(t *testing.T) {
	paths := fixtureArtifacts(t)
	// Categorical fixture exercises sampler RNG isolation, not only argmax.
	for _, directory := range paths {
		body, err := os.ReadFile(filepath.Join(directory, "metadata.json"))
		if err != nil {
			t.Fatal(err)
		}
		var metadata policymlp.Metadata
		if err = json.Unmarshal(body, &metadata); err != nil {
			t.Fatal(err)
		}
		metadata.Sampler.Mode = "categorical"
		body, err = json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(directory, "metadata.json"), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	plainPolicies, err := loadPolicies(paths, true)
	if err != nil {
		t.Fatal(err)
	}
	filledPolicies, err := loadPolicies(paths, true)
	if err != nil {
		t.Fatal(err)
	}
	c := configuration{Rooms: 3, Scenario: "mixed", Profile: "mixed", Serialize: true, Seed: 19}
	plain, err := newWorkload(context.Background(), c, plainPolicies)
	if err != nil {
		t.Fatal(err)
	}
	c.InferenceFloor = 1000
	filled, err := newWorkload(context.Background(), c, filledPolicies)
	if err != nil {
		t.Fatal(err)
	}
	if filled.fill.corpus.size == 0 || filled.fill.corpus.captured == 0 {
		t.Fatal("mixed startup must acquire an actual delayed warmup observation")
	}
	for _, r := range filled.rooms {
		if r.game.Snapshot().Tick != 0 {
			t.Fatal("supplemental warmup advanced a measured room")
		}
	}
	var supplementalBefore uint64
	for tick := 0; tick < 150; tick++ {
		if err = plain.tick(context.Background(), tick); err != nil {
			t.Fatal(err)
		}
		if err = filled.tick(context.Background(), tick); err != nil {
			t.Fatal(err)
		}
		var supplemental uint64
		for _, count := range filled.fill.calls {
			supplemental += count
		}
		if supplemental-supplementalBefore > 34 {
			t.Fatal("floor made an unbounded catch-up burst")
		}
		supplementalBefore = supplemental
		combined := liveCalls(filledPolicies) - filled.fill.liveBaseline + supplemental
		target := uint64((tick + 1) * 1000 / game.SimulationHz)
		if filled.fill.target != target || combined < target || combined > target+12 {
			t.Fatalf("bad accumulator: tick=%d combined=%d target=%d", tick, combined, target)
		}
		for i := range plain.rooms {
			if !reflect.DeepEqual(plain.rooms[i].game.Snapshot(), filled.rooms[i].game.Snapshot()) {
				t.Fatalf("supplemental inference changed live room %d at tick %d", i, tick)
			}
		}
	}
	for i := range filled.fill.calls {
		if filled.fill.calls[i] == 0 || filled.fill.invalid[i] != 0 || filled.fill.memories[i].Decisions != filled.fill.calls[i] {
			t.Fatalf("actor %d missing independent valid supplemental memory", i)
		}
	}
	if filled.fill.corpus.used == 0 || filled.fill.corpus.size > observationCorpusCapacity || filled.fill.corpus.used > filled.fill.corpus.captured {
		t.Fatal("invalid bounded corpus accounting")
	}
	if liveCalls(plainPolicies) != liveCalls(filledPolicies)-filled.fill.liveBaseline {
		t.Fatal("supplemental calls leaked into actual game counts")
	}
}

func TestPacedInferenceFloorReportAndSelectedProfile(t *testing.T) {
	c := configuration{Rooms: 2, Duration: 220 * time.Millisecond, Scenario: "mixed", Profile: "expert", Serialize: true, Seed: 7, InferenceFloor: 1000}
	result, err := measure(context.Background(), c, testPolicies(t))
	if err != nil {
		t.Fatal(err)
	}
	if result.InferenceFloor != 1000 || result.SupplementalPolicyDecisions == 0 || result.ValidObservationsCaptured == 0 || result.ValidObservationsUsed == 0 {
		t.Fatal("floor/source evidence missing")
	}
	if result.CombinedPolicyDecisions != result.LivePolicyDecisions+result.SupplementalPolicyDecisions || result.CombinedPolicyDecisions < uint64(result.TickBatches*1000/game.SimulationHz) {
		t.Fatal("actual/supplemental/combined counts disagree")
	}
	if result.Models[0].SupplementalDecisions != 0 || result.Models[1].SupplementalDecisions != result.SupplementalPolicyDecisions || result.Models[2].SupplementalDecisions != 0 {
		t.Fatal("supplemental inference did not respect explicit profile")
	}
	if result.LivePolicyDecisions != 0 || result.PeakEnemies != 0 || result.CPUSeconds <= 0 || result.WorkBatchP99MS <= 0 {
		t.Fatal("floor must not claim supplemental calls are real enemies, and its work must be measured")
	}
}

func TestInferenceFloorCreditsActualTwentyFiveRoomBursts(t *testing.T) {
	c := configuration{Rooms: 25, Scenario: "max-enemies", Profile: "mixed", Seed: 29, InferenceFloor: 1000}
	p := testPolicies(t)
	w, err := newWorkload(context.Background(), c, p)
	if err != nil {
		t.Fatal(err)
	}
	var supplemental uint64
	for tick := 0; tick < 60; tick++ {
		previous := supplemental
		if err = w.tick(context.Background(), tick); err != nil {
			t.Fatal(err)
		}
		supplemental = 0
		for _, count := range w.fill.calls {
			supplemental += count
		}
		combined := liveCalls(p) - w.fill.liveBaseline + supplemental
		if combined < w.fill.target || combined > w.fill.target+100 || supplemental-previous > 34 {
			t.Fatalf("real 10Hz bursts were not credited: target=%d combined=%d extra=%d", w.fill.target, combined, supplemental-previous)
		}
	}
	if liveCalls(p)-w.fill.liveBaseline < 1000 || supplemental >= 1000 || w.report.PeakEnemies != 100 {
		t.Fatal("floor did not share the target with actual full-room actor calls")
	}
}

func BenchmarkTwentyFiveRoomsWithActorsAndJSON(b *testing.B) {
	w, err := newWorkload(context.Background(), configuration{Rooms: 25, Scenario: "max-enemies", Profile: "mixed", Serialize: true, Seed: 17}, testPolicies(b))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err = w.tick(context.Background(), i); err != nil {
			b.Fatal(err)
		}
	}
}

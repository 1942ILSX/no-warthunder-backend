package policymlp

import (
	"BattleCityReforge/internal/game"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func fixtureMetadata(hidden []int) Metadata {
	return Metadata{
		SchemaVersion: 1, ModelName: "test-actor", PolicyVersion: "fixture-v1", Difficulty: "trainee", TrainingSeed: 42, Trained: false,
		ObservationVersion: 2, NormalizationVersion: 2, InputSize: 320, HiddenSizes: hidden, OutputSize: 10,
		Activation: "relu", ActionEncoding: ActionEncoding, DecisionHz: 10, ObservationDelayDecisions: 2,
		WeightsFile: "weights.bin", WeightsFormat: WeightsFormat, WeightsLayout: WeightsLayout,
		Sampler: Sampler{Mode: "argmax", Temperature: 1, RNG: SamplerRNG},
	}
}

func parameterCount(metadata Metadata) int {
	return (metadata.InputSize+1)*metadata.HiddenSizes[0] + (metadata.HiddenSizes[0]+1)*metadata.HiddenSizes[1] + (metadata.HiddenSizes[1]+1)*metadata.OutputSize
}

func writeArtifact(t testing.TB, metadata Metadata, parameters []float32) string {
	t.Helper()
	directory := t.TempDir()
	weights := make([]byte, len(parameters)*4)
	for index, value := range parameters {
		binary.LittleEndian.PutUint32(weights[index*4:], math.Float32bits(value))
	}
	digest := sha256.Sum256(weights)
	metadata.WeightsSHA256 = hex.EncodeToString(digest[:])
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "metadata.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "weights.bin"), weights, 0600); err != nil {
		t.Fatal(err)
	}
	return directory
}

func loadTestModel(t testing.TB, mode string) *Model {
	t.Helper()
	metadata := fixtureMetadata([]int{64, 32})
	metadata.Sampler.Mode = mode
	parameters := make([]float32, parameterCount(metadata))
	for index := range parameters {
		parameters[index] = float32(index%19-9) * .002
	}
	model, err := Load(writeArtifact(t, metadata, parameters))
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func observationWithMask(indices ...int) game.Observation {
	observation := game.Observation{Version: 2}
	for _, index := range indices {
		observation.Mask[index] = true
		observation.Values[MaskOffset+index] = 1
	}
	return observation
}

func TestLoadShapesLayerOrderAndForward(t *testing.T) {
	for _, hidden := range [][]int{{64, 32}, {32, 16}} {
		metadata := fixtureMetadata(hidden)
		parameters := make([]float32, parameterCount(metadata))
		// h1[0] = relu(input[0]*2 + 1) = 2;
		// h2[1] = relu(h1[0]*3 + .5) = 6.5;
		// logits[8] = h2[1]*4 + .25 = 26.25.
		parameters[0] = 2
		parameters[320*hidden[0]] = 1
		second := (320 + 1) * hidden[0]
		parameters[second+hidden[0]] = 3
		parameters[second+hidden[0]*hidden[1]+1] = .5
		third := second + (hidden[0]+1)*hidden[1]
		parameters[third+8*hidden[1]+1] = 4
		parameters[third+10*hidden[1]+8] = .25
		model, err := Load(writeArtifact(t, metadata, parameters))
		if err != nil {
			t.Fatal(err)
		}
		observation := observationWithMask(0, 2, 4, 6, 8)
		observation.Values[0] = .5
		logits, action, err := model.Evaluate(observation, &game.PolicyMemory{})
		if err != nil || logits[8] != 26.25 || action.Index() != 8 {
			t.Fatalf("shape %v logits=%v action=%v error=%v", hidden, logits, action, err)
		}
		if model.Metadata().Trained {
			t.Fatal("untrained fixture lost its provenance")
		}
		copy := model.Metadata()
		copy.HiddenSizes[0] = 999
		if model.Metadata().HiddenSizes[0] != hidden[0] {
			t.Fatal("Metadata exposed mutable shared shape")
		}
	}
}

func TestArgmaxMasksAndTieBreak(t *testing.T) {
	metadata := fixtureMetadata([]int{64, 32})
	parameters := make([]float32, parameterCount(metadata))
	for i := 0; i < 10; i++ {
		parameters[len(parameters)-10+i] = float32(i)
	}
	model, err := Load(writeArtifact(t, metadata, parameters))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		indices []int
		want    int
	}{{[]int{0, 2, 4, 6, 8}, 8}, {[]int{3}, 3}, {[]int{0}, 0}} {
		memory := game.PolicyMemory{}
		action := model.Decide(observationWithMask(tc.indices...), &memory)
		if action.Index() != tc.want || memory.Decisions != 1 || memory.LastAction != action {
			t.Fatalf("mask=%v action=%v memory=%+v", tc.indices, action, memory)
		}
	}
	zeroModel, err := Load(writeArtifact(t, metadata, make([]float32, len(parameters))))
	if err != nil {
		t.Fatal(err)
	}
	if action := zeroModel.Decide(observationWithMask(4, 8), &game.PolicyMemory{}); action.Index() != 4 {
		t.Fatalf("argmax tie must choose lowest legal index: %+v", action)
	}
}

func TestThreeDifficultyArtifacts(t *testing.T) {
	for _, difficulty := range []string{"trainee", "expert", "ace"} {
		metadata := fixtureMetadata([]int{64, 32})
		metadata.Difficulty = difficulty
		model, err := Load(writeArtifact(t, metadata, make([]float32, parameterCount(metadata))))
		if err != nil || model.Metadata().Difficulty != difficulty {
			t.Fatalf("difficulty %s did not survive artifact validation: %v", difficulty, err)
		}
	}
}

func TestCategoricalReproducibleAndNeverMasked(t *testing.T) {
	model := loadTestModel(t, "categorical")
	observation := observationWithMask(0, 4, 8)
	first, second := game.PolicyMemory{Random: 42}, game.PolicyMemory{Random: 42}
	seen := map[int]bool{}
	for i := 0; i < 200; i++ {
		a, b := model.Decide(observation, &first), model.Decide(observation, &second)
		if a != b || first != second {
			t.Fatal("same seed and observation must produce identical actions and memory")
		}
		if !observation.Mask[a.Index()] {
			t.Fatal("categorical selected a masked action")
		}
		seen[a.Index()] = true
	}
	if len(seen) != 3 {
		t.Fatalf("near-uniform fixture failed to visit its three legal actions: %v", seen)
	}
	if NextRandom(42) != 45454805674 {
		t.Fatal("xorshift64 golden value changed")
	}
	if NextRandom(0) != NextRandom(DefaultRandom) || NextRandom(0) == 0 {
		t.Fatal("zero RNG seed must use the documented nonzero seed")
	}
}

func TestInvalidObservationsFallBackSafely(t *testing.T) {
	model := loadTestModel(t, "argmax")
	for _, tc := range []struct {
		name string
		edit func(*game.Observation)
		want error
	}{
		{"version", func(o *game.Observation) { o.Version = 1 }, ErrObservationVersion},
		{"NaN", func(o *game.Observation) { o.Values[0] = float32(math.NaN()) }, ErrObservationValue},
		{"Infinity", func(o *game.Observation) { o.Values[308] = float32(math.Inf(1)) }, ErrObservationValue},
		{"range", func(o *game.Observation) { o.Values[7] = 2 }, ErrObservationValue},
		{"mask mismatch", func(o *game.Observation) { o.Values[MaskOffset+4] = 0 }, ErrActionMask},
		{"empty mask", func(o *game.Observation) { o.Mask = [game.ActionCount]bool{}; o.Values[MaskOffset+4] = 0 }, ErrActionMask},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observation := observationWithMask(4)
			tc.edit(&observation)
			if _, err := model.Logits(observation); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			memory := game.PolicyMemory{}
			action := model.Decide(observation, &memory)
			if action.Move < 0 || action.Move > 4 || action.Fire || memory.Decisions != 1 {
				t.Fatalf("unsafe fallback action=%+v memory=%+v", action, memory)
			}
			if tc.name != "empty mask" && action.Index() != 4 {
				t.Fatal("fallback must enforce the remaining legal action")
			}
		})
	}
}

func TestOverflowUsesScriptedFallback(t *testing.T) {
	metadata := fixtureMetadata([]int{64, 32})
	parameters := make([]float32, parameterCount(metadata))
	parameters[0], parameters[1] = math.MaxFloat32, math.MaxFloat32
	model, err := Load(writeArtifact(t, metadata, parameters))
	if err != nil {
		t.Fatal(err)
	}
	observation := observationWithMask(0, 2, 4, 6, 8)
	observation.Values[0], observation.Values[1] = 1, 1
	if _, err := model.Logits(observation); !errors.Is(err, ErrNonfiniteLogits) {
		t.Fatalf("expected numerical failure, got %v", err)
	}
	first, second := game.PolicyMemory{Random: 42}, game.PolicyMemory{Random: 42}
	got := model.Decide(observation, &first)
	want := (game.ScriptedPolicy{}).Decide(observation, &second)
	if got != want || first != second {
		t.Fatalf("numerical failure did not use scripted fallback: got=%+v want=%+v", got, want)
	}
}

func TestDecideAllocatesZeroHeap(t *testing.T) {
	for _, mode := range []string{"argmax", "categorical"} {
		model := loadTestModel(t, mode)
		observation := observationWithMask(0, 1, 2, 3, 4, 5, 6, 7, 8, 9)
		memory := game.PolicyMemory{Random: 42}
		allocations := testing.AllocsPerRun(1000, func() { _ = model.Decide(observation, &memory) })
		if allocations != 0 {
			t.Errorf("%s Decide allocated %v times", mode, allocations)
		}
	}
}

func TestSharedModelConcurrentIndependentMemory(t *testing.T) {
	model := loadTestModel(t, "categorical")
	observation := observationWithMask(0, 1, 2, 3, 4, 5, 6, 7, 8, 9)
	var expected [20]game.Action
	initial := game.PolicyMemory{Random: 99}
	for i := range expected {
		expected[i] = model.Decide(observation, &initial)
	}
	var workers sync.WaitGroup
	for room := 0; room < 25; room++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			memory := game.PolicyMemory{Random: 99}
			for _, want := range expected {
				if got := model.Decide(observation, &memory); got != want {
					t.Error("another room changed the shared actor")
				}
			}
		}()
	}
	workers.Wait()
}

func TestRejectMalformedMetadata(t *testing.T) {
	base := fixtureMetadata([]int{64, 32})
	for _, tc := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"unknown field", func(m map[string]any) { m["surprise"] = true }},
		{"missing field", func(m map[string]any) { delete(m, "trainingSeed") }},
		{"null trained", func(m map[string]any) { m["trained"] = nil }},
		{"wrong schema", func(m map[string]any) { m["schemaVersion"] = 2 }},
		{"missing difficulty", func(m map[string]any) { delete(m, "difficulty") }},
		{"unknown difficulty", func(m map[string]any) { m["difficulty"] = "easy" }},
		{"wrong observation", func(m map[string]any) { m["observationVersion"] = 1 }},
		{"wrong normalization", func(m map[string]any) { m["normalizationVersion"] = 1 }},
		{"wrong input", func(m map[string]any) { m["inputSize"] = 319 }},
		{"teacher shape", func(m map[string]any) { m["hiddenSizes"] = []int{256, 256} }},
		{"wrong action", func(m map[string]any) { m["actionEncoding"] = "fire*5+move" }},
		{"wrong delay", func(m map[string]any) { m["observationDelayDecisions"] = 0 }},
		{"wrong cadence", func(m map[string]any) { m["decisionHz"] = 30 }},
		{"path traversal", func(m map[string]any) { m["weightsFile"] = "../weights.bin" }},
		{"bad layout", func(m map[string]any) { m["weightsLayout"] = "column-major" }},
		{"negative seed", func(m map[string]any) { m["trainingSeed"] = -1 }},
		{"bad sampler", func(m map[string]any) {
			m["sampler"] = map[string]any{"mode": "random", "temperature": 1, "rng": SamplerRNG}
		}},
		{"missing sampler field", func(m map[string]any) { m["sampler"] = map[string]any{"mode": "argmax", "temperature": 1} }},
		{"zero temperature", func(m map[string]any) {
			m["sampler"] = map[string]any{"mode": "argmax", "temperature": 0, "rng": SamplerRNG}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := writeArtifact(t, base, make([]float32, parameterCount(base)))
			path := filepath.Join(directory, "metadata.json")
			data, _ := os.ReadFile(path)
			var fields map[string]any
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			tc.edit(fields)
			data, _ = json.Marshal(fields)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(directory); err == nil {
				t.Fatal("malformed metadata loaded successfully")
			}
		})
	}
	for _, extra := range []string{`,"schemaVersion":1}`, `} {}`, `,"sampler":{"mode":"argmax","mode":"categorical","temperature":1,"rng":"xorshift64-v1"}}`} {
		directory := writeArtifact(t, base, make([]float32, parameterCount(base)))
		path := filepath.Join(directory, "metadata.json")
		data, _ := os.ReadFile(path)
		data = append(data[:len(data)-1], []byte(extra)...)
		_ = os.WriteFile(path, data, 0600)
		if _, err := Load(directory); err == nil {
			t.Fatalf("duplicate/trailing metadata accepted: %q", extra)
		}
	}
}

func TestRejectBadWeightArtifacts(t *testing.T) {
	metadata := fixtureMetadata([]int{64, 32})
	for _, test := range []string{"truncated", "extra", "checksum", "nonfinite", "oversize", "metadata oversize", "symlink"} {
		t.Run(test, func(t *testing.T) {
			parameters := make([]float32, parameterCount(metadata))
			if test == "nonfinite" {
				parameters[55] = float32(math.Inf(1))
			}
			directory := writeArtifact(t, metadata, parameters)
			path := filepath.Join(directory, "weights.bin")
			data, _ := os.ReadFile(path)
			switch test {
			case "truncated":
				data = data[:len(data)-4]
			case "extra":
				data = append(data, 0, 0, 0, 0)
			case "checksum":
				data[0] ^= 1
			case "oversize":
				data = make([]byte, MaxWeightsBytes+1)
			case "metadata oversize":
				_ = os.WriteFile(filepath.Join(directory, "metadata.json"), []byte(strings.Repeat(" ", MaxMetadataBytes+1)), 0600)
			case "symlink":
				if err := os.Rename(path, filepath.Join(directory, "real.bin")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("real.bin", path); err != nil {
					t.Fatal(err)
				}
			}
			if test != "symlink" {
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Load(directory); err == nil {
				t.Fatalf("%s weights unexpectedly accepted", test)
			}
		})
	}
}

func BenchmarkDecideArgmax(b *testing.B)      { benchmarkDecide(b, "argmax") }
func BenchmarkDecideCategorical(b *testing.B) { benchmarkDecide(b, "categorical") }

func benchmarkDecide(b *testing.B, mode string) {
	model := loadTestModel(b, mode)
	observation := observationWithMask(0, 1, 2, 3, 4, 5, 6, 7, 8, 9)
	memory := game.PolicyMemory{Random: 42}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = model.Decide(observation, &memory)
	}
}

package training

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"BattleCityReforge/internal/game"
	"BattleCityReforge/internal/policymlp"
)

type countingPolicy struct{ calls atomic.Int64 }

func (*countingPolicy) Name() string { return "counting-scripted" }
func (p *countingPolicy) Decide(o game.Observation, memory *game.PolicyMemory) game.Action {
	p.calls.Add(1)
	return (game.ScriptedPolicy{}).Decide(o, memory)
}

func TestInjectedPolicySurvivesBothResetForms(t *testing.T) {
	policy := &countingPolicy{}
	batch := NewBatchWithPolicy(policy)
	defer batch.Close()
	configs := []Config{{Seed: 91, MaxSteps: 100}, {LevelID: 2, Seed: 92, MaxSteps: 100}}
	if _, err := batch.Reset(configs); err != nil {
		t.Fatal(err)
	}
	advance := func() {
		t.Helper()
		before := policy.calls.Load()
		for range 20 {
			if _, err := batch.Step([]Actions{baselineActions(), baselineActions()}); err != nil {
				t.Fatal(err)
			}
		}
		if policy.calls.Load() <= before {
			t.Fatal("installed policy was not executed for native -1 actions")
		}
	}
	advance()
	if _, err := batch.ResetIndices([]int{0}, configs[:1]); err != nil {
		t.Fatal(err)
	}
	advance()
	if _, err := batch.Reset(configs); err != nil {
		t.Fatal(err)
	}
	advance()
	before := policy.calls.Load()
	for range 5 {
		// Explicit action indices retain their original override semantics.
		if _, err := batch.Step([]Actions{{Defender: -1}, {Defender: -1}}); err != nil {
			t.Fatal(err)
		}
	}
	if policy.calls.Load() != before {
		t.Fatal("explicit enemy actions unexpectedly invoked the native policy")
	}
}

func nativeFixture(t *testing.T) *policymlp.Model {
	t.Helper()
	parameters := (320+1)*32 + (32+1)*16 + (16+1)*10
	weights := make([]byte, parameters*4)
	digest := sha256.Sum256(weights)
	metadata := policymlp.Metadata{
		SchemaVersion: 1, ModelName: "native-evaluation-test", PolicyVersion: "v1",
		Difficulty: "trainee", TrainingSeed: 7, Trained: false,
		ObservationVersion: 2, NormalizationVersion: 2, InputSize: 320,
		HiddenSizes: []int{32, 16}, OutputSize: 10, Activation: "relu",
		ActionEncoding: "move*2+fire", DecisionHz: 10, ObservationDelayDecisions: 2,
		WeightsFile: "weights.bin", WeightsFormat: "float32-le",
		WeightsLayout: "out-in-row-major-weights-then-bias", WeightsSHA256: hex.EncodeToString(digest[:]),
		Sampler: policymlp.Sampler{Mode: "categorical", Temperature: 1, RNG: "xorshift64-v1"},
	}
	directory := t.TempDir()
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string][]byte{"metadata.json": data, "weights.bin": weights} {
		if err := os.WriteFile(filepath.Join(directory, name), value, 0600); err != nil {
			t.Fatal(err)
		}
	}
	model, err := policymlp.Load(directory)
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func TestNativeCategoricalModelHasReproduciblePerTankRNG(t *testing.T) {
	model := nativeFixture(t)
	batch := NewBatchWithPolicy(model)
	defer batch.Close()
	configs := []Config{{LevelID: 3, Seed: -700003, MaxSteps: 200}, {LevelID: 9, Seed: -700009, MaxSteps: 200}}
	if _, err := batch.Reset(configs); err != nil {
		t.Fatal(err)
	}
	want := make([][]Transition, 100)
	for i := range want {
		var err error
		want[i], err = batch.Step([]Actions{baselineActions(), baselineActions()})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Both reset paths must reconstruct production spawn RNG and policy memory.
	for _, selective := range []bool{false, true} {
		var err error
		if selective {
			_, err = batch.ResetIndices([]int{0, 1}, configs)
		} else {
			_, err = batch.Reset(configs)
		}
		if err != nil {
			t.Fatal(err)
		}
		for i := range want {
			got, err := batch.Step([]Actions{baselineActions(), baselineActions()})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want[i]) {
				t.Fatalf("native categorical replay differs at step %d selective=%v", i, selective)
			}
		}
	}
}

func TestNativeHelloIdentityAndLegacyCompatibility(t *testing.T) {
	model := nativeFixture(t)
	var output bytes.Buffer
	if err := ServeWithEnemyModel(strings.NewReader("{\"cmd\":\"close\"}\n"), &output, model); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(&output)
	hello := readResponse(t, reader)
	if hello.EnemyPolicy == nil || hello.EnemyPolicy.Implementation != "go-policymlp" ||
		!reflect.DeepEqual(hello.EnemyPolicy.Metadata, model.Metadata()) {
		t.Fatalf("native hello does not identify the loaded artifact: %+v", hello.EnemyPolicy)
	}
	if closed := readResponse(t, reader); closed.Type != "closed" {
		t.Fatal("native worker did not close")
	}
	commands := "{\"cmd\":\"reset\",\"configs\":[{\"seed\":13,\"max_steps\":1}]}\n" +
		"{\"cmd\":\"step\",\"enemy_actions\":[[-1,-1,-1,-1,-1]],\"defender_actions\":[-1]}\n{\"cmd\":\"close\"}\n"
	var old, nilModel bytes.Buffer
	if err := Serve(strings.NewReader(commands), &old); err != nil {
		t.Fatal(err)
	}
	if err := ServeWithEnemyModel(strings.NewReader(commands), &nilModel, nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(old.Bytes(), nilModel.Bytes()) || bytes.Contains(old.Bytes(), []byte("enemy_policy")) {
		t.Fatal("nil native model changed the legacy wire protocol")
	}
}

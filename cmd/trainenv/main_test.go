package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"BattleCityReforge/internal/policymlp"
)

func writeCLIArtifact(t *testing.T, trained bool) string {
	t.Helper()
	weights := make([]byte, ((320+1)*32+(32+1)*16+(16+1)*10)*4)
	digest := sha256.Sum256(weights)
	metadata := policymlp.Metadata{
		SchemaVersion: 1, ModelName: "trainenv-cli-test", PolicyVersion: "v1", Difficulty: "expert",
		TrainingSeed: 1, Trained: trained, ObservationVersion: 2, NormalizationVersion: 2,
		InputSize: 320, HiddenSizes: []int{32, 16}, OutputSize: 10, Activation: "relu",
		ActionEncoding: "move*2+fire", DecisionHz: 10, ObservationDelayDecisions: 2,
		WeightsFile: "weights.bin", WeightsFormat: "float32-le", WeightsLayout: "out-in-row-major-weights-then-bias",
		WeightsSHA256: hex.EncodeToString(digest[:]), Sampler: policymlp.Sampler{Mode: "categorical", Temperature: .5, RNG: "xorshift64-v1"},
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
	return directory
}

func TestNativeCLIRejectsBeforeHello(t *testing.T) {
	for name, args := range map[string][]string{
		"missing":   {"-enemy-model", filepath.Join(t.TempDir(), "missing")},
		"untrained": {"-enemy-model", writeCLIArtifact(t, false)},
		"unknown":   {"-surprise"}, "positional": {"unexpected"},
	} {
		t.Run(name, func(t *testing.T) {
			var output, diagnostics bytes.Buffer
			if err := run(args, strings.NewReader(""), &output, &diagnostics); err == nil || output.Len() != 0 {
				t.Fatalf("invalid invocation emitted hello or succeeded: %v %q", err, output.String())
			}
		})
	}
}

func TestNativeCLIUsesArtifactSampler(t *testing.T) {
	var output, diagnostics bytes.Buffer
	if err := run([]string{"-enemy-model", writeCLIArtifact(t, true)}, strings.NewReader("{\"cmd\":\"close\"}\n"), &output, &diagnostics); err != nil {
		t.Fatal(err)
	}
	line := bytes.SplitN(output.Bytes(), []byte("\n"), 2)[0]
	var hello struct {
		EnemyPolicy struct {
			Metadata policymlp.Metadata `json:"metadata"`
		} `json:"enemy_policy"`
	}
	if err := json.Unmarshal(line, &hello); err != nil {
		t.Fatal(err)
	}
	if hello.EnemyPolicy.Metadata.Sampler.Mode != "categorical" || hello.EnemyPolicy.Metadata.Sampler.Temperature != .5 {
		t.Fatal("artifact sampler was not preserved")
	}
	if diagnostics.Len() != 0 {
		t.Fatalf("successful invocation emitted diagnostics: %q", diagnostics.String())
	}
}

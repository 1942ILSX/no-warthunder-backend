package difficulty

import (
	"BattleCityReforge/internal/policymlp"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestProfilesAndIntegerScoring(t *testing.T) {
	for _, test := range []struct {
		id          ID
		base, score int
	}{{Trainee, 101, 101}, {Expert, 101, 151}, {Ace, 101, 202}, {Expert, 0, 0}} {
		profile, ok := Parse(string(test.id))
		if !ok || profile.ApplyScore(test.base) != test.score {
			t.Fatalf("%s score for %d: got %d, want %d", test.id, test.base, profile.ApplyScore(test.base), test.score)
		}
	}
	for _, invalid := range []string{"", "Expert", "expert ", "nightmare"} {
		if _, ok := Parse(invalid); ok {
			t.Fatalf("accepted %q", invalid)
		}
	}
	profiles := All()
	profiles[0].ScoreMultiplier = 99
	if All()[0].ScoreMultiplier != 1 {
		t.Fatal("profile list is mutable shared state")
	}
}

func TestQualifiedArtifactsLoad(t *testing.T) {
	policies, err := LoadPolicies("../../models")
	if err != nil {
		t.Fatal(err)
	}
	if len(policies) != 3 {
		t.Fatalf("loaded %d models", len(policies))
	}
	for _, profile := range All() {
		metadata := policies[profile.ID].(*policymlp.Model).Metadata()
		if metadata.Difficulty != string(profile.ID) || metadata.PolicyVersion != qualified[profile.ID].version {
			t.Fatalf("wrong model: %+v", metadata)
		}
	}
}

func TestRejectUnqualifiedArtifacts(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"untrained", func(m map[string]any) { m["trained"] = false }},
		{"wrong-tier", func(m map[string]any) { m["difficulty"] = "ace" }},
		{"old-version", func(m map[string]any) { m["policyVersion"] = "nwt-trainee-v2" }},
		{"argmax", func(m map[string]any) { m["sampler"].(map[string]any)["mode"] = "argmax" }},
		{"changed-temperature", func(m map[string]any) { m["sampler"].(map[string]any)["temperature"] = .3 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			for _, profile := range All() {
				directory := filepath.Join(root, string(profile.ID))
				if err := os.Mkdir(directory, 0700); err != nil {
					t.Fatal(err)
				}
				for _, file := range []string{"metadata.json", "weights.bin"} {
					data, err := os.ReadFile(filepath.Join("../../models", string(profile.ID), file))
					if err != nil {
						t.Fatal(err)
					}
					if file == "metadata.json" && profile.ID == Trainee {
						var metadata map[string]any
						if err := json.Unmarshal(data, &metadata); err != nil {
							t.Fatal(err)
						}
						test.edit(metadata)
						data, err = json.Marshal(metadata)
						if err != nil {
							t.Fatal(err)
						}
					}
					if err := os.WriteFile(filepath.Join(directory, file), data, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := LoadPolicies(root); err == nil {
				t.Fatal("accepted unqualified artifact")
			}
		})
	}
	if _, err := LoadPolicies(t.TempDir()); err == nil {
		t.Fatal("accepted missing models")
	}
}

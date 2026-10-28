package leaderboard

import (
	"BattleCityReforge/internal/difficulty"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestTopTenOrderPersistenceAndIsolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scores.json")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	for i := range 25 {
		if _, _, _, err := s.Submit(1, fmt.Sprintf("player-%d", i), 100, int64(25000-i*100), at.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	got := s.Entries(1)
	if len(got) != 10 || got[0].ID != "player-24" || got[9].ID != "player-15" {
		t.Fatalf("wrong top ten: %+v", got)
	}
	if rank, recorded, _, err := s.Submit(1, "slow", 99, 1, at); err != nil || recorded || rank != 0 {
		t.Fatalf("lower score must lose: %d %v %v", rank, recorded, err)
	}
	if rank, _, _, err := s.Submit(1, "high", 101, 999999, at); err != nil || rank != 1 {
		t.Fatalf("score precedes speed: %d %v", rank, err)
	}
	if len(s.Entries(2)) != 0 {
		t.Fatal("boards must be independent")
	}
	if _, _, _, err := s.Submit(2, "same-id", 10, 3000, at); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.Submit(2, "same-id", 10, 2000, at); err != nil {
		t.Fatal(err)
	}
	loaded, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Entries(1)) != 10 || len(loaded.Entries(2)) != 2 {
		t.Fatal("persistent top ten changed")
	}
	data, _ := os.ReadFile(path)
	if len(data) > 20000 {
		t.Fatal("unexpected history accumulation")
	}
}

func TestConcurrentSubmissionsBounded(t *testing.T) {
	s, _ := New(filepath.Join(t.TempDir(), "scores.json"))
	var wg sync.WaitGroup
	for i := range 30 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, _, _, err := s.Submit(1, "pilot", i, 1000, time.Now()); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	got := s.Entries(1)
	if len(got) != 10 || got[0].Score != 29 || got[9].Score != 20 {
		t.Fatalf("wrong concurrent board: %+v", got)
	}
}

func TestInvalidIDsAndCorruptFilePreserved(t *testing.T) {
	for _, id := range []string{"", "   ", "<script>", "a\nb", "a/b", "abcdefghijklmnopqrstu"} {
		if _, err := ValidateID(id); err == nil {
			t.Errorf("accepted invalid ID %q", id)
		}
	}
	for _, id := range []string{"pilot", "玩家 01", "ace._-"} {
		if _, err := ValidateID(id); err != nil {
			t.Errorf("rejected %q: %v", id, err)
		}
	}
	path := filepath.Join(t.TempDir(), "scores.json")
	os.WriteFile(path, []byte("broken"), 0600)
	if _, err := New(path); err == nil {
		t.Fatal("expected corruption error")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "broken" {
		t.Fatal("corrupt original overwritten")
	}
}

func TestFailedPersistenceDoesNotPublishRecord(t *testing.T) {
	dir := t.TempDir()
	parentFile := filepath.Join(dir, "not-a-directory")
	os.WriteFile(parentFile, []byte("x"), 0600)
	s := &Store{path: filepath.Join(parentFile, "scores.json"), levels: make(map[int][]Record)}
	if _, _, _, err := s.Submit(1, "pilot", 100, 1000, time.Now()); err == nil {
		t.Fatal("expected storage error")
	}
	if len(s.Entries(1)) != 0 {
		t.Fatal("failed write became visible")
	}
}

func TestDifficultyMetadataAndLegacyRecordsShareOneTopTen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scores.json")
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, _, _, err := store.Submit(1, "legacy", 9999, 1000, now); err != nil {
		t.Fatal(err)
	}
	for index := range 18 {
		profile := difficulty.All()[index%3]
		base := 100 + index
		metadata := RunMetadata{Difficulty: profile.ID, ScoreMultiplier: profile.ScoreMultiplier, BaseScore: base}
		if _, _, _, err := store.Submit(1, fmt.Sprintf("run-%d", index), profile.ApplyScore(base), int64(2000+index), now, metadata); err != nil {
			t.Fatal(err)
		}
	}
	// Equal weighted scores from different tiers are ordered by speedrun time.
	if _, _, _, err := store.Submit(1, "slow-ace", 600, 4000, now, RunMetadata{Difficulty: difficulty.Ace, ScoreMultiplier: 2, BaseScore: 300}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.Submit(1, "fast-expert", 600, 3000, now, RunMetadata{Difficulty: difficulty.Expert, ScoreMultiplier: 1.5, BaseScore: 400}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	records := reloaded.Entries(1)
	if len(records) != Limit || records[0].ID != "legacy" || records[1].ID != "fast-expert" || records[2].ID != "slow-ace" {
		t.Fatalf("wrong mixed top ten: %+v", records)
	}
	if records[0].RunMetadata != (RunMetadata{}) {
		t.Fatal("legacy record relabeled as a trained tier")
	}
	if records[1].BaseScore != 400 || records[1].Difficulty != difficulty.Expert || records[1].ScoreMultiplier != 1.5 {
		t.Fatalf("metadata lost: %+v", records[1])
	}
	data, err := json.Marshal(records[0])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("difficulty")) || bytes.Contains(data, []byte("scoreMultiplier")) {
		t.Fatalf("legacy metadata fabricated: %s", data)
	}
}

func TestInvalidDifficultyMetadataCannotBeStoredOrLoaded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scores.json")
	store, _ := New(path)
	for _, metadata := range []RunMetadata{
		{Difficulty: difficulty.Ace, ScoreMultiplier: 99, BaseScore: 100},
		{Difficulty: difficulty.Expert, ScoreMultiplier: 1.5, BaseScore: 100},
		{Difficulty: "unknown", ScoreMultiplier: 2, BaseScore: 100},
		{BaseScore: 100},
	} {
		if _, _, _, err := store.Submit(1, "pilot", 200, 1000, time.Now(), metadata); err == nil {
			t.Fatalf("accepted inconsistent metadata: %+v", metadata)
		}
	}
	if len(store.Entries(1)) != 0 {
		t.Fatal("invalid metadata published")
	}
	invalid := []byte(`{"version":1,"levels":{"1":[{"id":"pilot","score":200,"durationMs":1000,"completedAt":"2026-10-28T00:00:00Z","difficulty":"expert","scoreMultiplier":1.5,"baseScore":100}]}}`)
	if err := os.WriteFile(path, invalid, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(path); err == nil {
		t.Fatal("loaded inconsistent score metadata")
	}
	unchanged, _ := os.ReadFile(path)
	if !bytes.Equal(unchanged, invalid) {
		t.Fatal("corrupt original overwritten")
	}
}

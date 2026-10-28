package leaderboard

import (
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

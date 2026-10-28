// Package leaderboard persists only the best ten completed runs for each level.
// No player accounts, abandoned runs, or score history are stored.
package leaderboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const Limit = 10

type Record struct {
	ID          string `json:"id"`
	Score       int    `json:"score"`
	DurationMS  int64  `json:"durationMs"`
	CompletedAt string `json:"completedAt"`
}

type diskData struct {
	Version int              `json:"version"`
	Levels  map[int][]Record `json:"levels"`
}

type Store struct {
	mu     sync.RWMutex
	path   string
	levels map[int][]Record
}

func New(path string) (*Store, error) {
	s := &Store{path: path, levels: make(map[int][]Record)}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var disk diskData
	decoder := json.NewDecoder(io.LimitReader(f, 1<<20))
	if err := decoder.Decode(&disk); err != nil {
		return nil, fmt.Errorf("read leaderboard (original file preserved): %w", err)
	}
	if disk.Version != 1 {
		return nil, fmt.Errorf("unsupported leaderboard version %d", disk.Version)
	}
	for level, records := range disk.Levels {
		if level < 1 || level > 10 || len(records) > Limit {
			return nil, errors.New("leaderboard file exceeds the 10-level / 10-record contract")
		}
		for _, r := range records {
			id, err := ValidateID(r.ID)
			if err != nil || id != r.ID || r.Score < 0 || r.DurationMS <= 0 {
				return nil, errors.New("invalid leaderboard record; original file preserved")
			}
			if _, err := time.Parse(time.RFC3339Nano, r.CompletedAt); err != nil {
				return nil, errors.New("invalid leaderboard timestamp; original file preserved")
			}
		}
		sort.SliceStable(records, func(i, j int) bool { return better(records[i], records[j]) })
		s.levels[level] = records
	}
	return s, nil
}

func ValidateID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if !utf8.ValidString(id) || utf8.RuneCountInString(id) < 1 || utf8.RuneCountInString(id) > 20 {
		return "", errors.New("ID must contain 1–20 characters")
	}
	for _, r := range id {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != ' ' && r != '_' && r != '-' && r != '.' {
			return "", errors.New("ID may contain letters, numbers, spaces, underscores, hyphens and periods")
		}
	}
	return id, nil
}

func better(a, b Record) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	if a.DurationMS != b.DurationMS {
		return a.DurationMS < b.DurationMS
	}
	ta, _ := time.Parse(time.RFC3339Nano, a.CompletedAt)
	tb, _ := time.Parse(time.RFC3339Nano, b.CompletedAt)
	return ta.Before(tb)
}

func (s *Store) Entries(level int) []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Record{}, s.levels[level]...)
}

// Submit receives trusted score/time from the server's completion receipt, never
// from the HTTP body. rank=0 means the run did not enter the top ten. IDs may
// repeat because the leaderboard ranks runs, not unique players.
func (s *Store) Submit(level int, id string, score int, durationMS int64, completed time.Time) (rank int, recorded bool, entries []Record, err error) {
	id, err = ValidateID(id)
	if err != nil {
		return
	}
	if level < 1 || level > 10 || score < 0 || durationMS <= 0 {
		err = errors.New("invalid completed run")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	candidate := Record{ID: id, Score: score, DurationMS: durationMS, CompletedAt: completed.UTC().Format(time.RFC3339Nano)}
	current := s.levels[level]
	index := sort.Search(len(current), func(i int) bool { return better(candidate, current[i]) })
	if index >= Limit {
		return 0, false, append([]Record{}, current...), nil
	}
	next := make([]Record, 0, min(len(current)+1, Limit))
	next = append(next, current[:index]...)
	next = append(next, candidate)
	next = append(next, current[index:]...)
	if len(next) > Limit {
		next = next[:Limit]
	}
	updated := make(map[int][]Record, len(s.levels)+1)
	for key, value := range s.levels {
		updated[key] = value
	}
	updated[level] = next
	if err := s.persist(updated); err != nil {
		return 0, false, nil, err
	}
	s.levels = updated
	return index + 1, true, append([]Record{}, next...), nil
}

// Write a bounded replacement, fsync, then atomically rename. There is no append
// log or backup containing evicted records. One server process owns this file.
func (s *Store) persist(records map[int][]Record) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".leaderboard-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(diskData{Version: 1, Levels: records}); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	// Directory sync is best-effort: some platforms/filesystems do not support it.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

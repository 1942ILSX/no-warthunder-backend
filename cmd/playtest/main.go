// Command playtest starts an isolated loopback server with a temporary record
// file, then plays through the normal HTTP/WebSocket API using legal controls.
// It never connects to the user's :8080 server or modifies real leaderboard data.
// Usage: go run ./cmd/playtest
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"BattleCityReforge/internal/difficulty"
	"BattleCityReforge/internal/game"
	"BattleCityReforge/internal/leaderboard"
	"BattleCityReforge/internal/server"
	"github.com/gorilla/websocket"
)

const privateURL = "http://127.0.0.1:18083"

type finalResult struct {
	Status          string `json:"status"`
	LevelID         int    `json:"levelId"`
	Score           int    `json:"score"`
	DurationMS      int64  `json:"durationMs"`
	Kills           int    `json:"kills"`
	CompletionToken string `json:"completionToken"`
}

type serverMessage struct {
	Type   string      `json:"type"`
	State  game.State  `json:"state"`
	Result finalResult `json:"result"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "playtest:", err)
		os.Exit(1)
	}
}

func run() error {
	dir, err := os.MkdirTemp("", "battlecity-playtest-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "leaderboard.json")
	store, err := leaderboard.New(path)
	if err != nil {
		return err
	}
	policies, err := difficulty.LoadPolicies("models")
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:18083")
	if err != nil {
		return fmt.Errorf("private QA port unavailable (existing server untouched): %w", err)
	}
	app := server.New(server.Config{MaxRooms: 3, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Policies: policies}, store)
	httpServer := &http.Server{Handler: app.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = httpServer.Serve(listener) }()
	defer func() { app.Close(); _ = httpServer.Close() }()
	fmt.Println("Private QA server:127.0.0.1:18083; temporary records; up to3 real-time attempts")
	for attempt := 1; attempt <= 3; attempt++ {
		result, err := play(2 * time.Minute)
		if err != nil {
			return fmt.Errorf("attempt%d: %w", attempt, err)
		}
		fmt.Printf("attempt=%d status=%s kills=%d score=%d durationMs=%d\n", attempt, result.Status, result.Kills, result.Score, result.DurationMS)
		if result.Status != "won" {
			continue
		}
		if result.CompletionToken == "" {
			return errors.New("victory did not issue a completion receipt")
		}
		if err := verifyRecord(result, path); err != nil {
			return err
		}
		fmt.Println("PASS: real victory → optional ID submission → rank1 → exact server score/time on disk and GET; forged score rejected; receipt reuse rejected")
		return nil
	}
	return errors.New("scripted public-input defender did not win within three attempts; no result was forged")
}

func play(limit time.Duration) (finalResult, error) {
	var created struct {
		WSURL string `json:"wsUrl"`
	}
	status, err := request(http.MethodPost, "/api/rooms", map[string]any{"levelId": 1, "difficulty": difficulty.Trainee}, &created)
	if err != nil {
		return finalResult{}, err
	}
	if status != 201 {
		return finalResult{}, fmt.Errorf("create room HTTP%d", status)
	}
	conn, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:18083"+created.WSURL, nil)
	if err != nil {
		return finalResult{}, err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	messages := make(chan serverMessage, 4)
	errs := make(chan error, 1)
	go func() {
		for {
			var message serverMessage
			if err := conn.ReadJSON(&message); err != nil {
				select {
				case errs <- err:
				default:
				}
				return
			}
			select {
			case messages <- message:
			case <-ctx.Done():
				return
			}
		}
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	bot := legalDefender{}
	var state game.State
	ready := false
	for {
		select {
		case <-ctx.Done():
			return finalResult{Status: "timeout", Score: state.Score, Kills: state.Kills, DurationMS: state.TimeMS}, nil
		case message := <-messages:
			switch message.Type {
			case "welcome", "state":
				state = message.State
				ready = true
			case "finished":
				return message.Result, nil
			}
		case err := <-errs:
			// A normal close can race the buffered final event; consume it first.
			for len(messages) > 0 {
				message := <-messages
				if message.Type == "finished" {
					return message.Result, nil
				}
			}
			return finalResult{}, err
		case <-ticker.C:
			if !ready || state.Status != "playing" {
				continue
			}
			input := bot.input(state)
			_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
			if err := conn.WriteJSON(struct {
				Type string `json:"type"`
				game.Input
			}{Type: "input", Input: input}); err != nil {
				return finalResult{}, err
			}
		}
	}
}

func verifyRecord(result finalResult, path string) error {
	var ignored any
	status, err := request(http.MethodPost, "/api/leaderboard", map[string]any{"completionToken": result.CompletionToken, "id": "QA-Defender", "score": 999999}, &ignored)
	if err != nil {
		return err
	}
	if status != 400 {
		return fmt.Errorf("forged score must be rejected, HTTP%d", status)
	}
	body := map[string]any{"completionToken": result.CompletionToken, "id": "QA-Defender"}
	var submitted struct {
		Rank     int                  `json:"rank"`
		Recorded bool                 `json:"recorded"`
		Entries  []leaderboard.Record `json:"entries"`
	}
	status, err = request(http.MethodPost, "/api/leaderboard", body, &submitted)
	if err != nil {
		return err
	}
	if status != 200 || !submitted.Recorded || submitted.Rank != 1 || len(submitted.Entries) != 1 {
		return fmt.Errorf("unexpected submission response: HTTP%d %+v", status, submitted)
	}
	record := submitted.Entries[0]
	if record.ID != "QA-Defender" || record.Score != result.Score || record.DurationMS != result.DurationMS {
		return errors.New("stored record differs from server completion result")
	}
	var board struct {
		Entries []leaderboard.Record `json:"entries"`
	}
	status, err = request(http.MethodGet, "/api/leaderboard?levelId=1", nil, &board)
	if err != nil {
		return err
	}
	if status != 200 || len(board.Entries) != 1 || board.Entries[0] != record {
		return errors.New("public leaderboard does not match submitted record")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var disk struct {
		Version int                          `json:"version"`
		Levels  map[int][]leaderboard.Record `json:"levels"`
	}
	if err = json.Unmarshal(data, &disk); err != nil {
		return err
	}
	if disk.Version != 1 || len(disk.Levels) != 1 || len(disk.Levels[1]) != 1 || disk.Levels[1][0] != record {
		return errors.New("persisted replacement file does not match real completion")
	}
	status, err = request(http.MethodPost, "/api/leaderboard", body, &ignored)
	if err != nil {
		return err
	}
	if status != 410 {
		return fmt.Errorf("receipt reuse must be rejected, HTTP%d", status)
	}
	return nil
}

func request(method, path string, body any, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, privateURL+path, reader)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(out); err != nil {
		return response.StatusCode, err
	}
	return response.StatusCode, nil
}

package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"BattleCityReforge/internal/difficulty"
	"BattleCityReforge/internal/game"
	"BattleCityReforge/internal/leaderboard"
	"BattleCityReforge/internal/levels"
	"github.com/gorilla/websocket"
)

func testServer(t *testing.T, maxRooms int) (*Server, *httptest.Server) {
	t.Helper()
	store, err := leaderboard.New(filepath.Join(t.TempDir(), "scores.json"))
	if err != nil {
		t.Fatal(err)
	}
	policies := make(map[difficulty.ID]game.Policy)
	for _, profile := range difficulty.All() {
		policies[profile.ID] = game.ScriptedPolicy{}
	}
	app := New(Config{MaxRooms: maxRooms, AllowedOrigins: []string{"http://localhost:5173"}, Policies: policies}, store)
	ts := httptest.NewServer(app.Handler())
	t.Cleanup(func() { app.Close(); ts.Close() })
	return app, ts
}
func post(t *testing.T, address string, body any) (int, []byte) {
	t.Helper()
	data, _ := json.Marshal(body)
	response, err := http.Post(address, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ = io.ReadAll(response.Body)
	return response.StatusCode, data
}
func reserve(t *testing.T, ts *httptest.Server) map[string]string {
	t.Helper()
	code, data := post(t, ts.URL+"/api/rooms", map[string]any{"levelId": 1})
	if code != 201 {
		t.Fatalf("reserve=%d %s", code, data)
	}
	var reservation struct{ RoomID, Token, WSURL string }
	if err := json.Unmarshal(data, &reservation); err != nil {
		t.Fatal(err)
	}
	return map[string]string{"roomId": reservation.RoomID, "token": reservation.Token, "wsUrl": reservation.WSURL}
}

func TestErrorsExposeStableTranslationCodes(t *testing.T) {
	_, ts := testServer(t, 1)
	reserve(t, ts)
	for _, tc := range []struct {
		name, method, path, body, origin string
		status                           int
		code                             string
	}{
		{"invalid level", "POST", "/api/rooms", `{"levelId":11}`, "", 400, "invalid_level"},
		{"full rooms", "POST", "/api/rooms", `{"levelId":1}`, "", 503, "rooms_full"},
		{"invalid ID", "POST", "/api/leaderboard", `{"completionToken":"missing","id":"<script>"}`, "", 400, "invalid_id"},
		{"expired completion", "POST", "/api/leaderboard", `{"completionToken":"missing","id":"pilot"}`, "", 410, "completion_expired"},
		{"forbidden origin", "POST", "/api/rooms", `{"levelId":1}`, "https://untrusted.example", 403, "origin_not_allowed"},
		{"unknown endpoint", "GET", "/api/missing", "", "", 404, "not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, ts.URL+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			response, err := ts.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var body struct {
				Code  string `json:"code"`
				Error string `json:"error"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != tc.status || body.Code != tc.code || body.Error == "" {
				t.Fatalf("status=%d code=%q diagnostic=%q; want status=%d code=%q and a legacy diagnostic", response.StatusCode, body.Code, body.Error, tc.status, tc.code)
			}
		})
	}
}

func TestRoomInputAuthorityAndCleanup(t *testing.T) {
	app, ts := testServer(t, 1)
	reservation := reserve(t, ts)
	if code, _ := post(t, ts.URL+"/api/rooms", map[string]any{"levelId": 2}); code != 503 {
		t.Fatalf("capacity status %d", code)
	}
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + reservation["wsUrl"]
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var message struct {
		Type  string     `json:"type"`
		State game.State `json:"state"`
	}
	if err := conn.ReadJSON(&message); err != nil || message.Type != "welcome" {
		t.Fatalf("greeting: %s %v", message.Type, err)
	}
	startX := message.State.Player.X
	if err := conn.WriteJSON(map[string]any{"type": "input", "seq": 1, "move": 2, "fire": true, "score": 999999}); err != nil {
		t.Fatal(err)
	}
	moved := false
	for range 12 {
		if err := conn.ReadJSON(&message); err != nil {
			t.Fatal(err)
		}
		if message.Type == "state" && message.State.LastInputSeq == 1 && message.State.Player.X > startX {
			moved = true
			break
		}
	}
	if !moved {
		t.Fatal("valid input did not move authoritative player")
	}
	if message.State.Score >= 999999 {
		t.Fatal("client score became authoritative")
	}
	// Same sequence cannot change the accepted direction.
	conn.WriteJSON(map[string]any{"type": "input", "seq": 1, "move": 4, "fire": false})
	if _, response, err := websocket.DefaultDialer.Dial(wsURL, nil); err == nil || response.StatusCode != 409 {
		t.Fatal("room allowed second controller")
	}
	conn.Close()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		app.mu.Lock()
		n := len(app.rooms)
		app.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("disconnected room was retained")
}

func TestTokenOriginReservationAndMalformedInput(t *testing.T) {
	app, ts := testServer(t, 3)
	reservation := reserve(t, ts)
	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/rooms/" + reservation["roomId"] + "/ws"
	if _, r, err := websocket.DefaultDialer.Dial(wsBase+"?token=wrong", nil); err == nil || r.StatusCode != 404 {
		t.Fatal("accepted forged room token")
	}
	header := http.Header{"Origin": []string{"https://untrusted.example"}}
	if _, r, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+reservation["wsUrl"], header); err == nil || r.StatusCode != 403 {
		t.Fatal("accepted foreign origin")
	}
	app.mu.Lock()
	app.rooms[reservation["roomId"]].created = time.Now().Add(-time.Minute)
	app.mu.Unlock()
	if _, r, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+reservation["wsUrl"], nil); err == nil || r.StatusCode != 404 {
		t.Fatal("accepted expired reservation")
	}
	for _, body := range []any{map[string]any{"levelId": 11}, map[string]any{"levelId": 1, "score": 999}, map[string]any{"levelId": "1"}} {
		if code, _ := post(t, ts.URL+"/api/rooms", body); code != 400 {
			t.Fatalf("accepted invalid create: %v", body)
		}
	}
}

func TestCompletionIsOptInTrustedAndSingleUse(t *testing.T) {
	app, ts := testServer(t, 1)
	at := time.Now().UTC()
	// Receipt injection is test-only. Production issues these exclusively from
	// the game loop after an authoritative victory, never an HTTP endpoint.
	app.mu.Lock()
	app.receipts["trusted-test-receipt"] = &receipt{LevelID: 2, Score: 4200, DurationMS: 76543, CompletedAt: at, Expires: at.Add(time.Minute)}
	app.mu.Unlock()
	if len(app.store.Entries(2)) != 0 {
		t.Fatal("unsubmitted victory persisted")
	}
	if code, _ := post(t, ts.URL+"/api/leaderboard", map[string]any{"completionToken": "forged", "id": "pilot"}); code != 410 {
		t.Fatal("accepted forged completion")
	}
	if code, _ := post(t, ts.URL+"/api/leaderboard", map[string]any{"completionToken": "trusted-test-receipt", "id": "pilot", "score": 999999}); code != 400 {
		t.Fatal("accepted client score field")
	}
	if code, _ := post(t, ts.URL+"/api/leaderboard", map[string]any{"completionToken": "trusted-test-receipt", "id": "<script>"}); code != 400 {
		t.Fatal("accepted invalid display ID")
	}
	code, data := post(t, ts.URL+"/api/leaderboard", map[string]any{"completionToken": "trusted-test-receipt", "id": "玩家 01"})
	if code != 200 {
		t.Fatalf("submit: %d %s", code, data)
	}
	records := app.store.Entries(2)
	if len(records) != 1 || records[0].Score != 4200 || records[0].DurationMS != 76543 || records[0].ID != "玩家 01" {
		t.Fatalf("wrong authoritative record: %+v", records)
	}
	if code, _ := post(t, ts.URL+"/api/leaderboard", map[string]any{"completionToken": "trusted-test-receipt", "id": "again"}); code != 410 {
		t.Fatal("reused receipt")
	}
	if len(app.store.Entries(2)) != 1 {
		t.Fatal("duplicate record")
	}
}

func TestConcurrentTokenSubmissionOnlyOnce(t *testing.T) {
	app, ts := testServer(t, 1)
	now := time.Now()
	app.mu.Lock()
	app.receipts["once"] = &receipt{LevelID: 1, Score: 100, DurationMS: 1000, CompletedAt: now, Expires: now.Add(time.Minute)}
	app.mu.Unlock()
	var wg sync.WaitGroup
	codes := make(chan int, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _ := post(t, ts.URL+"/api/leaderboard", map[string]any{"completionToken": "once", "id": "pilot"})
			codes <- code
		}()
	}
	wg.Wait()
	close(codes)
	success := 0
	for code := range codes {
		if code == 200 {
			success++
		} else if code != 409 && code != 410 {
			t.Errorf("unexpected status %d", code)
		}
	}
	if success != 1 || len(app.store.Entries(1)) != 1 {
		t.Fatalf("receipt accepted %d times", success)
	}
}

func TestIdleInputStopsAndShutdownClosesSocket(t *testing.T) {
	app, ts := testServer(t, 1)
	reservation := reserve(t, ts)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+reservation["wsUrl"], nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var msg struct {
		Type  string     `json:"type"`
		State game.State `json:"state"`
	}
	conn.ReadJSON(&msg)
	conn.WriteJSON(map[string]any{"type": "input", "seq": 1, "move": 2, "fire": false})
	var lastX float64
	stopped := false
	for range 25 {
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type != "state" {
			continue
		}
		if msg.State.TimeMS > 800 {
			if lastX != 0 && msg.State.Player.X == lastX {
				stopped = true
				break
			}
			lastX = msg.State.Player.X
		}
	}
	if !stopped {
		t.Fatal("idle movement was not cleared")
	}
	closed := make(chan struct{})
	go func() { app.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown blocked on live websocket")
	}
}

func TestDifficultyCatalogValidationAndDefault(t *testing.T) {
	app, ts := testServer(t, 5)
	response, err := ts.Client().Get(ts.URL + "/api/difficulties")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var catalog struct {
		DefaultDifficulty difficulty.ID        `json:"defaultDifficulty"`
		Difficulties      []difficulty.Profile `json:"difficulties"`
	}
	if err := json.NewDecoder(response.Body).Decode(&catalog); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || catalog.DefaultDifficulty != difficulty.Expert || len(catalog.Difficulties) != 3 {
		t.Fatalf("wrong catalog: %+v", catalog)
	}
	reservation := reserve(t, ts)
	app.mu.Lock()
	rm := app.rooms[reservation["roomId"]]
	app.mu.Unlock()
	if rm.profile.ID != difficulty.Expert || rm.profile.ScoreMultiplier != 1.5 {
		t.Fatalf("wrong default profile: %+v", rm.profile)
	}
	for _, invalid := range []string{"", "Expert", "impossible", " ace "} {
		status, data := post(t, ts.URL+"/api/rooms", map[string]any{"levelId": 1, "difficulty": invalid})
		if status != 400 || !bytes.Contains(data, []byte(`"code":"invalid_difficulty"`)) {
			t.Fatalf("accepted invalid difficulty %q: %d %s", invalid, status, data)
		}
	}
}

func TestMissingPolicyNeverStartsScriptedRoom(t *testing.T) {
	store, _ := leaderboard.New(filepath.Join(t.TempDir(), "scores.json"))
	app := New(Config{}, store)
	ts := httptest.NewServer(app.Handler())
	defer ts.Close()
	defer app.Close()
	status, data := post(t, ts.URL+"/api/rooms", map[string]any{"levelId": 1, "difficulty": "ace"})
	if status != 503 || !bytes.Contains(data, []byte(`"code":"policy_unavailable"`)) || len(app.rooms) != 0 {
		t.Fatalf("missing policy accepted: %d %s", status, data)
	}
}

type countingPolicy struct {
	name      string
	decisions atomic.Int64
}

func (p *countingPolicy) Name() string { return p.name }
func (p *countingPolicy) Decide(_ game.Observation, _ *game.PolicyMemory) game.Action {
	p.decisions.Add(1)
	return game.Action{}
}

func TestRoomUsesLockedSelectedPolicy(t *testing.T) {
	store, _ := leaderboard.New(filepath.Join(t.TempDir(), "scores.json"))
	selected := &countingPolicy{name: "selected-ace"}
	other := &countingPolicy{name: "other-expert"}
	policies := map[difficulty.ID]game.Policy{difficulty.Ace: selected, difficulty.Expert: other}
	app := New(Config{Policies: policies}, store)
	ts := httptest.NewServer(app.Handler())
	defer ts.Close()
	defer app.Close()
	// The caller cannot replace the server's policy map after construction.
	policies[difficulty.Ace] = other
	status, data := post(t, ts.URL+"/api/rooms", map[string]any{"levelId": 1, "difficulty": "ace"})
	if status != 201 {
		t.Fatalf("create room: %d %s", status, data)
	}
	var reservation struct {
		WSURL           string        `json:"wsUrl"`
		Difficulty      difficulty.ID `json:"difficulty"`
		ScoreMultiplier float64       `json:"scoreMultiplier"`
		PolicyVersion   string        `json:"policyVersion"`
	}
	if err := json.Unmarshal(data, &reservation); err != nil {
		t.Fatal(err)
	}
	if reservation.Difficulty != difficulty.Ace || reservation.ScoreMultiplier != 2 || reservation.PolicyVersion != selected.Name() {
		t.Fatalf("wrong reserved policy: %+v", reservation)
	}
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+reservation.WSURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(4 * time.Second))
	var welcome struct {
		Type            string        `json:"type"`
		Difficulty      difficulty.ID `json:"difficulty"`
		ScoreMultiplier float64       `json:"scoreMultiplier"`
		PolicyVersion   string        `json:"policyVersion"`
		State           roomState     `json:"state"`
	}
	if err := conn.ReadJSON(&welcome); err != nil {
		t.Fatal(err)
	}
	if welcome.Type != "welcome" || welcome.Difficulty != difficulty.Ace || welcome.ScoreMultiplier != 2 || welcome.PolicyVersion != selected.Name() {
		t.Fatalf("wrong welcome policy: %+v", welcome)
	}
	if err := conn.WriteJSON(map[string]any{"type": "input", "seq": 1, "move": 0, "fire": false, "difficulty": "expert", "scoreMultiplier": 99}); err != nil {
		t.Fatal(err)
	}
	for selected.decisions.Load() == 0 {
		var message map[string]json.RawMessage
		if err := conn.ReadJSON(&message); err != nil {
			t.Fatal(err)
		}
	}
	if other.decisions.Load() != 0 {
		t.Fatal("client input changed selected policy")
	}
}

func TestWeightedSnapshotsResultsAndTrustedSubmission(t *testing.T) {
	app, ts := testServer(t, 1)
	for _, profile := range difficulty.All() {
		t.Run(string(profile.ID), func(t *testing.T) {
			level, _ := levels.Get(1)
			rm := &room{level: level, profile: profile, policyVersion: "test-" + string(profile.ID)}
			raw := game.State{Score: 101, Status: "won", Kills: 3}
			expected := map[difficulty.ID]int{difficulty.Trainee: 101, difficulty.Expert: 151, difficulty.Ace: 202}[profile.ID]
			for range 3 {
				data, err := json.Marshal(rm.state(raw))
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Count(data, []byte(`"score":`)) != 1 {
					t.Fatalf("duplicate score field: %s", data)
				}
				var decoded map[string]any
				if err := json.Unmarshal(data, &decoded); err != nil {
					t.Fatal(err)
				}
				if decoded["score"] != float64(expected) || decoded["baseScore"] != float64(101) {
					t.Fatalf("wrong weighted state: %s", data)
				}
			}
			if raw.Score != 101 {
				t.Fatal("transport changed raw simulation score")
			}
			final := rm.result(raw, 4321)
			if final.Score != expected || final.BaseScore != 101 || final.Difficulty != profile.ID || final.ScoreMultiplier != profile.ScoreMultiplier || final.PolicyVersion != rm.policyVersion {
				t.Fatalf("wrong final score: %+v", final)
			}
			now := time.Now()
			token := string(profile.ID)
			app.mu.Lock()
			app.receipts[token] = &receipt{LevelID: 1, Score: final.Score, DurationMS: final.DurationMS, CompletedAt: now, Expires: now.Add(time.Minute),
				RunMetadata: leaderboard.RunMetadata{Difficulty: profile.ID, ScoreMultiplier: profile.ScoreMultiplier, BaseScore: final.BaseScore}}
			app.mu.Unlock()
			for _, field := range []string{"score", "baseScore", "scoreMultiplier", "difficulty"} {
				status, _ := post(t, ts.URL+"/api/leaderboard", map[string]any{"completionToken": token, "id": token, field: 999})
				if status != 400 {
					t.Fatalf("accepted client %s", field)
				}
			}
			status, data := post(t, ts.URL+"/api/leaderboard", map[string]any{"completionToken": token, "id": token})
			if status != 200 {
				t.Fatalf("trusted submission: %d %s", status, data)
			}
			found := false
			for _, record := range app.store.Entries(1) {
				if record.ID == token {
					found = true
					if record.Score != expected || record.BaseScore != 101 || record.Difficulty != profile.ID || record.ScoreMultiplier != profile.ScoreMultiplier {
						t.Fatalf("wrong stored score: %+v", record)
					}
				}
			}
			if !found {
				t.Fatal("completed run missing")
			}
		})
	}
}

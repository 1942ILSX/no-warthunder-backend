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
	"testing"
	"time"

	"BattleCityReforge/internal/game"
	"BattleCityReforge/internal/leaderboard"
	"github.com/gorilla/websocket"
)

func testServer(t *testing.T, maxRooms int) (*Server, *httptest.Server) {
	t.Helper()
	store, err := leaderboard.New(filepath.Join(t.TempDir(), "scores.json"))
	if err != nil {
		t.Fatal(err)
	}
	app := New(Config{MaxRooms: maxRooms, AllowedOrigins: []string{"http://localhost:5173"}}, store)
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
	var reservation map[string]string
	if err := json.Unmarshal(data, &reservation); err != nil {
		t.Fatal(err)
	}
	return reservation
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

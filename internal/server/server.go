// Package server owns transport, bounded room lifetimes and completion receipts.
// Game state belongs to each room's goroutine; clients send inputs only.
package server

import (
	"BattleCityReforge/internal/difficulty"
	"BattleCityReforge/internal/game"
	"BattleCityReforge/internal/leaderboard"
	"BattleCityReforge/internal/levels"
	"BattleCityReforge/internal/policymlp"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gorilla/websocket"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	completionTTL  = 10 * time.Minute
	reservationTTL = 30 * time.Second
	roomTTL        = 15 * time.Minute
	maxReceipts    = 1024
)

type Config struct {
	MaxRooms       int
	AllowedOrigins []string
	FrontendDir    string
	Logger         *slog.Logger
	// Policies are loaded once at startup and shared across rooms. A missing
	// policy rejects room creation rather than silently changing its difficulty.
	Policies map[difficulty.ID]game.Policy
}
type Server struct {
	config   Config
	store    *leaderboard.Store
	mu       sync.Mutex
	rooms    map[string]*room
	receipts map[string]*receipt
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	upgrader websocket.Upgrader
}
type room struct {
	id, token     string
	level         levels.Level
	created       time.Time
	claimed       bool
	profile       difficulty.Profile
	policy        game.Policy
	policyVersion string
}
type receipt struct {
	LevelID              int
	Score                int
	DurationMS           int64
	CompletedAt, Expires time.Time
	Submitting           bool
	RunMetadata          leaderboard.RunMetadata
}
type result struct {
	Status          string        `json:"status"`
	LevelID         int           `json:"levelId"`
	Score           int           `json:"score"`
	DurationMS      int64         `json:"durationMs"`
	Kills           int           `json:"kills"`
	CompletionToken string        `json:"completionToken,omitempty"`
	Difficulty      difficulty.ID `json:"difficulty"`
	ScoreMultiplier float64       `json:"scoreMultiplier"`
	BaseScore       int           `json:"baseScore"`
	PolicyVersion   string        `json:"policyVersion"`
}

// Only transport scores are weighted. The simulation and training environment
// always retain their original base score, so snapshots cannot compound it.
type roomState struct {
	game.State
	BaseScore int `json:"baseScore"`
}

func (rm *room) state(state game.State) roomState {
	base := state.Score
	state.Score = rm.profile.ApplyScore(base)
	return roomState{State: state, BaseScore: base}
}

func (rm *room) result(state game.State, durationMS int64) result {
	return result{Status: state.Status, LevelID: rm.level.ID, Score: rm.profile.ApplyScore(state.Score),
		BaseScore: state.Score, DurationMS: max(durationMS, 1), Kills: state.Kills,
		Difficulty: rm.profile.ID, ScoreMultiplier: rm.profile.ScoreMultiplier, PolicyVersion: rm.policyVersion}
}

func New(config Config, store *leaderboard.Store) *Server {
	policies := make(map[difficulty.ID]game.Policy, len(config.Policies))
	for id, policy := range config.Policies {
		policies[id] = policy
	}
	config.Policies = policies
	if config.MaxRooms <= 0 {
		config.MaxRooms = 25
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{config: config, store: store, rooms: make(map[string]*room), receipts: make(map[string]*receipt), ctx: ctx, cancel: cancel}
	s.upgrader = websocket.Upgrader{ReadBufferSize: 1024, WriteBufferSize: 4096, HandshakeTimeout: 5 * time.Second, CheckOrigin: s.validOrigin}
	s.wg.Add(1)
	go s.sweep()
	return s
}
func (s *Server) Close() {
	// Serialize cancellation with connect's Add, so no room can be added
	// after Wait observes an empty group during shutdown.
	s.mu.Lock()
	s.cancel()
	s.mu.Unlock()
	s.wg.Wait()
}
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /api/difficulties", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"defaultDifficulty": difficulty.Default, "difficulties": difficulty.All()})
	})
	mux.HandleFunc("GET /api/levels", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"levels": levels.All()})
	})
	mux.HandleFunc("POST /api/rooms", s.createRoom)
	mux.HandleFunc("GET /api/rooms/{id}/ws", s.connect)
	mux.HandleFunc("GET /api/leaderboard", s.getBoard)
	mux.HandleFunc("POST /api/leaderboard", s.submit)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { fail(w, 404, "not_found", "API endpoint not found") })
	mux.HandleFunc("/", s.static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/healthz" {
			w.Header().Set("Cache-Control", "no-store")
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			if !s.validOrigin(r) {
				fail(w, 403, "origin_not_allowed", "Origin is not allowed")
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mux.ServeHTTP(w, r)
	})
}
func (s *Server) validOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	for _, allowed := range s.config.AllowedOrigins {
		if strings.TrimRight(origin, "/") == strings.TrimRight(allowed, "/") {
			return true
		}
	}
	return false
}
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	count := len(s.rooms)
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"status": "ok", "activeRooms": count, "maxRooms": s.config.MaxRooms})
}
func (s *Server) createRoom(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LevelID    int     `json:"levelId"`
		Difficulty *string `json:"difficulty"`
	}
	if err := readJSON(w, r, &body); err != nil {
		fail(w, 400, "invalid_level", "Expected a valid levelId")
		return
	}
	level, ok := levels.Get(body.LevelID)
	if !ok {
		fail(w, 400, "invalid_level", "Choose a level from 1 to 10")
		return
	}
	choice := string(difficulty.Default)
	if body.Difficulty != nil {
		choice = *body.Difficulty
	}
	profile, ok := difficulty.Parse(choice)
	if !ok {
		fail(w, 400, "invalid_difficulty", "Choose trainee, expert or ace")
		return
	}
	policy := s.config.Policies[profile.ID]
	if policy == nil {
		fail(w, 503, "policy_unavailable", "The selected difficulty model is unavailable")
		return
	}
	policyVersion := policy.Name()
	if model, ok := policy.(interface{ Metadata() policymlp.Metadata }); ok {
		policyVersion = model.Metadata().PolicyVersion
	}
	id, err := randomToken(12)
	if err != nil {
		fail(w, 500, "room_creation_failed", "Could not create room")
		return
	}
	token, err := randomToken(24)
	if err != nil {
		fail(w, 500, "room_creation_failed", "Could not create room")
		return
	}
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		fail(w, 503, "server_unavailable", "Server is shutting down")
		return
	}
	if len(s.rooms) >= s.config.MaxRooms {
		s.mu.Unlock()
		fail(w, 503, "rooms_full", "All rooms are occupied. Try again in a moment.")
		return
	}
	// Reserve room for every active run's eventual completion receipt. This
	// preserves the promised ten-minute submission window without eviction.
	if len(s.receipts)+len(s.rooms) >= maxReceipts {
		s.mu.Unlock()
		fail(w, 503, "run_capacity_full", "Run capacity is temporarily full. Please try again shortly.")
		return
	}
	s.rooms[id] = &room{id: id, token: token, level: level, created: time.Now(), profile: profile, policy: policy, policyVersion: policyVersion}
	s.mu.Unlock()
	writeJSON(w, 201, map[string]any{"roomId": id, "token": token, "wsUrl": "/api/rooms/" + id + "/ws?token=" + token,
		"difficulty": profile.ID, "scoreMultiplier": profile.ScoreMultiplier, "policyVersion": policyVersion})
}
func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	rm, ok := s.rooms[id]
	if !ok || time.Since(rm.created) > reservationTTL || subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("token")), []byte(rm.token)) != 1 {
		s.mu.Unlock()
		fail(w, 404, "room_expired", "Room expired or token is invalid")
		return
	}
	if rm.claimed {
		s.mu.Unlock()
		fail(w, 409, "room_in_use", "Room already has a player")
		return
	}
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		fail(w, 503, "server_unavailable", "Server is shutting down")
		return
	}
	rm.claimed = true
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()
	defer func() { s.mu.Lock(); delete(s.rooms, id); s.mu.Unlock() }()
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	s.runRoom(conn, rm)
}
func (s *Server) runRoom(conn *websocket.Conn, rm *room) {
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	inputs := make(chan game.Input, 32)
	readDone := make(chan struct{})
	conn.SetReadLimit(512)
	_ = conn.SetReadDeadline(time.Now().Add(45 * time.Second))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(45 * time.Second)) })
	go func() {
		defer close(readDone)
		var previous uint64
		window, count := time.Now(), 0
		for {
			var message struct {
				Type string `json:"type"`
				Seq  uint64 `json:"seq"`
				Move int    `json:"move"`
				Fire bool   `json:"fire"`
			}
			if err := conn.ReadJSON(&message); err != nil {
				return
			}
			if time.Since(window) >= time.Second {
				window = time.Now()
				count = 0
			}
			count++
			if count > 100 || message.Type != "input" || message.Move < 0 || message.Move > 4 {
				return
			}
			if message.Seq <= previous {
				continue
			}
			previous = message.Seq
			input := game.Input{Seq: message.Seq, Move: message.Move, Fire: message.Fire}
			select {
			case inputs <- input:
			case <-ctx.Done():
				return
			default:
			}
		}
	}()
	defer func() { cancel(); conn.Close(); <-readDone }()
	seed := int64(0)
	for _, c := range rm.id {
		seed = seed*31 + int64(c)
	}
	g := game.New(rm.level, seed)
	g.SetPolicy(rm.policy)
	started := time.Now()
	send := func(message any) bool {
		_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		return conn.WriteJSON(message) == nil
	}
	if !send(map[string]any{"type": "welcome", "roomId": rm.id, "level": rm.level, "state": rm.state(g.Snapshot()),
		"difficulty": rm.profile.ID, "scoreMultiplier": rm.profile.ScoreMultiplier, "policyVersion": rm.policyVersion}) {
		return
	}
	ticker := time.NewTicker(time.Second / 30)
	defer ticker.Stop()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	lastStep := started
	accumulator := 0.0
	lastInput := time.Now()
	var sequence uint64
	frames := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-readDone:
			return
		case input := <-inputs:
			g.SetInput(input)
			sequence = input.Seq
			lastInput = time.Now()
		case <-ping.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second)); err != nil {
				return
			}
		case now := <-ticker.C:
			if now.Sub(started) > roomTTL {
				state := g.Snapshot()
				state.Status = "lost"
				send(map[string]any{"type": "finished", "result": rm.result(state, now.Sub(started).Milliseconds())})
				return
			}
			if now.Sub(lastInput) > 500*time.Millisecond {
				g.SetInput(game.Input{Seq: sequence})
			}
			// Fixed steps preserve physics. Bounded catch-up avoids a spiral of death
			// under CPU quotas. Ranking always uses wall clock, never dropped steps.
			accumulator += min(now.Sub(lastStep).Seconds(), 0.2)
			lastStep = now
			steps := 0
			for accumulator >= 1.0/30.0 && steps < 6 {
				g.Step(1.0 / 30.0)
				accumulator -= 1.0 / 30.0
				steps++
			}
			frames++
			if frames%2 != 0 {
				continue
			}
			state := g.Snapshot()
			state.TimeMS = now.Sub(started).Milliseconds()
			if !send(map[string]any{"type": "state", "state": rm.state(state)}) {
				return
			}
			if state.Status != "playing" {
				final := rm.result(state, state.TimeMS)
				if state.Status == "won" {
					token, err := randomToken(32)
					if err == nil {
						s.mu.Lock()
						if len(s.receipts) < maxReceipts {
							s.receipts[token] = &receipt{LevelID: rm.level.ID, Score: final.Score, DurationMS: final.DurationMS, CompletedAt: now, Expires: now.Add(completionTTL),
								RunMetadata: leaderboard.RunMetadata{Difficulty: final.Difficulty, ScoreMultiplier: final.ScoreMultiplier, BaseScore: final.BaseScore}}
							final.CompletionToken = token
						}
						s.mu.Unlock()
					}
				}
				send(map[string]any{"type": "finished", "result": final})
				_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "Run complete"), time.Now().Add(time.Second))
				return
			}
		}
	}
}
func (s *Server) getBoard(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.URL.Query().Get("levelId"))
	if err != nil || id < 1 || id > 10 {
		fail(w, 400, "invalid_level", "Choose a level from 1 to 10")
		return
	}
	writeJSON(w, 200, map[string]any{"levelId": id, "entries": s.store.Entries(id)})
}
func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CompletionToken string `json:"completionToken"`
		ID              string `json:"id"`
	}
	if err := readJSON(w, r, &body); err != nil {
		fail(w, 400, "invalid_submission", "Expected completionToken and ID only")
		return
	}
	id, err := leaderboard.ValidateID(body.ID)
	if err != nil {
		fail(w, 400, "invalid_id", err.Error())
		return
	}
	s.mu.Lock()
	rc, ok := s.receipts[body.CompletionToken]
	if !ok || time.Now().After(rc.Expires) {
		s.mu.Unlock()
		fail(w, 410, "completion_expired", "Completion expired or was already submitted")
		return
	}
	if rc.Submitting {
		s.mu.Unlock()
		fail(w, 409, "submission_in_progress", "This completion is already being submitted")
		return
	}
	rc.Submitting = true
	s.mu.Unlock()
	rank, recorded, entries, err := s.store.Submit(rc.LevelID, id, rc.Score, rc.DurationMS, rc.CompletedAt, rc.RunMetadata)
	s.mu.Lock()
	if err != nil {
		rc.Submitting = false
	} else {
		delete(s.receipts, body.CompletionToken)
	}
	s.mu.Unlock()
	if err != nil {
		s.config.Logger.Error("leaderboard write failed", "error", err)
		fail(w, 500, "record_save_failed", "Record could not be saved. Please retry.")
		return
	}
	var displayedRank any
	if recorded {
		displayedRank = rank
	}
	writeJSON(w, 200, map[string]any{"rank": displayedRank, "recorded": recorded, "entries": entries})
}
func (s *Server) sweep() {
	defer s.wg.Done()
	timer := time.NewTicker(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-timer.C:
			s.mu.Lock()
			for id, rm := range s.rooms {
				if !rm.claimed && now.Sub(rm.created) > reservationTTL {
					delete(s.rooms, id)
				}
			}
			for token, rc := range s.receipts {
				if !rc.Submitting && now.After(rc.Expires) {
					delete(s.receipts, token)
				}
			}
			s.mu.Unlock()
		}
	}
}
func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		fail(w, 405, "method_not_allowed", "Method not allowed")
		return
	}
	if s.config.FrontendDir == "" {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, 200, map[string]string{"name": "NO WarThunder", "frontend": "Run the frontend dev server on http://localhost:5173"})
		return
	}
	if r.URL.Path == "/" {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, filepath.Join(s.config.FrontendDir, "index.html"))
		return
	}
	http.FileServer(http.Dir(s.config.FrontendDir)).ServeHTTP(w, r)
}
func randomToken(bytes int) (string, error) {
	data := make([]byte, bytes)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}
func readJSON(w http.ResponseWriter, r *http.Request, target any) error {
	defer r.Body.Close()
	if media := strings.Split(r.Header.Get("Content-Type"), ";")[0]; media != "application/json" {
		return errors.New("Content-Type must be application/json")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2048)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		return errors.New("expected one JSON object")
	}
	return nil
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// Error codes are stable protocol values. Clients translate them; the English
// diagnostic remains available for logs and older clients.
func fail(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "error": message})
}
func CheckFrontend(dir string) error {
	if dir == "" {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
		return fmt.Errorf("FRONTEND_DIR must contain a built index.html: %w", err)
	}
	return nil
}

package server

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"BattleCityReforge/internal/difficulty"
	"BattleCityReforge/internal/game"
	"BattleCityReforge/internal/leaderboard"
	"github.com/gorilla/websocket"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

func testWebTransportServer(t *testing.T) (*Server, *httptest.Server, string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost"},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	wt := &webtransport.Server{H3: &http3.Server{TLSConfig: http3.ConfigureTLSConfig(&tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: private}}}), QUICConfig: &quic.Config{Allow0RTT: false}},
		Config: &webtransport.Config{MaxIncomingStreams: 1, MaxIncomingUniStreams: -1, MaxIncomingData: 64 * 1024}}
	store, err := leaderboard.New(filepath.Join(t.TempDir(), "scores.json"))
	if err != nil {
		t.Fatal(err)
	}
	policies := map[difficulty.ID]game.Policy{}
	for _, p := range difficulty.All() {
		policies[p.ID] = game.ScriptedPolicy{}
	}
	app := New(Config{MaxRooms: 2, Policies: policies, WebTransport: wt, AssetBaseURL: "https://cdn.example/release/", OriginAssetRatio: 0.1}, store)
	wt.H3.Handler = app.Handler()
	ts := httptest.NewServer(app.Handler())
	done := make(chan struct{})
	go func() { defer close(done); _ = wt.Serve(packet) }()
	t.Cleanup(func() { app.Close(); _ = wt.Close(); _ = packet.Close(); ts.Close(); <-done })
	return app, ts, "https://" + packet.LocalAddr().String()
}

func dialWT(t *testing.T, address string) (*webtransport.Session, *webtransport.Transport) {
	t.Helper()
	dialer := &webtransport.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, session, err := dialer.Dial(ctx, address, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.CloseWithError(0, "test complete"); _ = dialer.Close() })
	return session, dialer
}

func TestWebTransportPreJoinFallbackAndAuthoritativeInput(t *testing.T) {
	app, ts, origin := testWebTransportServer(t)
	reservation := reserve(t, ts)
	path := "/api/rooms/" + reservation["roomId"] + "/wt?token=" + reservation["token"]
	session, _ := dialWT(t, origin+path)
	_ = session.CloseWithError(0, "choose fallback")
	// QUIC establishment never consumes a reservation: a legacy client can
	// immediately connect with the same token, even while WT cleanup runs.
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+reservation["wsUrl"], nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = ws.Close()
	reservation = reserve(t, ts)
	path = "/api/rooms/" + reservation["roomId"] + "/wt?token=" + reservation["token"]
	session, _ = dialWT(t, origin+path)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := session.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.SetDeadline(time.Now().Add(5 * time.Second))
	encoder := json.NewEncoder(stream)
	if err := encoder.Encode(map[string]any{"type": "join"}); err != nil {
		t.Fatal(err)
	}
	var welcome struct {
		Type            string
		State           game.State
		Difficulty      string
		ScoreMultiplier float64
	}
	if err := json.NewDecoder(stream).Decode(&welcome); err != nil || welcome.Type != "welcome" || welcome.Difficulty != "expert" || welcome.ScoreMultiplier != 1.5 {
		t.Fatalf("welcome=%+v err=%v", welcome, err)
	}
	if _, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+reservation["wsUrl"], nil); err == nil || response.StatusCode != 409 {
		t.Fatal("WebSocket acquired WT-owned room")
	}
	states, err := session.AcceptUniStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = states.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := encoder.Encode(map[string]any{"type": "input", "seq": 1, "move": 2, "fire": true, "score": 999999}); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(states)
	scanner.Buffer(make([]byte, 4096), 64*1024)
	moved := false
	for range 20 {
		if !scanner.Scan() {
			t.Fatalf("state: %v", scanner.Err())
		}
		var message struct {
			Type  string
			State game.State
		}
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			t.Fatal(err)
		}
		if message.State.Score >= 999999 {
			t.Fatal("client overrode score")
		}
		if message.State.LastInputSeq == 1 && message.State.Player.X > welcome.State.Player.X {
			moved = true
			break
		}
	}
	if !moved {
		t.Fatal("WT input did not move authoritative player")
	}
	_ = session.CloseWithError(0, "done")
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
	t.Fatal("disconnected rooms not reclaimed")
}

func TestClientAssetConfigurationAndUnsupportedTransport(t *testing.T) {
	_, ts, _ := testWebTransportServer(t)
	resp, err := http.Get(ts.URL + "/api/client-config")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var config struct {
		AssetBaseURL     string
		OriginAssetRatio float64
	}
	if err := json.NewDecoder(resp.Body).Decode(&config); err != nil {
		t.Fatal(err)
	}
	if config.AssetBaseURL != "https://cdn.example/release/" || config.OriginAssetRatio != 0.1 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("config=%+v headers=%v", config, resp.Header)
	}
	request, _ := http.NewRequest(http.MethodConnect, ts.URL+"/api/rooms/missing/wt", nil)
	response, err := ts.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 426 {
		t.Fatalf("non-H3 WT status=%d", response.StatusCode)
	}
}

func TestWebTransportSlowSnapshotReaderIsDisconnectedAndRoomReclaimed(t *testing.T) {
	app, ts, origin := testWebTransportServer(t)
	reservation := reserve(t, ts)
	path := "/api/rooms/" + reservation["roomId"] + "/wt?token=" + reservation["token"]
	// A deliberately tiny stream receive window creates real QUIC flow-control
	// backpressure. Reading welcome replenishes its window; refusing to read the
	// separate state stream must not retain an active room indefinitely.
	transport := &webtransport.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		QUICConfig: &quic.Config{
			EnableDatagrams: true, EnableStreamResetPartialDelivery: true,
			InitialStreamReceiveWindow: 1024, MaxStreamReceiveWindow: 1024,
			InitialConnectionReceiveWindow: 16 * 1024, MaxConnectionReceiveWindow: 16 * 1024,
			MaxIdleTimeout: 30 * time.Second,
		},
	}
	joining, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, session, err := transport.Dial(joining, origin+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.CloseWithError(0, "test complete"); _ = transport.Close() })
	control, err := session.OpenStreamSync(joining)
	if err != nil {
		t.Fatal(err)
	}
	_ = control.SetDeadline(time.Now().Add(5 * time.Second))
	encoder := json.NewEncoder(control)
	if err := encoder.Encode(map[string]string{"type": "join"}); err != nil {
		t.Fatal(err)
	}
	var welcome struct{ Type string }
	if err := json.NewDecoder(control).Decode(&welcome); err != nil || welcome.Type != "welcome" {
		t.Fatalf("welcome=%+v err=%v", welcome, err)
	}
	// Accepting the stream consumes only its transport header, not snapshots.
	// Leave its application payload completely unread throughout the test.
	if _, err := session.AcceptUniStream(joining); err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(6 * time.Second)
	defer deadline.Stop()
	var inputs uint64
waiting:
	for {
		select {
		case <-session.Context().Done():
			break waiting
		case <-deadline.C:
			t.Fatal("slow snapshot reader survived the write deadline")
		case <-ticker.C:
			inputs++
			_ = control.SetWriteDeadline(time.Now().Add(time.Second))
			if err := encoder.Encode(map[string]any{"type": "input", "seq": inputs, "move": 0, "fire": false}); err != nil {
				// A close can race this final input write. Require the session's
				// own close notification, then inspect the server error below.
				select {
				case <-session.Context().Done():
					break waiting
				case <-deadline.C:
					t.Fatalf("input failed without session cleanup: %v", err)
				}
			}
		}
	}
	if inputs < 10 {
		t.Fatalf("session closed before exercising sustained input: %d inputs", inputs)
	}
	_, err = session.OpenStream()
	var closed *webtransport.SessionError
	if !errors.As(err, &closed) || !closed.Remote || closed.ErrorCode != 1 {
		t.Fatalf("expected server slow-writer close, got %T: %v", err, err)
	}
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		app.mu.Lock()
		_, exists := app.rooms[reservation["roomId"]]
		app.mu.Unlock()
		if !exists {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("slow client's room was not reclaimed")
}

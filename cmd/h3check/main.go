// Command h3check makes a bounded, external full-stack probe of a deployment
// owned by its operator. It never submits leaderboard records. Run the client
// off the measured server so its CPU/RAM do not pollute server measurements.
package main

import (
	"BattleCityReforge/internal/difficulty"
	"BattleCityReforge/internal/game"
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

const maxMessage = 256 << 10

type paths []string

func (p *paths) String() string     { return strings.Join(*p, ",") }
func (p *paths) Set(s string) error { *p = append(*p, s); return nil }

type config struct {
	base      *url.URL
	transport string
	rooms     int
	duration  time.Duration
	insecure  bool
	httpOnly  bool
	output    string
	gets      paths
}

type httpCheck struct {
	Path         string `json:"path"`
	Status       int    `json:"status"`
	Protocol     string `json:"protocol"`
	PayloadBytes int    `json:"payloadBytes"`
	ContentType  string `json:"contentType,omitempty"`
	CacheControl string `json:"cacheControl,omitempty"`
	AltSvc       string `json:"altSvc,omitempty"`
}

type slotReport struct {
	Slot              int      `json:"slot"`
	Difficulty        string   `json:"difficulty"`
	Reservations      int      `json:"reservations"`
	Sessions          int      `json:"sessions"`
	Finished          int      `json:"finished"`
	Wins              int      `json:"wins"`
	Messages          int      `json:"messages"`
	Snapshots         int      `json:"snapshots"`
	Inputs            int      `json:"inputs"`
	AcknowledgedInput uint64   `json:"acknowledgedInput"`
	ReceivedBytes     int      `json:"receivedPayloadBytes"`
	SentBytes         int      `json:"sentPayloadBytes"`
	MaximumScore      int      `json:"maximumScore"`
	ActiveSeconds     float64  `json:"activeSeconds"`
	SnapshotHz        float64  `json:"snapshotHz"`
	SnapshotGapP99MS  float64  `json:"snapshotGapP99Ms"`
	SnapshotGapMaxMS  float64  `json:"snapshotGapMaxMs"`
	APIProtocol       string   `json:"apiProtocol,omitempty"`
	SessionProtocol   string   `json:"sessionProtocol,omitempty"`
	Errors            []string `json:"errors"`
	gaps              []float64
}

type report struct {
	Target                 string       `json:"target"`
	Transport              string       `json:"transport"`
	HTTPOnly               bool         `json:"httpOnly"`
	StartedUTC             string       `json:"startedUtc"`
	ElapsedSeconds         float64      `json:"elapsedSeconds"`
	RequestedRooms         int          `json:"requestedRooms"`
	PeakConnectedRooms     int          `json:"peakConnectedRooms"`
	SecondsAtTargetRooms   float64      `json:"secondsAtTargetRooms"`
	AverageConnectedRooms  float64      `json:"averageConnectedRooms"`
	Reservations           int          `json:"reservations"`
	SuccessfulSessions     int          `json:"successfulSessions"`
	Snapshots              int          `json:"snapshots"`
	Inputs                 int          `json:"inputs"`
	ReceivedBytes          int          `json:"receivedPayloadBytes"`
	SentBytes              int          `json:"sentPayloadBytes"`
	SnapshotsPerRoomSecond float64      `json:"snapshotsPerRoomSecond"`
	HTTPChecks             []httpCheck  `json:"httpChecks"`
	Slots                  []slotReport `json:"slots"`
	Errors                 []string     `json:"errors"`
	Note                   string       `json:"note"`
}

type occupancy struct {
	mu                       sync.Mutex
	active, peak, target     int
	last                     time.Time
	roomSeconds, fullSeconds float64
}

func (o *occupancy) change(delta int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := time.Now()
	seconds := now.Sub(o.last).Seconds()
	o.roomSeconds += float64(o.active) * seconds
	if o.active == o.target && o.target > 0 {
		o.fullSeconds += seconds
	}
	o.last = now
	o.active += delta
	o.peak = max(o.peak, o.active)
}

type reservation struct {
	RoomID     string  `json:"roomId"`
	WSURL      string  `json:"wsUrl"`
	WTPath     string  `json:"webTransportPath"`
	Difficulty string  `json:"difficulty"`
	Multiplier float64 `json:"scoreMultiplier"`
	Policy     string  `json:"policyVersion"`
}

type wireState struct {
	game.State
	BaseScore int `json:"baseScore"`
}
type wireResult struct {
	Status     string  `json:"status"`
	Difficulty string  `json:"difficulty"`
	Multiplier float64 `json:"scoreMultiplier"`
	Policy     string  `json:"policyVersion"`
	Score      int     `json:"score"`
	BaseScore  int     `json:"baseScore"`
}
type wireMessage struct {
	Type       string     `json:"type"`
	RoomID     string     `json:"roomId"`
	Difficulty string     `json:"difficulty"`
	Multiplier float64    `json:"scoreMultiplier"`
	Policy     string     `json:"policyVersion"`
	State      wireState  `json:"state"`
	Result     wireResult `json:"result"`
}
type event struct {
	data   []byte
	err    error
	source string
}
type wire struct {
	events   <-chan event
	send     func([]byte) error
	close    func()
	protocol string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "h3check:", err)
		os.Exit(1)
	}
}

func run() error {
	var c config
	address := flag.String("url", "https://127.0.0.1:18443", "origin of a deployment you own; HTTPS is required for WT")
	flag.StringVar(&c.transport, "transport", "wt", "wt (HTTP3 + WebTransport) or ws (HTTP/TCP + WebSocket)")
	flag.IntVar(&c.rooms, "rooms", 25, "concurrent rooms, 0–25; zero performs HTTP checks only")
	flag.DurationVar(&c.duration, "duration", 30*time.Second, "bounded gameplay duration, 1s–60s")
	flag.BoolVar(&c.insecure, "insecure", false, "trust a self-signed certificate on a loopback origin only")
	flag.BoolVar(&c.httpOnly, "http-only", false, "check only explicit -get paths; requires -rooms=0 (for CDN/static endpoints)")
	flag.StringVar(&c.output, "output", "", "optional JSON report file; also prints report to stdout")
	flag.Var(&c.gets, "get", "additional same-origin GET path to check (repeatable, max 4MiB each)")
	flag.Parse()
	var err error
	c.base, err = targetURL(*address, c.insecure)
	if err != nil {
		return err
	}
	if c.transport != "wt" && c.transport != "ws" {
		return errors.New("transport must be wt or ws")
	}
	if c.transport == "wt" && c.base.Scheme != "https" {
		return errors.New("WT requires an HTTPS origin")
	}
	if c.rooms < 0 || c.rooms > 25 || c.duration < time.Second || c.duration > time.Minute {
		return errors.New("require rooms=0–25 and duration=1s–60s")
	}
	for _, path := range c.gets {
		if _, err := endpoint(c.base, path, ""); err != nil {
			return err
		}
	}
	checkPaths, err := plannedChecks(c)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client, closeHTTP := httpClient(c)
	defer closeHTTP()
	started := time.Now()
	r := report{Target: c.base.String(), Transport: c.transport, HTTPOnly: c.httpOnly, StartedUTC: started.UTC().Format(time.RFC3339), RequestedRooms: c.rooms, Errors: []string{}, Slots: []slotReport{}, Note: "Client runs off-server. CPU/RAM must be measured separately using the server cgroup (including TLS/QUIC and kernel socket accounting). Payload bytes exclude HTTP/QUIC/TCP/TLS framing; API check bytes are reported separately. Each slot has its own HTTP connection pool and realtime connection. No leaderboard records are submitted. Failed unclaimed reservations expire server-side."}
	for _, path := range checkPaths {
		check, err := getCheck(ctx, c, client, path)
		r.HTTPChecks = append(r.HTTPChecks, check)
		if err != nil {
			r.Errors = append(r.Errors, redact(err.Error()))
			r.ElapsedSeconds = time.Since(started).Seconds()
			_ = emit(c, r)
			return err
		}
	}
	if c.rooms > 0 {
		loadStarted := time.Now()
		loadCtx, cancel := context.WithTimeout(ctx, c.duration)
		defer cancel()
		o := &occupancy{target: c.rooms, last: time.Now()}
		r.Slots = make([]slotReport, c.rooms)
		var wg sync.WaitGroup
		fmt.Fprintf(os.Stderr, "Bounded %s probe: %d rooms for %s; 20 inputs/s/room; no leaderboard submissions.\n", c.transport, c.rooms, c.duration)
		for i := 0; i < c.rooms; i++ {
			wg.Add(1)
			go func(slot int) { defer wg.Done(); r.Slots[slot] = worker(loadCtx, c, o, slot) }(i)
		}
		wg.Wait()
		o.change(0)
		r.PeakConnectedRooms = o.peak
		r.SecondsAtTargetRooms = o.fullSeconds
		loadElapsed := time.Since(loadStarted).Seconds()
		if loadElapsed > 0 {
			r.AverageConnectedRooms = o.roomSeconds / loadElapsed
		}
		for _, slot := range r.Slots {
			r.Reservations += slot.Reservations
			r.SuccessfulSessions += slot.Sessions
			r.Snapshots += slot.Snapshots
			r.Inputs += slot.Inputs
			r.ReceivedBytes += slot.ReceivedBytes
			r.SentBytes += slot.SentBytes
			for _, e := range slot.Errors {
				r.Errors = append(r.Errors, fmt.Sprintf("slot %d: %s", slot.Slot, e))
			}
		}
		if o.roomSeconds > 0 {
			r.SnapshotsPerRoomSecond = float64(r.Snapshots) / o.roomSeconds
		}
		if r.PeakConnectedRooms < c.rooms {
			r.Errors = append(r.Errors, fmt.Sprintf("peak connected rooms %d below requested %d", r.PeakConnectedRooms, c.rooms))
		}
		if r.Snapshots == 0 {
			r.Errors = append(r.Errors, "no snapshots received")
		}
	}
	r.ElapsedSeconds = time.Since(started).Seconds()
	if err := emit(c, r); err != nil {
		return err
	}
	if len(r.Errors) > 0 {
		return fmt.Errorf("probe failed with %d errors; see report", len(r.Errors))
	}
	return nil
}

func plannedChecks(c config) (paths, error) {
	if c.httpOnly {
		if c.rooms != 0 || len(c.gets) == 0 {
			return nil, errors.New("http-only requires rooms=0 and at least one explicit -get path")
		}
		return append(paths(nil), c.gets...), nil
	}
	return append(paths{"/healthz", "/api/difficulties"}, c.gets...), nil
}

func emit(c config, r report) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if c.output != "" {
		if err := os.WriteFile(c.output, b, 0600); err != nil {
			return err
		}
	}
	_, err = os.Stdout.Write(b)
	return err
}

func targetURL(address string, insecure bool) (*url.URL, error) {
	u, err := url.Parse(address)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("url must be an http(s) origin without credentials, path, query or fragment")
	}
	if insecure {
		host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, errors.New("insecure is restricted to loopback origins")
		}
	}
	u.Path = ""
	return u, nil
}

func endpoint(base *url.URL, path, suffix string) (string, error) {
	u, err := url.Parse(path)
	if err != nil || u.IsAbs() || u.Host != "" || u.User != nil || !strings.HasPrefix(u.Path, "/") || u.Fragment != "" {
		return "", errors.New("server endpoint must be a same-origin relative path")
	}
	if suffix != "" && (!strings.HasPrefix(u.Path, "/api/rooms/") || !strings.HasSuffix(u.Path, suffix) || u.Query().Get("token") == "") {
		return "", errors.New("invalid room transport endpoint")
	}
	return base.ResolveReference(u).String(), nil
}

func tlsConfig(c config) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: c.insecure}
} // only loopback accepts insecure

func httpClient(c config) (*http.Client, func()) {
	var rt http.RoundTripper
	var closeFn func()
	if c.transport == "wt" {
		t := &http3.Transport{TLSClientConfig: tlsConfig(c), MaxResponseHeaderBytes: 16384, QUICConfig: &quic.Config{HandshakeIdleTimeout: 5 * time.Second, MaxIdleTimeout: 20 * time.Second}}
		rt = t
		closeFn = func() { _ = t.Close() }
	} else {
		t := &http.Transport{TLSClientConfig: tlsConfig(c), ForceAttemptHTTP2: true, MaxIdleConnsPerHost: 2, IdleConnTimeout: 20 * time.Second}
		rt = t
		closeFn = t.CloseIdleConnections
	}
	return &http.Client{Transport: rt, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, closeFn
}

func getCheck(ctx context.Context, c config, client *http.Client, path string) (httpCheck, error) {
	check := httpCheck{Path: path}
	address, err := endpoint(c.base, path, "")
	if err != nil {
		return check, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	resp, err := client.Do(req)
	if err != nil {
		return check, err
	}
	defer resp.Body.Close()
	check.Status = resp.StatusCode
	check.Protocol = resp.Proto
	check.ContentType = resp.Header.Get("Content-Type")
	check.CacheControl = resp.Header.Get("Cache-Control")
	check.AltSvc = resp.Header.Get("Alt-Svc")
	body, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	check.PayloadBytes = len(body)
	if err != nil {
		return check, err
	}
	if len(body) > 4<<20 {
		return check, errors.New("HTTP check exceeds 4MiB bound")
	}
	if resp.StatusCode != 200 {
		return check, fmt.Errorf("GET %s returned HTTP %d", path, resp.StatusCode)
	}
	if c.transport == "wt" && resp.ProtoMajor != 3 {
		return check, fmt.Errorf("GET %s used %s instead of HTTP/3", path, resp.Proto)
	}
	return check, nil
}

func createRoom(ctx context.Context, c config, client *http.Client, slot, attempt int, s *slotReport) (reservation, error) {
	var room reservation
	body, _ := json.Marshal(map[string]any{"levelId": (slot+attempt)%10 + 1, "difficulty": s.Difficulty})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.base.String()+"/api/rooms", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return room, err
	}
	defer resp.Body.Close()
	s.APIProtocol = resp.Proto
	if c.transport == "wt" && resp.ProtoMajor != 3 {
		return room, errors.New("room API did not use HTTP/3")
	}
	if resp.StatusCode != 201 {
		return room, fmt.Errorf("room creation returned HTTP %d", resp.StatusCode)
	}
	s.Reservations++
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&room); err != nil {
		return room, err
	}
	if err := metadata(s.Difficulty, room.Difficulty, room.Multiplier, room.Policy); err != nil {
		return room, err
	}
	return room, nil
}

func worker(ctx context.Context, c config, o *occupancy, slot int) slotReport {
	s := slotReport{Slot: slot, Difficulty: []string{"trainee", "expert", "ace"}[slot%3], Errors: []string{}}
	client, closeHTTP := httpClient(c)
	defer closeHTTP()
	if !wait(ctx, time.Duration(slot)*10*time.Millisecond) {
		return s
	}
	for attempt := 0; attempt < 20 && ctx.Err() == nil; attempt++ {
		room, err := createRoom(ctx, c, client, slot, attempt, &s)
		if err != nil {
			if ctx.Err() == nil {
				s.Errors = append(s.Errors, redact(err.Error()))
			}
			break
		}
		finished, err := play(ctx, c, o, room, &s)
		if err != nil {
			if ctx.Err() == nil {
				s.Errors = append(s.Errors, redact(err.Error()))
			}
			break
		}
		if !finished || !wait(ctx, 150*time.Millisecond) {
			break
		}
	}
	if s.ActiveSeconds > 0 {
		s.SnapshotHz = float64(s.Snapshots) / s.ActiveSeconds
	}
	if s.ActiveSeconds >= 2 && s.SnapshotHz < 12 {
		s.Errors = append(s.Errors, fmt.Sprintf("snapshot cadence %.2f Hz below 12 Hz minimum (target 15 Hz)", s.SnapshotHz))
	}
	if s.ActiveSeconds >= 2 && s.AcknowledgedInput == 0 {
		s.Errors = append(s.Errors, "no player input was acknowledged")
	}
	if len(s.gaps) > 0 {
		slices.Sort(s.gaps)
		s.SnapshotGapP99MS = s.gaps[min(len(s.gaps)-1, int(math.Ceil(float64(len(s.gaps))*.99))-1)]
		s.SnapshotGapMaxMS = s.gaps[len(s.gaps)-1]
	}
	return s
}

func play(ctx context.Context, c config, o *occupancy, room reservation, s *slotReport) (bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w, err := connect(ctx, c, room)
	if err != nil {
		return false, err
	}
	defer w.close()
	s.SessionProtocol = w.protocol
	if c.transport == "wt" {
		s.SentBytes += len(`{"type":"join"}`)
		if w.protocol != "h3" {
			return false, fmt.Errorf("WebTransport negotiated %q instead of h3", w.protocol)
		}
	}
	ready := false
	var current game.State
	var lastTick uint64
	var lastSnapshot time.Time
	var connected time.Time
	bot := legalDefender{}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	handshake := time.NewTimer(6 * time.Second)
	defer handshake.Stop()
	defer func() {
		if ready {
			o.change(-1)
			s.ActiveSeconds += time.Since(connected).Seconds()
		}
	}()
	var streamFailure <-chan time.Time
	var pendingError error
	for {
		select {
		case <-ctx.Done():
			return false, nil
		case <-handshake.C:
			if !ready {
				return false, errors.New("welcome timed out")
			}
		case <-streamFailure:
			return false, pendingError
		case e := <-w.events:
			if e.err != nil {
				// EOF on the state stream may arrive before a finished control
				// message on the other stream. Give already-in-flight control a
				// short bounded opportunity to arrive before reporting failure.
				if streamFailure == nil {
					pendingError = fmt.Errorf("%s: %w", e.source, e.err)
					streamFailure = time.After(200 * time.Millisecond)
				}
				continue
			}
			s.Messages++
			s.ReceivedBytes += len(e.data)
			var m wireMessage
			if err := json.Unmarshal(e.data, &m); err != nil {
				return false, err
			}
			switch m.Type {
			case "welcome":
				if ready {
					return false, errors.New("duplicate welcome")
				}
				if m.RoomID != room.RoomID {
					return false, errors.New("welcome room mismatch")
				}
				if err := metadata(s.Difficulty, m.Difficulty, m.Multiplier, m.Policy); err != nil {
					return false, err
				}
				if err := score(s.Difficulty, m.State.Score, m.State.BaseScore); err != nil {
					return false, err
				}
				current = m.State.State
				ready = true
				connected = time.Now()
				o.change(1)
				s.Sessions++
				handshake.Stop()
			case "state":
				if !ready {
					return false, errors.New("state arrived before welcome")
				}
				if err := score(s.Difficulty, m.State.Score, m.State.BaseScore); err != nil {
					return false, err
				}
				if m.State.Tick < lastTick {
					return false, errors.New("state tick regressed")
				}
				lastTick = m.State.Tick
				current = m.State.State
				now := time.Now()
				if !lastSnapshot.IsZero() {
					s.gaps = append(s.gaps, float64(now.Sub(lastSnapshot))/float64(time.Millisecond))
				}
				lastSnapshot = now
				s.Snapshots++
				s.MaximumScore = max(s.MaximumScore, current.Score)
				s.AcknowledgedInput = max(s.AcknowledgedInput, current.LastInputSeq)
			case "finished":
				if !ready {
					return false, errors.New("finished arrived before welcome")
				}
				if err := metadata(s.Difficulty, m.Result.Difficulty, m.Result.Multiplier, m.Result.Policy); err != nil {
					return false, err
				}
				if err := score(s.Difficulty, m.Result.Score, m.Result.BaseScore); err != nil {
					return false, err
				}
				if m.Result.Status != "won" && m.Result.Status != "lost" {
					return false, errors.New("invalid result status")
				}
				s.Finished++
				if m.Result.Status == "won" {
					s.Wins++
				}
				if c.transport == "wt" {
					ack := []byte(`{"type":"finishedAck"}`)
					if err := w.send(ack); err != nil {
						return false, err
					}
					s.SentBytes += len(ack)
				}
				return true, nil
			default:
				return false, fmt.Errorf("unexpected message type %q", m.Type)
			}
		case <-ticker.C:
			if !ready || current.Status != "playing" {
				continue
			}
			// The defender reads only browser-visible state and emits legal
			// controls. It cannot alter the simulation or trained policy.
			if len(current.Grid) != game.BoardSize {
				return false, errors.New("invalid board size")
			}
			for _, row := range current.Grid {
				if len(row) != game.BoardSize {
					return false, errors.New("invalid board row size")
				}
			}
			input := bot.input(current)
			data, _ := json.Marshal(struct {
				Type string `json:"type"`
				game.Input
			}{"input", input})
			if err := w.send(data); err != nil {
				return false, err
			}
			s.Inputs++
			s.SentBytes += len(data)
		}
	}
}

func metadata(want, actual string, multiplier float64, policy string) error {
	p, ok := difficulty.Parse(want)
	if !ok || actual != want || multiplier != p.ScoreMultiplier || policy != "nwt-"+want+"-v3" {
		return errors.New("difficulty, score multiplier or qualified policy version mismatch")
	}
	return nil
}
func score(tier string, weighted, base int) error {
	p, ok := difficulty.Parse(tier)
	if !ok || base < 0 || weighted != p.ApplyScore(base) {
		return errors.New("weighted score differs from floor(baseScore × difficulty multiplier)")
	}
	return nil
}

func connect(ctx context.Context, c config, room reservation) (wire, error) {
	if c.transport == "ws" {
		return connectWS(ctx, c, room)
	}
	address, err := endpoint(c.base, room.WTPath, "/wt")
	if err != nil {
		return wire{}, err
	}
	transport := &webtransport.Transport{TLSClientConfig: tlsConfig(c), QUICConfig: &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true, HandshakeIdleTimeout: 5 * time.Second, MaxIdleTimeout: 15 * time.Second, MaxIncomingStreams: 4, MaxIncomingUniStreams: 8, InitialStreamReceiveWindow: 128 << 10, MaxStreamReceiveWindow: 256 << 10, InitialConnectionReceiveWindow: 512 << 10, MaxConnectionReceiveWindow: 1 << 20}}
	// webtransport-go passes the URL authority straight to its QUIC dialer.
	// Supply HTTPS's default port only to the network dial, preserving the
	// request Host and Origin authority (including whether :443 was explicit).
	transport.DialAddr = func(ctx context.Context, addr string, tlsConfig *tls.Config, quicConfig *quic.Config) (*quic.Conn, error) {
		return quic.DialAddrEarly(ctx, defaultHTTPSPort(addr), tlsConfig, quicConfig)
	}
	connecting, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	resp, session, err := transport.Dial(connecting, address, http.Header{"Origin": []string{c.base.String()}})
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		_ = transport.Close()
		return wire{}, err
	}
	stream, err := session.OpenStreamSync(connecting)
	if err != nil {
		_ = session.CloseWithError(0, "probe setup failed")
		_ = transport.Close()
		return wire{}, err
	}
	_ = stream.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err = stream.Write([]byte("{\"type\":\"join\"}\n")); err != nil {
		_ = session.CloseWithError(0, "probe setup failed")
		_ = transport.Close()
		return wire{}, err
	}
	events := make(chan event, 16)
	readerCtx, stopReaders := context.WithCancel(ctx)
	var readers sync.WaitGroup
	readers.Add(2)
	controlFirst := make(chan struct{})
	go func() { defer readers.Done(); readLines(readerCtx, stream, "control", events, controlFirst) }()
	go func() {
		defer readers.Done()
		// QUIC orders bytes within a stream, not between streams. Queue the
		// first control frame before reading snapshots so a welcome cannot
		// be overtaken solely by client goroutine scheduling.
		select {
		case <-controlFirst:
		case <-readerCtx.Done():
			return
		}
		incoming, err := session.AcceptUniStream(readerCtx)
		if err != nil {
			deliver(readerCtx, events, event{err: err, source: "state"})
			return
		}
		readLines(readerCtx, incoming, "state", events, nil)
	}()
	stopClose := context.AfterFunc(ctx, func() { _ = session.CloseWithError(0, "probe duration complete") })
	return wire{events: events, protocol: session.SessionState().ConnectionState.TLS.NegotiatedProtocol, send: func(data []byte) error {
		_ = stream.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_, err := stream.Write(append(data, '\n'))
		return err
	}, close: func() {
		stopClose()
		stopReaders()
		_ = session.CloseWithError(0, "probe complete")
		readers.Wait()
		_ = transport.Close()
	}}, nil
}

func defaultHTTPSPort(authority string) string {
	host, port, err := net.SplitHostPort(authority)
	if err == nil {
		if port != "" {
			return authority
		}
		return net.JoinHostPort(host, "443")
	}
	return net.JoinHostPort(strings.Trim(authority, "[]"), "443")
}

func connectWS(ctx context.Context, c config, room reservation) (wire, error) {
	address, err := endpoint(c.base, room.WSURL, "/ws")
	if err != nil {
		return wire{}, err
	}
	u, _ := url.Parse(address)
	u.Scheme = "ws"
	if c.base.Scheme == "https" {
		u.Scheme = "wss"
	}
	d := websocket.Dialer{HandshakeTimeout: 6 * time.Second, TLSClientConfig: tlsConfig(c), ReadBufferSize: 4096, WriteBufferSize: 1024}
	conn, resp, err := d.DialContext(ctx, u.String(), http.Header{"Origin": []string{c.base.String()}})
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return wire{}, err
	}
	conn.SetReadLimit(maxMessage)
	events := make(chan event, 16)
	readerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, data, err := conn.ReadMessage()
			if !deliver(readerCtx, events, event{data: data, err: err, source: "websocket"}) || err != nil {
				return
			}
		}
	}()
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	return wire{events: events, protocol: "websocket/HTTP/1.1", send: func(data []byte) error {
		_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		return conn.WriteMessage(websocket.TextMessage, data)
	}, close: func() {
		stopClose()
		cancel()
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "probe complete"), time.Now().Add(100*time.Millisecond))
		_ = conn.Close()
		<-done
	}}, nil
}

func readLines(ctx context.Context, r io.Reader, source string, out chan<- event, first chan struct{}) {
	opened := false
	open := func() {
		if first != nil && !opened {
			close(first)
			opened = true
		}
	}
	defer open()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), maxMessage)
	for scanner.Scan() {
		data := append([]byte(nil), scanner.Bytes()...)
		if !deliver(ctx, out, event{data: data, source: source}) {
			return
		}
		open()
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	deliver(ctx, out, event{err: err, source: source})
}
func deliver(ctx context.Context, out chan<- event, e event) bool {
	select {
	case out <- e:
		return true
	case <-ctx.Done():
		return false
	}
}
func wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

var tokenPattern = regexp.MustCompile(`([?&]token=)[^\s&"']+`)

func redact(s string) string { return tokenPattern.ReplaceAllString(s, "${1}<redacted>") }

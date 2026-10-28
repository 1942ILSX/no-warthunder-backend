// Command loadtest exercises real REST room creation and WebSocket gameplay.
// It only accepts loopback targets. To test a deployment you own, forward its
// port to localhost. It never creates leaderboard records or submits an ID.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

type configuration struct {
	base     *url.URL
	rooms    int
	duration time.Duration
	rate     int
}

type counters struct {
	apiErrors        atomic.Int64
	capacityRetries  atomic.Int64
	connectionErrors atomic.Int64
	protocolErrors   atomic.Int64
	messages         atomic.Int64
	snapshots        atomic.Int64
	receivedBytes    atomic.Int64
	inputs           atomic.Int64
	sentBytes        atomic.Int64
	wins             atomic.Int64
	losses           atomic.Int64
	restarts         atomic.Int64
	mu               sync.Mutex
	started          time.Time
	lastChange       time.Time
	active           int
	peak             int
	sessions         int
	roomSeconds      float64
	fullSeconds      float64
	sessionCount     int
	sessionSum       time.Duration
	sessionMin       time.Duration
	sessionMax       time.Duration
}

type report struct {
	Target                  string  `json:"target"`
	TargetRooms             int     `json:"targetRooms"`
	ElapsedSeconds          float64 `json:"elapsedSeconds"`
	InputsPerSecondPerRoom  int     `json:"inputsPerSecondPerRoom"`
	PeakConnectedRooms      int     `json:"peakConnectedRooms"`
	CurrentConnectedRooms   int     `json:"currentConnectedRooms"`
	AverageConnectedRooms   float64 `json:"averageConnectedRooms"`
	SecondsAtTargetRooms    float64 `json:"secondsAtTargetRooms"`
	SuccessfulSessions      int     `json:"successfulSessions"`
	Restarts                int64   `json:"restarts"`
	Wins                    int64   `json:"wins"`
	Losses                  int64   `json:"losses"`
	APIErrors               int64   `json:"apiErrors"`
	CapacityRetries         int64   `json:"capacityRetries"`
	ConnectionErrors        int64   `json:"connectionErrors"`
	ProtocolErrors          int64   `json:"protocolErrors"`
	ReceivedMessages        int64   `json:"receivedMessages"`
	Snapshots               int64   `json:"snapshots"`
	ReceivedPayloadBytes    int64   `json:"receivedPayloadBytes"`
	SentInputs              int64   `json:"sentInputs"`
	SentPayloadBytes        int64   `json:"sentPayloadBytes"`
	SnapshotsPerRoomSecond  float64 `json:"snapshotsPerRoomSecond"`
	SessionDurationMeanMS   float64 `json:"sessionDurationMeanMs"`
	SessionDurationMinMS    float64 `json:"sessionDurationMinMs"`
	SessionDurationMaxMS    float64 `json:"sessionDurationMaxMs"`
	ResourceMeasurementNote string  `json:"resourceMeasurementNote"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		os.Exit(1)
	}
}

func run() error {
	address := flag.String("url", "http://127.0.0.1:8080", "loopback backend URL (forward a user-owned remote server to localhost)")
	rooms := flag.Int("rooms", 25, "target concurrent rooms, 1–25")
	duration := flag.Duration("duration", time.Minute, "test duration, 1s–30m")
	rate := flag.Int("rate", 15, "input messages per second per room, 10–20")
	flag.Parse()
	base, err := loopbackURL(*address)
	if err != nil {
		return err
	}
	if *rooms < 1 || *rooms > 25 || *duration < time.Second || *duration > 30*time.Minute || *rate < 10 || *rate > 20 {
		return errors.New("require rooms=1–25, duration=1s–30m, and rate=10–20")
	}
	config := configuration{base, *rooms, *duration, *rate}
	signalContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(signalContext, config.duration)
	defer cancel()
	transport := &http.Transport{MaxIdleConns: config.rooms * 2, MaxIdleConnsPerHost: config.rooms, IdleConnTimeout: 15 * time.Second}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	defer transport.CloseIdleConnections()
	now := time.Now()
	stats := &counters{started: now, lastChange: now}
	fmt.Fprintf(os.Stderr, "Target %s; %d rooms; %s; %d inputs/s/room. No leaderboard submissions.\n", base, config.rooms, config.duration, config.rate)
	var workers sync.WaitGroup
	for i := 0; i < config.rooms; i++ {
		workers.Add(1)
		go func(slot int) {
			defer workers.Done()
			roomWorker(ctx, config, client, stats, slot)
		}(i)
	}
	progressDone := make(chan struct{})
	go func() {
		defer close(progressDone)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				snapshot := stats.snapshot(config)
				fmt.Fprintf(os.Stderr, "%.0fs active=%d/%d peak=%d snapshots=%d errors(api/ws/protocol)=%d/%d/%d capacity-retries=%d\n",
					snapshot.ElapsedSeconds, snapshot.CurrentConnectedRooms, config.rooms, snapshot.PeakConnectedRooms, snapshot.Snapshots,
					snapshot.APIErrors, snapshot.ConnectionErrors, snapshot.ProtocolErrors, snapshot.CapacityRetries)
			}
		}
	}()
	workers.Wait()
	cancel()
	<-progressDone
	final := stats.snapshot(config)
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(final); err != nil {
		return err
	}
	if final.SuccessfulSessions == 0 {
		return errors.New("no gameplay sessions connected; inspect the API/connection counters and server logs")
	}
	return nil
}

func loopbackURL(address string) (*url.URL, error) {
	u, err := url.Parse(address)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("url must be an http(s) loopback origin without credentials, path, query or fragment")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	addressIP := net.ParseIP(host)
	if host != "localhost" && (addressIP == nil || !addressIP.IsLoopback()) {
		return nil, errors.New("only loopback targets are accepted; use a local port forward for a deployment you own")
	}
	u.Path = ""
	return u, nil
}

func roomWorker(ctx context.Context, config configuration, client *http.Client, stats *counters, slot int) {
	if !wait(ctx, time.Duration(slot)*5*time.Millisecond) {
		return
	}
	attempt := 0
	for ctx.Err() == nil {
		levelID := (slot+attempt)%10 + 1
		attempt++
		endpoint, err := createRoom(ctx, config, client, stats, levelID)
		if err != nil {
			if !wait(ctx, 300*time.Millisecond+time.Duration(slot)*7*time.Millisecond) {
				return
			}
			continue
		}
		connected := playSession(ctx, config, stats, endpoint, slot)
		if ctx.Err() != nil {
			return
		}
		if connected {
			stats.restarts.Add(1)
		}
		// Allow the server's close/defer cleanup to release the previous room.
		if !wait(ctx, 120*time.Millisecond+time.Duration(slot)*3*time.Millisecond) {
			return
		}
	}
}

func createRoom(ctx context.Context, config configuration, client *http.Client, stats *counters, levelID int) (string, error) {
	data := []byte(fmt.Sprintf(`{"levelId":%d}`, levelID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, config.base.String()+"/api/rooms", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			stats.apiErrors.Add(1)
		}
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		if response.StatusCode == http.StatusServiceUnavailable {
			stats.capacityRetries.Add(1)
		} else {
			stats.apiErrors.Add(1)
		}
		return "", fmt.Errorf("room creation returned %d", response.StatusCode)
	}
	var room struct {
		WSURL string `json:"wsUrl"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&room); err != nil {
		stats.protocolErrors.Add(1)
		return "", err
	}
	path, err := url.Parse(room.WSURL)
	if err != nil || path.IsAbs() || path.Host != "" || !strings.HasPrefix(path.Path, "/api/rooms/") || !strings.HasSuffix(path.Path, "/ws") {
		stats.protocolErrors.Add(1)
		return "", errors.New("server returned an invalid relative WebSocket endpoint")
	}
	endpoint := config.base.ResolveReference(path)
	endpoint.Scheme = "ws"
	if config.base.Scheme == "https" {
		endpoint.Scheme = "wss"
	}
	return endpoint.String(), nil
}

type serverMessage struct {
	Type   string `json:"type"`
	Result struct {
		Status string `json:"status"`
	} `json:"result"`
}

func receive(conn *websocket.Conn, stats *counters) (serverMessage, error) {
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		return serverMessage{}, err
	}
	stats.messages.Add(1)
	stats.receivedBytes.Add(int64(len(data)))
	var message serverMessage
	if err := json.Unmarshal(data, &message); err != nil {
		stats.protocolErrors.Add(1)
		return message, err
	}
	if message.Type == "state" {
		stats.snapshots.Add(1)
	}
	return message, nil
}

func playSession(ctx context.Context, config configuration, stats *counters, endpoint string, slot int) bool {
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second, ReadBufferSize: 1024, WriteBufferSize: 1024}
	conn, response, err := dialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		if ctx.Err() == nil {
			stats.connectionErrors.Add(1)
		}
		return false
	}
	defer conn.Close()
	conn.SetReadLimit(256 << 10)
	// Cancellation must also interrupt the initial welcome read, not only the
	// gameplay loop. Close is safe concurrently with the single reader/writer.
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	welcome, err := receive(conn, stats)
	if err != nil || welcome.Type != "welcome" {
		if ctx.Err() == nil {
			if err != nil {
				stats.connectionErrors.Add(1)
			} else {
				stats.protocolErrors.Add(1)
			}
		}
		return false
	}
	started := time.Now()
	stats.connected(config.rooms)
	defer stats.disconnected(config.rooms, started)
	readResult := make(chan error, 1)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			message, err := receive(conn, stats)
			if err != nil {
				readResult <- err
				return
			}
			if message.Type == "finished" {
				switch message.Result.Status {
				case "won":
					stats.wins.Add(1)
				case "lost":
					stats.losses.Add(1)
				default:
					stats.protocolErrors.Add(1)
				}
				readResult <- nil
				return
			}
		}
	}()
	defer func() {
		_ = conn.Close()
		<-readDone
	}()
	ticker := time.NewTicker(time.Second / time.Duration(config.rate))
	defer ticker.Stop()
	var sequence uint64
	for {
		select {
		case <-ctx.Done():
			return true
		case err := <-readResult:
			if err != nil && ctx.Err() == nil {
				stats.connectionErrors.Add(1)
			}
			return true
		case <-ticker.C:
			sequence++
			input := struct {
				Type string `json:"type"`
				Seq  uint64 `json:"seq"`
				Move int    `json:"move"`
				Fire bool   `json:"fire"`
			}{"input", sequence, (int(sequence/7) + slot) % 5, sequence%5 != 0}
			data, _ := json.Marshal(input)
			_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
				if ctx.Err() == nil {
					stats.connectionErrors.Add(1)
				}
				return true
			}
			stats.inputs.Add(1)
			stats.sentBytes.Add(int64(len(data)))
		}
	}
}

func (stats *counters) integrate(now time.Time, target int) {
	seconds := now.Sub(stats.lastChange).Seconds()
	stats.roomSeconds += float64(stats.active) * seconds
	if stats.active == target {
		stats.fullSeconds += seconds
	}
	stats.lastChange = now
}

func (stats *counters) connected(target int) {
	stats.mu.Lock()
	defer stats.mu.Unlock()
	stats.integrate(time.Now(), target)
	stats.active++
	stats.sessions++
	stats.peak = max(stats.peak, stats.active)
}

func (stats *counters) disconnected(target int, started time.Time) {
	stats.mu.Lock()
	defer stats.mu.Unlock()
	now := time.Now()
	duration := now.Sub(started)
	stats.integrate(now, target)
	stats.active--
	stats.sessionCount++
	stats.sessionSum += duration
	if stats.sessionCount == 1 || duration < stats.sessionMin {
		stats.sessionMin = duration
	}
	stats.sessionMax = max(stats.sessionMax, duration)
}

func (stats *counters) snapshot(config configuration) report {
	stats.mu.Lock()
	defer stats.mu.Unlock()
	now := time.Now()
	stats.integrate(now, config.rooms)
	elapsed := now.Sub(stats.started).Seconds()
	result := report{
		Target: config.base.String(), TargetRooms: config.rooms, ElapsedSeconds: elapsed, InputsPerSecondPerRoom: config.rate,
		PeakConnectedRooms: stats.peak, CurrentConnectedRooms: stats.active, SuccessfulSessions: stats.sessions,
		SecondsAtTargetRooms: stats.fullSeconds, Restarts: stats.restarts.Load(), Wins: stats.wins.Load(), Losses: stats.losses.Load(),
		APIErrors: stats.apiErrors.Load(), CapacityRetries: stats.capacityRetries.Load(), ConnectionErrors: stats.connectionErrors.Load(), ProtocolErrors: stats.protocolErrors.Load(),
		ReceivedMessages: stats.messages.Load(), Snapshots: stats.snapshots.Load(), ReceivedPayloadBytes: stats.receivedBytes.Load(),
		SentInputs: stats.inputs.Load(), SentPayloadBytes: stats.sentBytes.Load(),
		SessionDurationMinMS: float64(stats.sessionMin) / float64(time.Millisecond), SessionDurationMaxMS: float64(stats.sessionMax) / float64(time.Millisecond),
		ResourceMeasurementNote: "CPU/RAM are not measured by this client. Sample the separate server process/container; local results do not prove Linux 0.5-CPU/256-MB compliance. Payload bytes exclude HTTP/WebSocket/TCP/TLS overhead. Session durations include sessions interrupted at test end.",
	}
	if elapsed > 0 {
		result.AverageConnectedRooms = stats.roomSeconds / elapsed
	}
	if stats.roomSeconds > 0 {
		result.SnapshotsPerRoomSecond = float64(result.Snapshots) / stats.roomSeconds
	}
	if stats.sessionCount > 0 {
		result.SessionDurationMeanMS = float64(stats.sessionSum) / float64(time.Millisecond) / float64(stats.sessionCount)
	}
	return result
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

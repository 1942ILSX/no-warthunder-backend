package server

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/quic-go/webtransport-go"
)

// Both transports feed the same authoritative room loop. Only WebTransport
// separates control and snapshots; neither accepts client-provided scores.
type roomConnection interface {
	ReadJSON(any) error
	WriteJSON(any) error
	WriteState(any) error
	Ping() error
	Finish()
	Close() error
}

type websocketConnection struct{ *websocket.Conn }

func newWebSocketConnection(conn *websocket.Conn) *websocketConnection {
	conn.SetReadLimit(512)
	_ = conn.SetReadDeadline(time.Now().Add(45 * time.Second))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(45 * time.Second)) })
	return &websocketConnection{conn}
}
func (c *websocketConnection) WriteJSON(v any) error {
	_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	return c.Conn.WriteJSON(v)
}
func (c *websocketConnection) WriteState(v any) error { return c.WriteJSON(v) }
func (c *websocketConnection) Ping() error {
	return c.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second))
}
func (c *websocketConnection) Finish() {
	_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "Run complete"), time.Now().Add(time.Second))
}

func (s *Server) connectWebTransport(w http.ResponseWriter, r *http.Request) {
	if s.config.WebTransport == nil || r.ProtoMajor != 3 {
		fail(w, 426, "transport_unavailable", "WebTransport requires HTTP/3")
		return
	}
	id, token := r.PathValue("id"), r.URL.Query().Get("token")
	s.mu.Lock()
	rm, ok := s.rooms[id]
	if !ok || time.Since(rm.created) > reservationTTL || subtle.ConstantTimeCompare([]byte(token), []byte(rm.token)) != 1 {
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
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()
	session, err := s.config.WebTransport.Upgrade(w, r)
	if err != nil {
		return
	}
	defer session.CloseWithError(0, "Session ended")
	// Establishing QUIC alone does not consume the reservation. The browser can
	// cancel before sending join and safely use the same token for WebSocket.
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	control, err := session.AcceptStream(ctx)
	if err != nil {
		return
	}
	_ = control.SetReadDeadline(time.Now().Add(5 * time.Second))
	scanner := bufio.NewScanner(control)
	scanner.Buffer(make([]byte, 512), 512)
	if !scanner.Scan() {
		return
	}
	var join struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(scanner.Bytes(), &join) != nil || join.Type != "join" {
		return
	}
	states, err := session.OpenUniStreamSync(ctx)
	if err != nil {
		return
	}
	s.mu.Lock()
	current, exists := s.rooms[id]
	if !exists || current != rm || rm.claimed || time.Since(rm.created) > reservationTTL || s.ctx.Err() != nil {
		s.mu.Unlock()
		return
	}
	rm.claimed = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.rooms, id); s.mu.Unlock() }()
	conn := newWebTransportConnection(session, control, states, scanner)
	defer conn.Close()
	s.runRoom(conn, rm)
}

type webTransportConnection struct {
	session     *webtransport.Session
	control     *webtransport.Stream
	states      *webtransport.SendStream
	scanner     *bufio.Scanner
	pending     chan []byte
	done        chan struct{}
	writerDone  chan struct{}
	finishedAck chan struct{}
	ackOnce     sync.Once
	closeOnce   sync.Once
}

func newWebTransportConnection(session *webtransport.Session, control *webtransport.Stream, states *webtransport.SendStream, scanner *bufio.Scanner) *webTransportConnection {
	c := &webTransportConnection{session: session, control: control, states: states, scanner: scanner,
		pending: make(chan []byte, 1), done: make(chan struct{}), writerDone: make(chan struct{}), finishedAck: make(chan struct{})}
	go c.writeStates()
	return c
}
func (c *webTransportConnection) ReadJSON(v any) error {
	for {
		_ = c.control.SetReadDeadline(time.Now().Add(45 * time.Second))
		if !c.scanner.Scan() {
			if err := c.scanner.Err(); err != nil {
				return err
			}
			return io.EOF
		}
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(c.scanner.Bytes(), &envelope); err != nil {
			return err
		}
		if envelope.Type == "finishedAck" {
			c.ackOnce.Do(func() { close(c.finishedAck) })
			return io.EOF
		}
		return json.Unmarshal(c.scanner.Bytes(), v)
	}
}
func (c *webTransportConnection) WriteJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(data) > 64*1024-1 {
		return errors.New("control message exceeds transport limit")
	}
	_ = c.control.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err = c.control.Write(append(data, '\n'))
	return err
}
func (c *webTransportConnection) WriteState(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(data) > 64*1024-1 {
		return errors.New("snapshot exceeds transport limit")
	}
	data = append(data, '\n')
	select {
	case <-c.done:
		return io.ErrClosedPipe
	default:
	}
	// Keep only the latest not-yet-written full snapshot. QUIC's send buffers
	// and the two-second deadline bound data already in flight.
	select {
	case c.pending <- data:
		return nil
	default:
	}
	select {
	case <-c.pending:
	default:
	}
	select {
	case c.pending <- data:
		return nil
	case <-c.done:
		return io.ErrClosedPipe
	}
}
func (c *webTransportConnection) writeStates() {
	defer close(c.writerDone)
	for {
		select {
		case <-c.done:
			return
		case <-c.session.Context().Done():
			return
		case data := <-c.pending:
			_ = c.states.SetWriteDeadline(time.Now().Add(2 * time.Second))
			if _, err := c.states.Write(data); err != nil {
				_ = c.session.CloseWithError(1, "Slow or disconnected client")
				return
			}
		}
	}
}
func (c *webTransportConnection) Ping() error { return c.session.Context().Err() }
func (c *webTransportConnection) Finish() {
	// Wait for application receipt: closing a session immediately after Write
	// can otherwise overtake the finished message on a different QUIC stream.
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-c.finishedAck:
	case <-c.session.Context().Done():
	case <-timer.C:
	}
}
func (c *webTransportConnection) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.session.CloseWithError(0, "Session ended")
		<-c.writerDone
	})
	return nil
}

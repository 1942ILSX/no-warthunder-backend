package main

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"sync/atomic"

	"github.com/quic-go/quic-go"
)

// Reload is explicit (SIGHUP), so certificate I/O never happens per handshake.
// Invalid replacement files leave the already-validated certificate in service.
type certificateStore struct {
	certFile, keyFile string
	current           atomic.Pointer[tls.Certificate]
}

func newCertificateStore(certFile, keyFile string) (*certificateStore, error) {
	s := &certificateStore{certFile: certFile, keyFile: keyFile}
	if err := s.reload(); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *certificateStore) reload() error {
	cert, err := tls.LoadX509KeyPair(s.certFile, s.keyFile)
	if err != nil {
		return err
	}
	s.current.Store(&cert)
	return nil
}
func (s *certificateStore) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return s.current.Load(), nil
}

type limitedListener struct {
	net.Listener
	max   int64
	count atomic.Int64
}

func (l *limitedListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.count.Add(1) > l.max {
			l.count.Add(-1)
			_ = conn.Close()
			continue
		}
		return &countedConnection{Conn: conn, release: func() { l.count.Add(-1) }}, nil
	}
}

type countedConnection struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *countedConnection) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

type quicLimiter struct {
	max   int64
	count atomic.Int64
	base  *quic.Config
}

func newQUICLimiter(max int64) *quicLimiter { return &quicLimiter{max: max} }
func (l *quicLimiter) config(*quic.ClientInfo) (*quic.Config, error) {
	if l.count.Load() >= l.max {
		return nil, errors.New("connection limit reached")
	}
	// Returning nil selects library defaults, not the original listener
	// config; preserve our resource limits and required WT capabilities.
	return l.base, nil
}
func (l *quicLimiter) context(ctx context.Context, conn *quic.Conn) context.Context {
	if l.count.Add(1) > l.max {
		l.count.Add(-1)
		_ = conn.CloseWithError(0, "Connection limit reached")
		return ctx
	}
	context.AfterFunc(conn.Context(), func() { l.count.Add(-1) })
	return ctx
}

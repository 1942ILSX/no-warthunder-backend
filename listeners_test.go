package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"BattleCityReforge/internal/difficulty"
	"BattleCityReforge/internal/game"
	"BattleCityReforge/internal/leaderboard"
	"BattleCityReforge/internal/server"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

func testCertificateFiles(t *testing.T) (string, string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(123), NotBefore: time.Now().Add(-time.Minute),
		NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost"}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func TestCertificateReloadRetainsValidCertificateOnFailure(t *testing.T) {
	certFile, keyFile := testCertificateFiles(t)
	store, err := newCertificateStore(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := store.get(nil)
	if err := os.WriteFile(keyFile, []byte("incomplete rotation"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.reload(); err == nil {
		t.Fatal("invalid replacement accepted")
	}
	after, _ := store.get(nil)
	if before != after {
		t.Fatal("failed reload changed active certificate")
	}
}

// Exercise the actual listener configuration, not just the room handler. A
// GetConfigForClient callback returning nil silently selects QUIC defaults and
// loses WebTransport capabilities; this test detects that regression.
func TestProductionListenersServeHTTP3AndWebTransport(t *testing.T) {
	certFile, keyFile := testCertificateFiles(t)
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	quicAddress := packet.LocalAddr().String()
	_ = packet.Close()
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcpAddress := tcp.Addr().String()
	_ = tcp.Close()
	for key, value := range map[string]string{"TLS_CERT_FILE": certFile, "TLS_KEY_FILE": keyFile, "H3_ADDR": quicAddress, "HTTPS_ADDR": tcpAddress, "HTTP_ADDR": "", "REDIRECT_ADDR": ""} {
		t.Setenv(key, value)
	}
	store, err := leaderboard.New(filepath.Join(t.TempDir(), "scores.json"))
	if err != nil {
		t.Fatal(err)
	}
	policies := map[difficulty.ID]game.Policy{}
	for _, p := range difficulty.All() {
		policies[p.ID] = game.ScriptedPolicy{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, server.Config{MaxRooms: 2, Policies: policies}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(7 * time.Second):
			t.Error("server failed to stop")
		}
	})
	tcpTransport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, ForceAttemptHTTP2: true}
	t.Cleanup(tcpTransport.CloseIdleConnections)
	tcpClient := &http.Client{Transport: tcpTransport, Timeout: time.Second}
	ready := false
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		response, err := tcpClient.Get("https://" + tcpAddress + "/healthz")
		if err == nil {
			_ = response.Body.Close()
			if response.ProtoMajor != 2 {
				t.Fatalf("HTTPS fallback negotiated %s", response.Proto)
			}
			if !strings.Contains(response.Header.Get("Alt-Svc"), strings.Split(quicAddress, ":")[1]) {
				t.Fatalf("wrong Alt-Svc: %v", response.Header)
			}
			ready = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		t.Fatal("TCP listener did not start")
	}
	h3Transport := &http3.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	t.Cleanup(func() { _ = h3Transport.Close() })
	client := &http.Client{Transport: h3Transport, Timeout: 3 * time.Second}
	base := "https://" + quicAddress
	response, err := client.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.ProtoMajor != 3 || response.StatusCode != 200 {
		t.Fatalf("H3 health: %s %d", response.Proto, response.StatusCode)
	}
	response, err = client.Post(base+"/api/rooms", "application/json", strings.NewReader(`{"levelId":1}`))
	if err != nil {
		t.Fatal(err)
	}
	var reservation struct{ WebTransportPath string }
	err = json.NewDecoder(response.Body).Decode(&reservation)
	_ = response.Body.Close()
	if err != nil || reservation.WebTransportPath == "" {
		t.Fatalf("reservation=%+v err=%v", reservation, err)
	}
	transport := &webtransport.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	t.Cleanup(func() { _ = transport.Close() })
	joining, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	_, session, err := transport.Dial(joining, base+reservation.WebTransportPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.CloseWithError(0, "test complete")
	stream, err := session.OpenStreamSync(joining)
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := fmt.Fprintln(stream, `{"type":"join"}`); err != nil {
		t.Fatal(err)
	}
	var welcome struct{ Type string }
	if err := json.NewDecoder(stream).Decode(&welcome); err != nil || welcome.Type != "welcome" {
		t.Fatalf("welcome=%+v err=%v", welcome, err)
	}
}

package main

import (
	"BattleCityReforge/internal/difficulty"
	"BattleCityReforge/internal/leaderboard"
	"BattleCityReforge/internal/levels"
	"BattleCityReforge/internal/server"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	for _, level := range levels.All() {
		if err := levels.Validate(level); err != nil {
			logger.Error("invalid level", "id", level.ID, "error", err)
			os.Exit(1)
		}
	}
	// Fail before opening a listener if any release artifact is missing or has
	// changed. All rooms share these three immutable, validated Go models.
	modelsDir := env("MODELS_DIR", "models")
	policies, err := difficulty.LoadPolicies(modelsDir)
	if err != nil {
		logger.Error("qualified models unavailable", "directory", modelsDir, "error", err)
		os.Exit(1)
	}
	store, err := leaderboard.New(env("LEADERBOARD_FILE", "data/leaderboard.json"))
	if err != nil {
		logger.Error("leaderboard unavailable", "error", err)
		os.Exit(1)
	}
	front := os.Getenv("FRONTEND_DIR")
	if err := server.CheckFrontend(front); err != nil {
		logger.Error("frontend unavailable", "error", err)
		os.Exit(1)
	}
	maxRooms, err := strconv.Atoi(env("MAX_ROOMS", "25"))
	if err != nil || maxRooms < 1 || maxRooms > 100 {
		logger.Error("MAX_ROOMS must be between 1 and 100")
		os.Exit(1)
	}
	assetBase := os.Getenv("CDN_ASSET_BASE")
	if assetBase != "" {
		u, err := url.Parse(assetBase)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			logger.Error("CDN_ASSET_BASE must be an HTTPS release root")
			os.Exit(1)
		}
		assetBase = strings.TrimRight(assetBase, "/") + "/"
	}
	assetRatio, err := strconv.ParseFloat(env("ORIGIN_SESSION_RATIO", "0.1"), 64)
	if err != nil || !(assetRatio >= 0 && assetRatio <= 1) {
		logger.Error("ORIGIN_SESSION_RATIO must be between 0 and 1")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	config := server.Config{MaxRooms: maxRooms, AllowedOrigins: strings.Split(env("ALLOWED_ORIGINS", "http://localhost:5173,http://127.0.0.1:5173"), ","), FrontendDir: front, Logger: logger, Policies: policies, AssetBaseURL: assetBase, OriginAssetRatio: assetRatio}
	if err := serve(ctx, config, store, logger); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

// Plain HTTP remains the default for local development. Setting both certificate
// paths enables TLS + HTTP/3. HTTP_ADDR then adds a separate loopback-only app
// listener for a local reverse proxy; REDIRECT_ADDR serves redirects only.
func serve(ctx context.Context, config server.Config, store *leaderboard.Store, logger *slog.Logger) error {
	certFile, keyFile := os.Getenv("TLS_CERT_FILE"), os.Getenv("TLS_KEY_FILE")
	if (certFile == "") != (keyFile == "") {
		return errors.New("TLS_CERT_FILE and TLS_KEY_FILE must be set together")
	}
	var wt *webtransport.Server
	var certs *certificateStore
	if certFile != "" {
		var err error
		certs, err = newCertificateStore(certFile, keyFile)
		if err != nil {
			return err
		}
		h3Addr := env("H3_ADDR", ":443")
		_, portString, err := net.SplitHostPort(h3Addr)
		if err != nil {
			return fmt.Errorf("H3_ADDR: %w", err)
		}
		port, err := strconv.Atoi(portString)
		if err != nil || port < 1 || port > 65535 {
			return errors.New("H3_ADDR must contain a valid numeric port")
		}
		limiter := newQUICLimiter(96)
		wt = &webtransport.Server{H3: &http3.Server{
			Addr: h3Addr, Port: port,
			TLSConfig:      http3.ConfigureTLSConfig(&tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: certs.get}),
			MaxHeaderBytes: 8192, IdleTimeout: 60 * time.Second, Logger: logger,
			ConnContext: limiter.context,
			QUICConfig: &quic.Config{Allow0RTT: false, EnableDatagrams: true, EnableStreamResetPartialDelivery: true, HandshakeIdleTimeout: 3 * time.Second, MaxIdleTimeout: 45 * time.Second,
				KeepAlivePeriod: 15 * time.Second, MaxIncomingStreams: 16, MaxIncomingUniStreams: 8,
				InitialStreamReceiveWindow: 32 * 1024, MaxStreamReceiveWindow: 64 * 1024,
				InitialConnectionReceiveWindow: 128 * 1024, MaxConnectionReceiveWindow: 256 * 1024,
				GetConfigForClient: limiter.config},
		}, ReorderingTimeout: 3 * time.Second, Config: &webtransport.Config{MaxIncomingStreams: 1, MaxIncomingUniStreams: -1, MaxIncomingData: 64 * 1024}}
		limiter.base = wt.H3.QUICConfig
		config.WebTransport = wt
	}
	app := server.New(config, store)
	defer app.Close()
	handler := app.Handler()
	if wt != nil {
		wrapped := handler
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// webtransport-go serves raw QUIC connections, so http3.Server never
			// registers a listener to initialize its automatic Alt-Svc header.
			w.Header().Set("Alt-Svc", fmt.Sprintf(`h3=":%d"; ma=3600`, wt.H3.Port))
			wrapped.ServeHTTP(w, r)
		})
		wt.H3.Handler = handler
	}
	type endpoint struct {
		server   *http.Server
		listener net.Listener
		tls      bool
	}
	var endpoints []endpoint
	defer func() {
		for _, e := range endpoints {
			_ = e.listener.Close()
		}
	}()
	add := func(addr string, h http.Handler, secure bool) error {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		httpServer := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
			WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 8192,
			HTTP2: &http.HTTP2Config{MaxConcurrentStreams: 16, MaxReadFrameSize: 16 * 1024,
				MaxReceiveBufferPerConnection: 128 * 1024, MaxReceiveBufferPerStream: 32 * 1024, WriteByteTimeout: 5 * time.Second}}
		if secure {
			httpServer.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: certs.get}
		}
		endpoints = append(endpoints, endpoint{httpServer, &limitedListener{Listener: ln, max: 96}, secure})
		return nil
	}
	if wt == nil {
		if err := add(env("ADDR", "127.0.0.1:8080"), handler, false); err != nil {
			return err
		}
	} else {
		if err := add(env("HTTPS_ADDR", ":443"), handler, true); err != nil {
			return err
		}
		if addr := os.Getenv("HTTP_ADDR"); addr != "" {
			host, _, err := net.SplitHostPort(addr)
			if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
				return errors.New("HTTP_ADDR must be a loopback IP:port")
			}
			if err := add(addr, handler, false); err != nil {
				return err
			}
		}
		if addr := os.Getenv("REDIRECT_ADDR"); addr != "" {
			origin := os.Getenv("PUBLIC_ORIGIN")
			u, err := url.Parse(origin)
			if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
				return errors.New("REDIRECT_ADDR requires HTTPS PUBLIC_ORIGIN")
			}
			redirect := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, strings.TrimRight(origin, "/")+r.URL.RequestURI(), http.StatusPermanentRedirect)
			})
			if err := add(addr, redirect, false); err != nil {
				return err
			}
		}
	}
	var udp net.PacketConn
	if wt != nil {
		var err error
		udp, err = net.ListenPacket("udp", wt.H3.Addr)
		if err != nil {
			return err
		}
		defer udp.Close()
		defer wt.Close()
	}
	errorsCh := make(chan error, len(endpoints)+1)
	for _, e := range endpoints {
		go func() {
			if e.tls {
				errorsCh <- e.server.ServeTLS(e.listener, "", "")
			} else {
				errorsCh <- e.server.Serve(e.listener)
			}
		}()
		logger.Info("HTTP listener ready", "address", e.server.Addr, "tls", e.tls)
	}
	if wt != nil {
		go func() { errorsCh <- wt.Serve(udp) }()
		logger.Info("HTTP/3 and WebTransport ready", "address", wt.H3.Addr)
	}
	logger.Info("NO WarThunder ready", "maxRooms", config.MaxRooms, "defaultDifficulty", difficulty.Default, "originSessionRatio", config.OriginAssetRatio)
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	var serveErr error
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case err := <-errorsCh:
			if !errors.Is(err, http.ErrServerClosed) {
				serveErr = err
			}
			break loop
		case <-hup:
			if certs != nil {
				if err := certs.reload(); err != nil {
					logger.Error("certificate reload failed; retaining current certificate", "error", err)
				} else {
					logger.Info("certificate reloaded")
				}
			}
		}
	}
	app.Close()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, e := range endpoints {
		_ = e.server.Shutdown(shutdown)
	}
	return serveErr
}
func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

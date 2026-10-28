package main

import (
	"BattleCityReforge/internal/leaderboard"
	"BattleCityReforge/internal/levels"
	"BattleCityReforge/internal/server"
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	for _, level := range levels.All() {
		if err := levels.Validate(level); err != nil {
			logger.Error("invalid level", "id", level.ID, "error", err)
			os.Exit(1)
		}
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
	app := server.New(server.Config{MaxRooms: maxRooms, AllowedOrigins: strings.Split(env("ALLOWED_ORIGINS", "http://localhost:5173,http://127.0.0.1:5173"), ","), FrontendDir: front, Logger: logger}, store)
	defer app.Close()
	httpServer := &http.Server{Addr: env("ADDR", "127.0.0.1:8080"), Handler: app.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 8192}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		app.Close()
		_ = httpServer.Shutdown(shutdown)
	}()
	logger.Info("NO WarThunder ready", "address", httpServer.Addr, "maxRooms", maxRooms)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

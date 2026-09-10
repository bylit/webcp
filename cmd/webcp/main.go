package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"webcp/internal/authn"
	"webcp/internal/download"
	webserver "webcp/internal/server"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	authConfig, err := authn.LoadFromEnv()
	if err != nil {
		logger.Error("invalid authentication configuration", "error", err)
		os.Exit(1)
	}
	maxConcurrent := envInt("MAX_CONCURRENT_DOWNLOADS", 4)
	manager, err := download.NewManager(env("DOWNLOAD_DIR", "/data/downloads"), env("STATE_FILE", "/data/.webcp/downloads.json"), maxConcurrent, logger)
	if err != nil {
		logger.Error("could not initialize download manager", "error", err)
		os.Exit(1)
	}
	manager.Recover()

	address := env("LISTEN_ADDR", ":8080")
	httpServer := &http.Server{
		Addr:              address,
		Handler:           webserver.New(manager, logger, authConfig),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0, // completed files may be streamed for an arbitrary duration
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		logger.Info("webcp listening", "address", address, "max_concurrent", maxConcurrent)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server stopped unexpectedly", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	logger.Info("shutting down")
	manager.PauseAll()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(key))
	if err != nil || value < 1 {
		return fallback
	}
	return value
}

package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/petoshi/qday-swap/internal/relay"
)

var version = "dev"

func main() {
	listen := flag.String("listen", ":8080", "HTTP listen address")
	data := flag.String("data", "./data/orders.db", "relay database path")
	network := flag.String("network", "mainnet", "accepted order network")
	publicURL := flag.String("public-url", "https://dex.pqday.com", "public DEX URL")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := os.MkdirAll(filepath.Dir(*data), 0700); err != nil {
		logger.Error("create data directory", "error", err)
		os.Exit(1)
	}
	store, err := relay.OpenStore(*data)
	if err != nil {
		logger.Error("open order store", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	application, err := relay.NewServer(store, relay.Config{
		Network: *network, PublicURL: *publicURL, Version: version, Logger: logger,
	})
	if err != nil {
		logger.Error("create relay", "error", err)
		os.Exit(1)
	}
	server := &http.Server{
		Addr: *listen, Handler: application.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	shutdownContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-shutdownContext.Done()
		context, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(context); err != nil {
			logger.Error("graceful shutdown", "error", err)
		}
	}()

	logger.Info("QDAY DEX relay listening", "address", *listen, "network", *network, "publicURL", *publicURL, "version", version)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("HTTP server stopped", "error", err)
		os.Exit(1)
	}
}

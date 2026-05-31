package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"grafana-tg-proxy/internal/config"
	"grafana-tg-proxy/internal/db"
	"grafana-tg-proxy/internal/logger"
	"grafana-tg-proxy/internal/metrics"
	"grafana-tg-proxy/internal/proxy"
	"grafana-tg-proxy/internal/server"
	"grafana-tg-proxy/internal/worker"
)

func main() {
	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading configuration: %v\n", err)
		os.Exit(1)
	}

	// Initialize global logger config
	logger.Setup(cfg.LogLevel, cfg.LogFormat, cfg.LogColor)
	log := logger.New("main")

	log.Info("Starting Grafana Telegram Proxy...")
	log.Debug("Config: %+v", cfg)

	if len(cfg.Proxies) > 0 {
		log.Info("Loaded %d SOCKS5 proxies", len(cfg.Proxies))
		for i, p := range cfg.Proxies {
			log.Info("  Proxy [%d]: %s", i+1, p)
		}
	} else {
		log.Warn("No SOCKS5 proxies loaded. Working in direct-fallback mode only.")
	}

	// Instantiate proxy rotator
	rotator, err := proxy.NewRotator(cfg.Proxies, 5*time.Second, cfg.DirectFallback)
	if err != nil {
		log.Error("Failed to initialize proxy rotator: %v", err)
		os.Exit(1)
	}

	// Run startup health check on SOCKS5 proxies
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	rotator.TestProxies(ctx)
	cancel()

	// Determine DB path (read DB_PATH from env or use default)
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "data/alerts.db"
	}

	// Initialize SQLite Database spooler
	database, err := db.NewDB(dbPath)
	if err != nil {
		log.Error("Failed to initialize database spool: %v", err)
		os.Exit(1)
	}
	defer func() {
		log.Info("Closing database connection...")
		if err := database.Close(); err != nil {
			log.Error("Error closing database: %v", err)
		}
	}()

	// Initialize and boot background retry worker
	retryWorker := worker.NewWorker(cfg, database, rotator)
	retryWorker.Start()
	defer retryWorker.Stop()

	// Start Prometheus metrics server scraper
	metrics.StartMetricsServer(cfg.MetricsPort)

	// Initialize HTTP server
	srv := server.NewServer(cfg, rotator, database)

	// Graceful shutdown setup
	shutdownChan := make(chan os.Signal, 1)
	signal.Notify(shutdownChan, os.Interrupt, syscall.SIGTERM)

	// Run HTTP server inside goroutine to handle signal listening concurrently
	go func() {
		if err := srv.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("HTTP Server stopped with error: %v", err)
			os.Exit(1)
		}
	}()

	// Block until signal is caught
	<-shutdownChan
	log.Info("Shutting down Grafana Telegram Proxy service gracefully...")

	// Close context to terminate connections
	_, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
}

// Command api serves the Cortex HTTP API.
//
// M1 exposes health probes only. Ingestion, job streaming, and search arrive in M2.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/emmyf/cortex/backend/internal/config"
	"github.com/emmyf/cortex/backend/internal/db"
	"github.com/emmyf/cortex/backend/internal/httpx"
	"github.com/emmyf/cortex/backend/internal/logging"
)

const shutdownTimeout = 15 * time.Second

func main() {
	if err := run(); err != nil {
		slog.Error("api exited with error", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	cfg := config.MustLoad()
	logger := logging.New(cfg.LogLevel)
	slog.SetDefault(logger)

	// Config carries a password; LogValue redacts it.
	logger.Info("starting api", slog.Any("config", cfg))

	// Signal context: Ctrl+C cancels this, unwinding shutdown in order.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()
	logger.Info("connected to database", slog.String("dsn", cfg.RedactedDatabaseURL()))

	checker := &httpx.Checker{
		Pool:      pool,
		RedisAddr: cfg.RedisAddr,
		OllamaURL: cfg.OllamaURL,
		Logger:    logger,
	}

	srv := &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", cfg.APIPort),
		Handler:           httpx.NewRouter(checker, cfg.CORSOrigin, logger),
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", slog.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	// Fresh context: ctx is already cancelled, so it cannot bound the drain.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	logger.Info("api stopped")
	return nil
}

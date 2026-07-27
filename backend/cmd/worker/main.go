// Command worker consumes ingestion jobs from the asynq queue.
//
// M1 registers no task handlers: this proves the consumer starts, holds a
// database pool, and answers a liveness probe. M2 adds the ingestion pipeline.
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
	"github.com/emmyf/cortex/backend/internal/queue"
)

const shutdownTimeout = 15 * time.Second

func main() {
	if err := run(); err != nil {
		slog.Error("worker exited with error", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	cfg := config.MustLoad()
	logger := logging.New(cfg.LogLevel)
	slog.SetDefault(logger)

	logger.Info("starting worker", slog.Any("config", cfg))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()
	logger.Info("connected to database", slog.String("dsn", cfg.RedactedDatabaseURL()))

	// Fail at startup if Redis is unreachable, rather than letting asynq retry
	// silently while the process looks healthy.
	pingCtx, cancelPing := context.WithTimeout(ctx, 5*time.Second)
	defer cancelPing()
	if err := queue.Ping(pingCtx, cfg.RedisAddr); err != nil {
		return fmt.Errorf("connect to redis: %w", err)
	}
	logger.Info("connected to redis", slog.String("addr", cfg.RedisAddr))

	healthSrv := &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", cfg.WorkerHealthPort),
		Handler:           httpx.NewWorkerRouter(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		logger.Info("worker health listening", slog.String("addr", healthSrv.Addr))
		if err := healthSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("worker health server", slog.String("error", err.Error()))
		}
	}()

	srv := queue.NewServer(cfg.RedisAddr)
	mux := queue.NewMux()
	// M2: mux.Handle(tasks.TypeIngestSource, handler.HandleIngest)

	runErr := make(chan error, 1)
	go func() {
		logger.Info("worker consuming queue")
		if err := srv.Run(mux); err != nil {
			runErr <- err
		}
	}()

	select {
	case err := <-runErr:
		return fmt.Errorf("queue server: %w", err)
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	// Stop pulling new tasks, then let in-flight ones finish.
	srv.Shutdown()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := healthSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shut down health server: %w", err)
	}

	logger.Info("worker stopped")
	return nil
}

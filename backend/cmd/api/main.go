// Command api serves the Cortex HTTP API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/emmyf/cortex/backend/internal/config"
	"github.com/emmyf/cortex/backend/internal/db"
	"github.com/emmyf/cortex/backend/internal/embed"
	"github.com/emmyf/cortex/backend/internal/events"
	"github.com/emmyf/cortex/backend/internal/httpx"
	"github.com/emmyf/cortex/backend/internal/logging"
	"github.com/emmyf/cortex/backend/internal/queue"
	"github.com/emmyf/cortex/backend/internal/store"
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()
	logger.Info("connected to database", slog.String("dsn", cfg.RedactedDatabaseURL()))

	st := store.New(pool)

	queueClient := queue.NewClient(cfg.RedisAddr)
	defer queueClient.Close()

	subscriber := events.NewSubscriber(cfg.RedisAddr)
	defer subscriber.Close()

	embedder := embed.New(cfg.OllamaURL, cfg.EmbeddingModel)

	routes := httpx.Routes{
		Checker: &httpx.Checker{
			Pool:      pool,
			RedisAddr: cfg.RedisAddr,
			OllamaURL: cfg.OllamaURL,
			Logger:    logger,
		},
		Sources: &httpx.SourceHandler{
			Store:          st,
			Queue:          queueClient,
			Logger:         logger,
			UploadDir:      filepath.Join(os.TempDir(), "cortex-uploads"),
			MaxUploadBytes: cfg.MaxUploadBytes,
		},
		Search: &httpx.SearchHandler{
			Store:    st,
			Embedder: embedder,
			Logger:   logger,
			DefaultK: cfg.SearchDefaultK,
			MaxK:     cfg.SearchMaxK,
		},
		Stream: &httpx.StreamHandler{
			Store:      st,
			Subscriber: subscriber,
			Logger:     logger,
		},
	}

	srv := &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", cfg.APIPort),
		Handler:           httpx.NewRouter(routes, cfg.CORSOrigin, logger),
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

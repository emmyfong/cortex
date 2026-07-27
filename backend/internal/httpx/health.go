package httpx

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/emmyf/cortex/backend/internal/queue"
	"github.com/jackc/pgx/v5/pgxpool"
)

// dependencyTimeout bounds each readiness check so a hung dependency cannot
// hold the probe open indefinitely.
const dependencyTimeout = 3 * time.Second

// Checker reports whether the process's dependencies are usable.
type Checker struct {
	Pool      *pgxpool.Pool
	RedisAddr string
	OllamaURL string
	Logger    *slog.Logger

	// HTTPClient is used for the Ollama probe. Injectable for tests.
	HTTPClient *http.Client
}

type dependencyStatus struct {
	Status string `json:"status"`          // "ok" or "error"
	Detail string `json:"detail,omitempty"`
}

type readyResponse struct {
	Status       string                      `json:"status"`
	Dependencies map[string]dependencyStatus `json:"dependencies"`
}

// Liveness reports only that the process is running. It must not touch
// dependencies: a liveness probe that fails when the database blips would
// restart a perfectly healthy process.
func Liveness() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// Readiness probes Postgres, Redis, and Ollama concurrently and reports each
// dependency separately. Returns 503 if any dependency is unusable.
//
// Failure details are logged server-side but returned to the client only as a
// short reason: driver errors can carry connection strings and internal
// hostnames, which do not belong in an HTTP response.
func (c *Checker) Readiness() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), dependencyTimeout)
		defer cancel()

		checks := map[string]func(context.Context) error{
			"postgres": c.checkPostgres,
			"redis":    c.checkRedis,
			"ollama":   c.checkOllama,
		}

		var (
			mu      sync.Mutex
			wg      sync.WaitGroup
			results = make(map[string]dependencyStatus, len(checks))
			allOK   = true
		)

		for name, check := range checks {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := check(ctx)

				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					c.Logger.Error("readiness check failed",
						slog.String("dependency", name),
						slog.String("error", err.Error()))
					results[name] = dependencyStatus{Status: "error", Detail: "unreachable"}
					allOK = false
					return
				}
				results[name] = dependencyStatus{Status: "ok"}
			}()
		}
		wg.Wait()

		status := http.StatusOK
		overall := "ok"
		if !allOK {
			status = http.StatusServiceUnavailable
			overall = "degraded"
		}

		writeJSON(w, status, readyResponse{Status: overall, Dependencies: results})
	}
}

func (c *Checker) checkPostgres(ctx context.Context) error {
	if c.Pool == nil {
		return fmt.Errorf("no database pool configured")
	}
	return c.Pool.Ping(ctx)
}

func (c *Checker) checkRedis(ctx context.Context) error {
	return queue.Ping(ctx, c.RedisAddr)
}

// checkOllama hits the model list endpoint, which confirms both that the daemon
// is up and that it is answering API calls.
func (c *Checker) checkOllama(ctx context.Context) error {
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: dependencyTimeout}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.OllamaURL+"/api/tags", nil)
	if err != nil {
		return fmt.Errorf("build ollama request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("call ollama: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama returned status %d", resp.StatusCode)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// Status and headers are already committed; logging is all that remains.
		slog.Error("encode response", slog.String("error", err.Error()))
	}
}

package httpx

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestLivenessIgnoresDependencies(t *testing.T) {
	// Liveness must not touch dependencies: a probe that fails when the
	// database blips would restart an otherwise healthy process.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)

	Liveness().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestReadinessReportsUnreachableDependencies(t *testing.T) {
	// Pool is nil and the addresses point nowhere, so every check fails.
	checker := &Checker{
		Pool:      nil,
		RedisAddr: "127.0.0.1:1",
		OllamaURL: "http://127.0.0.1:1",
		Logger:    discardLogger(),
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)

	checker.Readiness().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	var body readyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if body.Status != "degraded" {
		t.Errorf("status = %q, want %q", body.Status, "degraded")
	}
	for _, name := range []string{"postgres", "redis", "ollama"} {
		dep, ok := body.Dependencies[name]
		if !ok {
			t.Errorf("dependency %q missing from response", name)
			continue
		}
		if dep.Status != "error" {
			t.Errorf("dependency %q status = %q, want %q", name, dep.Status, "error")
		}
	}
}

// Driver errors can carry DSNs and internal hostnames. The probe logs the
// detail server-side but must not return it to the caller.
func TestReadinessDoesNotLeakConnectionDetails(t *testing.T) {
	checker := &Checker{
		RedisAddr: "secret-redis-host.internal:6379",
		OllamaURL: "http://secret-ollama-host.internal:11434",
		Logger:    discardLogger(),
	}

	rec := httptest.NewRecorder()
	checker.Readiness().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	body := rec.Body.String()
	for _, secret := range []string{"secret-redis-host", "secret-ollama-host"} {
		if strings.Contains(body, secret) {
			t.Errorf("response leaked %q: %s", secret, body)
		}
	}
}

func TestReadinessSucceedsWhenDependenciesRespond(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer ollama.Close()

	checker := &Checker{
		OllamaURL:  ollama.URL,
		Logger:     discardLogger(),
		HTTPClient: ollama.Client(),
	}

	if err := checker.checkOllama(t.Context()); err != nil {
		t.Errorf("checkOllama() = %v, want nil", err)
	}
}

func TestCheckOllamaRejectsNonOKStatus(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{"server error", http.StatusInternalServerError},
		{"not found", http.StatusNotFound},
		{"unauthorized", http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			checker := &Checker{
				OllamaURL:  srv.URL,
				Logger:     discardLogger(),
				HTTPClient: srv.Client(),
			}

			if err := checker.checkOllama(t.Context()); err == nil {
				t.Errorf("checkOllama() = nil, want error for status %d", tt.status)
			}
		})
	}
}

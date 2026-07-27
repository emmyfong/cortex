// Package httpx builds the HTTP surface: router, middleware, and health probes.
package httpx

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

const requestTimeout = 30 * time.Second

// NewRouter builds the API router with the standard middleware stack.
//
// CORS is restricted to the single configured origin. In development the web
// app proxies /api/* through Next.js, so cross-origin requests should not
// normally occur at all — this is a backstop, deliberately not a wildcard.
func NewRouter(checker *Checker, corsOrigin string, logger *slog.Logger) *chi.Mux {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(requestLogger(logger))
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(requestTimeout))
	r.Use(cors(corsOrigin))

	r.Get("/healthz", Liveness())
	r.Get("/readyz", checker.Readiness())

	// M2 mounts /api/v1 here: ingestion, job streams, search.

	return r
}

// NewWorkerRouter is the worker's minimal HTTP surface: liveness only, so a
// process supervisor can tell whether the consumer is still alive.
func NewWorkerRouter() *chi.Mux {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Get("/healthz", Liveness())
	return r
}

// requestLogger emits one structured line per request. It replaces chi's
// middleware.Logger, which writes unstructured text.
func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			next.ServeHTTP(ww, r)

			logger.Info("request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", ww.Status()),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Duration("duration", time.Since(start)),
				slog.String("request_id", middleware.GetReqID(r.Context())),
			)
		})
	}
}

// cors allows exactly one origin. An echo-the-request-origin implementation
// would defeat the purpose, so a mismatched origin simply gets no CORS headers.
func cors(allowedOrigin string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if origin := r.Header.Get("Origin"); origin != "" && origin == allowedOrigin {
				w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
				w.Header().Set("Vary", "Origin")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

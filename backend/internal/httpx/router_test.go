package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

const allowedOrigin = "http://localhost:3000"

func testRouter() http.Handler {
	routes := Routes{
		Checker: &Checker{
			RedisAddr: "127.0.0.1:1",
			OllamaURL: "http://127.0.0.1:1",
			Logger:    discardLogger(),
		},
	}
	return NewRouter(routes, allowedOrigin, discardLogger())
}

// CORS must not echo back whatever Origin the caller sends — that would defeat
// the point of restricting it at all.
func TestCORSOnlyAllowsConfiguredOrigin(t *testing.T) {
	tests := []struct {
		name       string
		origin     string
		wantHeader string
	}{
		{"configured origin is allowed", allowedOrigin, allowedOrigin},
		{"other origin gets no cors header", "http://evil.example.com", ""},
		{"port mismatch gets no cors header", "http://localhost:3001", ""},
		{"no origin header gets no cors header", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			rec := httptest.NewRecorder()

			testRouter().ServeHTTP(rec, req)

			got := rec.Header().Get("Access-Control-Allow-Origin")
			if got != tt.wantHeader {
				t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, tt.wantHeader)
			}
		})
	}
}

func TestPreflightReturnsNoContent(t *testing.T) {
	req := httptest.NewRequest(http.MethodOptions, "/healthz", nil)
	req.Header.Set("Origin", allowedOrigin)
	rec := httptest.NewRecorder()

	testRouter().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != allowedOrigin {
		t.Errorf("preflight Access-Control-Allow-Origin = %q, want %q", got, allowedOrigin)
	}
}

func TestRouterWiresHealthRoutes(t *testing.T) {
	tests := []struct {
		path       string
		wantStatus int
	}{
		// Liveness ignores dependencies, so it is 200 even though the
		// checker above points at dead addresses.
		{"/healthz", http.StatusOK},
		{"/readyz", http.StatusServiceUnavailable},
		{"/nope", http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			testRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))

			if rec.Code != tt.wantStatus {
				t.Errorf("GET %s = %d, want %d", tt.path, rec.Code, tt.wantStatus)
			}
		})
	}
}

func TestWorkerRouterExposesOnlyLiveness(t *testing.T) {
	router := NewWorkerRouter()

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("worker /healthz = %d, want %d", rec.Code, http.StatusOK)
	}

	// The worker holds a database pool but must not expose a readiness probe
	// that could leak dependency topology on an unauthenticated port.
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("worker /readyz = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

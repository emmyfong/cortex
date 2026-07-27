package embed

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeOllama serves embeddings of a chosen width so dimension handling can be
// tested without a real model.
func fakeOllama(t *testing.T, dims int, value float32) (*httptest.Server, *[]embedRequest) {
	t.Helper()
	var received []embedRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		var req embedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		received = append(received, req)

		vectors := make([][]float32, len(req.Input))
		for i := range vectors {
			v := make([]float32, dims)
			for j := range v {
				v[j] = value
			}
			vectors[i] = v
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(embedResponse{Embeddings: vectors})
	}))
	t.Cleanup(srv.Close)

	return srv, &received
}

func TestEmbedBatchReturnsOneVectorPerInput(t *testing.T) {
	srv, _ := fakeOllama(t, Dimensions, 0.5)
	client := New(srv.URL, "nomic-embed-text")

	vectors, err := client.EmbedBatch(t.Context(), []string{"one", "two", "three"})

	if err != nil {
		t.Fatalf("EmbedBatch() = %v, want nil", err)
	}
	if len(vectors) != 3 {
		t.Fatalf("got %d vectors, want 3", len(vectors))
	}
	for i, v := range vectors {
		if len(v) != Dimensions {
			t.Errorf("vector %d has %d dimensions, want %d", i, len(v), Dimensions)
		}
	}
}

// A long document must not be sent as one giant request: Ollama holds the whole
// batch in VRAM.
func TestEmbedBatchSplitsLargeInput(t *testing.T) {
	srv, received := fakeOllama(t, Dimensions, 0.5)
	client := New(srv.URL, "nomic-embed-text")

	texts := make([]string, batchSize*2+5)
	for i := range texts {
		texts[i] = "chunk text"
	}

	vectors, err := client.EmbedBatch(t.Context(), texts)
	if err != nil {
		t.Fatalf("EmbedBatch() = %v, want nil", err)
	}

	if len(vectors) != len(texts) {
		t.Errorf("got %d vectors for %d inputs", len(vectors), len(texts))
	}
	if len(*received) != 3 {
		t.Errorf("made %d requests, want 3 (two full batches plus a remainder)", len(*received))
	}
	for i, req := range *received {
		if len(req.Input) > batchSize {
			t.Errorf("request %d carried %d texts, above the batch size of %d",
				i, len(req.Input), batchSize)
		}
	}
}

// A model whose width does not match the schema must fail with a message that
// names the problem, not with an opaque Postgres insert error later.
func TestRejectsWrongDimensions(t *testing.T) {
	srv, _ := fakeOllama(t, 384, 0.5)
	client := New(srv.URL, "some-other-model")

	_, err := client.Embed(t.Context(), "text")

	if err == nil {
		t.Fatal("Embed() = nil error for a 384-dimension model, want error")
	}
	for _, want := range []string{"384", "768", "EMBEDDING_MODEL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}

// A zero vector has undefined cosine distance and would silently corrupt search.
func TestRejectsAllZeroVector(t *testing.T) {
	srv, _ := fakeOllama(t, Dimensions, 0)
	client := New(srv.URL, "nomic-embed-text")

	if _, err := client.Embed(t.Context(), "text"); err == nil {
		t.Fatal("Embed() accepted an all-zero vector, want error")
	}
}

func TestSurfacesOllamaErrorMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(embedResponse{Error: `model "ghost" not found, try pulling it first`})
	}))
	defer srv.Close()

	_, err := New(srv.URL, "ghost").Embed(t.Context(), "text")

	if err == nil {
		t.Fatal("Embed() = nil, want error")
	}
	// The user needs Ollama's own wording — it tells them exactly what to pull.
	if !strings.Contains(err.Error(), "not found, try pulling it first") {
		t.Errorf("ollama's message was swallowed: %v", err)
	}
}

func TestEmbedBatchEmptyInput(t *testing.T) {
	srv, received := fakeOllama(t, Dimensions, 0.5)

	vectors, err := New(srv.URL, "m").EmbedBatch(t.Context(), nil)

	if err != nil {
		t.Errorf("EmbedBatch(nil) = %v, want nil", err)
	}
	if len(vectors) != 0 {
		t.Errorf("got %d vectors, want 0", len(vectors))
	}
	if len(*received) != 0 {
		t.Errorf("made %d HTTP requests for empty input, want 0", len(*received))
	}
}

func TestFormatProducesPgvectorLiteral(t *testing.T) {
	tests := []struct {
		name   string
		vector []float32
		want   string
	}{
		{"simple", []float32{1, 2, 3}, "[1,2,3]"},
		{"fractional", []float32{0.5, -0.25}, "[0.5,-0.25]"},
		{"zero", []float32{0, 0}, "[0,0]"},
		{"empty", []float32{}, "[]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Format(tt.vector); got != tt.want {
				t.Errorf("Format() = %q, want %q", got, tt.want)
			}
		})
	}
}

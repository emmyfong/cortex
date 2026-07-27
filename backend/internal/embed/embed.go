// Package embed turns text into vectors using a local Ollama model.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Dimensions is the width the database column expects. nomic-embed-text emits
// 768; a different model would silently produce vectors Postgres rejects at
// insert time with an opaque error, so the client checks here instead.
const Dimensions = 768

// batchSize bounds how many texts go in one request. Ollama holds the whole
// batch in VRAM, so an unbounded batch on a long document can exhaust it.
const batchSize = 32

const requestTimeout = 2 * time.Minute

// Client calls an Ollama server's embedding endpoint.
type Client struct {
	baseURL string
	model   string
	http    *http.Client
}

func New(baseURL, model string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		http:    &http.Client{Timeout: requestTimeout},
	}
}

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
	Error      string      `json:"error,omitempty"`
}

// EmbedBatch returns one vector per input text, in the same order.
//
// Large inputs are split into several requests transparently.
func (c *Client) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	vectors := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += batchSize {
		end := min(start+batchSize, len(texts))

		batch, err := c.embedOnce(ctx, texts[start:end])
		if err != nil {
			return nil, fmt.Errorf("embed texts %d-%d: %w", start, end-1, err)
		}
		vectors = append(vectors, batch...)
	}
	return vectors, nil
}

// Embed is the single-text convenience form, used by search queries.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	vectors, err := c.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vectors) != 1 {
		return nil, fmt.Errorf("expected 1 embedding, got %d", len(vectors))
	}
	return vectors[0], nil
}

func (c *Client) embedOnce(ctx context.Context, texts []string) ([][]float32, error) {
	payload, err := json.Marshal(embedRequest{Model: c.model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/embed", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call ollama at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	var decoded embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode response (status %d): %w", resp.StatusCode, err)
	}

	if resp.StatusCode != http.StatusOK {
		if decoded.Error != "" {
			// Surface Ollama's own message: "model not found" is the common
			// case and tells the user exactly what to pull.
			return nil, fmt.Errorf("ollama returned %d: %s", resp.StatusCode, decoded.Error)
		}
		return nil, fmt.Errorf("ollama returned status %d", resp.StatusCode)
	}

	if len(decoded.Embeddings) != len(texts) {
		return nil, fmt.Errorf("requested %d embeddings, received %d", len(texts), len(decoded.Embeddings))
	}

	for i, vector := range decoded.Embeddings {
		if len(vector) != Dimensions {
			return nil, fmt.Errorf(
				"model %q produced %d-dimension vectors, but the schema expects %d; "+
					"either set EMBEDDING_MODEL back to a %d-dimension model or migrate the chunks.embedding column",
				c.model, len(vector), Dimensions, Dimensions)
		}
		if i == 0 && allZero(vector) {
			return nil, fmt.Errorf("model %q returned an all-zero vector, which cannot be matched against", c.model)
		}
	}

	return decoded.Embeddings, nil
}

// allZero detects a degenerate embedding. A zero vector has undefined cosine
// distance to everything and would poison search results silently.
func allZero(vector []float32) bool {
	for _, v := range vector {
		if v != 0 {
			return false
		}
	}
	return true
}

// Format renders a vector in the literal syntax pgvector accepts, e.g.
// "[0.1,0.2,...]". Used for both inserts and query parameters.
func Format(vector []float32) string {
	var sb strings.Builder
	sb.Grow(len(vector) * 12)
	sb.WriteByte('[')
	for i, v := range vector {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(formatFloat(v))
	}
	sb.WriteByte(']')
	return sb.String()
}

func formatFloat(f float32) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.8f", f), "0"), ".")
}

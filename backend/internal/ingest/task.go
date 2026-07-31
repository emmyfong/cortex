// Package ingest runs the document pipeline: parse, chunk, embed, store.
package ingest

import (
	"encoding/json"
	"fmt"

	"github.com/hibiken/asynq"
)

// Task types handled by the worker.
const (
	// TypeIngestSource parses, chunks, embeds, and stores a document.
	TypeIngestSource = "source:ingest"

	// TypeExtractConcepts builds graph nodes and edges from a stored document.
	//
	// Deliberately a separate task rather than a step inside ingestion: concept
	// extraction is the slowest and least reliable stage, and folding it into
	// ingestion would mean an LLM failure discards chunks and embeddings that
	// were already computed correctly. Split this way, search works as soon as
	// ingestion finishes and extraction retries on its own.
	TypeExtractConcepts = "concepts:extract"
)

// ExtractPayload identifies the document to build graph structure from.
type ExtractPayload struct {
	JobID    string `json:"job_id"`
	SourceID string `json:"source_id"`
}

// NewExtractTask builds the queue task for concept extraction.
func NewExtractTask(p ExtractPayload) (*asynq.Task, error) {
	encoded, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("encode extract payload: %w", err)
	}
	return asynq.NewTask(TypeExtractConcepts, encoded), nil
}

func decodeExtractPayload(data []byte) (ExtractPayload, error) {
	var p ExtractPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return ExtractPayload{}, fmt.Errorf("decode extract payload: %w", err)
	}
	if p.JobID == "" || p.SourceID == "" {
		return ExtractPayload{}, fmt.Errorf("extract payload missing job_id or source_id")
	}
	return p, nil
}

// Payload carries everything the worker needs to process one source.
type Payload struct {
	JobID    string `json:"job_id"`
	SourceID string `json:"source_id"`

	// SourceType selects the parser. "web" reads Ref as a URL; "pdf" reads
	// BlobHash as the key of a stored original.
	SourceType string `json:"source_type"`
	Ref        string `json:"ref,omitempty"`

	// BlobHash is the hex-encoded key of the stored upload. The worker reads
	// the blob and leaves it in place — it is the retained original, not a
	// temp file to consume.
	BlobHash string `json:"blob_hash,omitempty"`

	// TitleHint is used when the parser cannot determine a title, e.g. the
	// original filename of an uploaded PDF.
	TitleHint string `json:"title_hint,omitempty"`
}

// NewTask builds the queue task for a payload.
func NewTask(p Payload) (*asynq.Task, error) {
	encoded, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("encode ingest payload: %w", err)
	}
	return asynq.NewTask(TypeIngestSource, encoded), nil
}

func decodePayload(data []byte) (Payload, error) {
	var p Payload
	if err := json.Unmarshal(data, &p); err != nil {
		return Payload{}, fmt.Errorf("decode ingest payload: %w", err)
	}
	if p.JobID == "" || p.SourceID == "" {
		return Payload{}, fmt.Errorf("payload missing job_id or source_id")
	}
	return p, nil
}

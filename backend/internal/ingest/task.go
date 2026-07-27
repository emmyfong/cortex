// Package ingest runs the document pipeline: parse, chunk, embed, store.
package ingest

import (
	"encoding/json"
	"fmt"

	"github.com/hibiken/asynq"
)

// TypeIngestSource is the asynq task type for a document ingestion.
const TypeIngestSource = "source:ingest"

// Payload carries everything the worker needs to process one source.
type Payload struct {
	JobID    string `json:"job_id"`
	SourceID string `json:"source_id"`

	// SourceType selects the parser: "web" reads Ref as a URL, "pdf" reads it
	// as a path to a temp file the API wrote.
	SourceType string `json:"source_type"`
	Ref        string `json:"ref"`

	// TitleHint is used when the parser cannot determine a title, e.g. the
	// original filename of an uploaded PDF.
	TitleHint string `json:"title_hint,omitempty"`

	// CleanupPath, when set, is deleted once processing finishes. Uploaded
	// files must not accumulate on disk.
	CleanupPath string `json:"cleanup_path,omitempty"`
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

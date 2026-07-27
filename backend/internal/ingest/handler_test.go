package ingest

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hibiken/asynq"
)

// Failure messages are shown to whoever uploaded the document, so queue
// internals must not appear in them.
func TestUserFacingError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "strips the asynq skip-retry prefix",
			err:  fmt.Errorf("%w: this document has already been ingested", asynq.SkipRetry),
			want: "This document has already been ingested",
		},
		{
			name: "capitalises an ordinary error",
			err:  errors.New("pdftotext failed: no text extracted"),
			want: "Pdftotext failed: no text extracted",
		},
		{
			name: "leaves an already-capitalised message alone",
			err:  errors.New("Ollama unreachable"),
			want: "Ollama unreachable",
		},
		{
			name: "falls back when the message is empty",
			err:  errors.New(""),
			want: "ingestion failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := userFacingError(tt.err)

			if got != tt.want {
				t.Errorf("userFacingError() = %q, want %q", got, tt.want)
			}
			if strings.Contains(got, "skip retry") {
				t.Errorf("queue internals leaked into the message: %q", got)
			}
		})
	}
}

func TestDecodePayloadRejectsIncomplete(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{"malformed json", `{"job_id":`},
		{"missing job id", `{"source_id":"abc"}`},
		{"missing source id", `{"job_id":"abc"}`},
		{"empty object", `{}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := decodePayload([]byte(tt.json)); err == nil {
				t.Error("decodePayload accepted an incomplete payload")
			}
		})
	}
}

func TestNewTaskRoundTrip(t *testing.T) {
	original := Payload{
		JobID:      "job-1",
		SourceID:   "src-1",
		SourceType: "pdf",
		BlobHash:   "abc123",
		TitleHint:  "paper.pdf",
	}

	task, err := NewTask(original)
	if err != nil {
		t.Fatalf("NewTask: %v", err)
	}
	if task.Type() != TypeIngestSource {
		t.Errorf("task type = %q, want %q", task.Type(), TypeIngestSource)
	}

	decoded, err := decodePayload(task.Payload())
	if err != nil {
		t.Fatalf("decodePayload: %v", err)
	}
	if decoded != original {
		t.Errorf("round trip changed payload:\n got: %+v\nwant: %+v", decoded, original)
	}
}

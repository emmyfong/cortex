package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/emmyf/cortex/backend/internal/chunk"
	"github.com/emmyf/cortex/backend/internal/embed"
	"github.com/emmyf/cortex/backend/internal/events"
	"github.com/emmyf/cortex/backend/internal/parse"
	"github.com/emmyf/cortex/backend/internal/store"
	"github.com/hibiken/asynq"
)

// Progress checkpoints. Parsing and embedding dominate the wall time, so the
// scale is weighted towards them rather than spread evenly across steps.
const (
	progressParsing   = 10
	progressChunking  = 35
	progressEmbedding = 50
	progressStoring   = 90
)

// Handler runs the ingestion pipeline for one source.
type Handler struct {
	store     *store.Store
	web       *parse.WebParser
	pdf       *parse.PDFParser
	embedder  *embed.Client
	splitter  *chunk.Splitter
	publisher *events.Publisher
	logger    *slog.Logger
}

func NewHandler(
	st *store.Store,
	web *parse.WebParser,
	pdf *parse.PDFParser,
	embedder *embed.Client,
	splitter *chunk.Splitter,
	publisher *events.Publisher,
	logger *slog.Logger,
) *Handler {
	return &Handler{
		store: st, web: web, pdf: pdf,
		embedder: embedder, splitter: splitter,
		publisher: publisher, logger: logger,
	}
}

// ProcessTask is the asynq entry point.
func (h *Handler) ProcessTask(ctx context.Context, task *asynq.Task) error {
	payload, err := decodePayload(task.Payload())
	if err != nil {
		// A malformed payload will never succeed, so tell asynq not to retry.
		return fmt.Errorf("%w: %v", asynq.SkipRetry, err)
	}

	// Uploaded files are temporary regardless of outcome.
	if payload.CleanupPath != "" {
		defer func() {
			if err := os.Remove(payload.CleanupPath); err != nil && !os.IsNotExist(err) {
				h.logger.Warn("could not remove upload",
					slog.String("path", payload.CleanupPath),
					slog.String("error", err.Error()))
			}
		}()
	}

	if err := h.run(ctx, payload); err != nil {
		h.fail(ctx, payload, err)
		return err
	}
	return nil
}

func (h *Handler) run(ctx context.Context, p Payload) error {
	h.report(ctx, p, "Extracting text", progressParsing)

	doc, err := h.parseSource(ctx, p)
	if err != nil {
		return err
	}

	title := strings.TrimSpace(doc.Title)
	if title == "" {
		title = p.TitleHint
	}
	if title == "" {
		title = "Untitled"
	}

	if err := h.store.SetSourceContent(ctx, p.SourceID, title, doc.Markdown); err != nil {
		if errors.Is(err, store.ErrDuplicateSource) {
			// Not a failure: the content is already in the index. Stop cleanly
			// rather than storing a second copy under a new id.
			return fmt.Errorf("%w: this document has already been ingested", asynq.SkipRetry)
		}
		return err
	}

	h.report(ctx, p, "Splitting into passages", progressChunking)

	chunks := h.splitter.Split(doc.Markdown)
	if len(chunks) == 0 {
		return fmt.Errorf("%w: document produced no text to index", asynq.SkipRetry)
	}

	h.report(ctx, p, fmt.Sprintf("Generating embeddings for %d passages", len(chunks)), progressEmbedding)

	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Content
	}

	vectors, err := h.embedder.EmbedBatch(ctx, texts)
	if err != nil {
		return fmt.Errorf("generate embeddings: %w", err)
	}

	h.report(ctx, p, "Saving passages", progressStoring)

	if err := h.store.ReplaceChunks(ctx, p.SourceID, chunks, vectors); err != nil {
		return err
	}
	if err := h.store.MarkSourceReady(ctx, p.SourceID); err != nil {
		return err
	}
	if err := h.store.MarkJobSucceeded(ctx, p.JobID); err != nil {
		return err
	}

	h.publisher.Publish(p.JobID, events.Event{
		Type:     events.EventComplete,
		Stage:    "Complete",
		Progress: 100,
		SourceID: p.SourceID,
		Chunks:   len(chunks),
	})

	h.logger.Info("ingest complete",
		slog.String("source_id", p.SourceID),
		slog.String("title", title),
		slog.Int("chunks", len(chunks)))
	return nil
}

func (h *Handler) parseSource(ctx context.Context, p Payload) (parse.Document, error) {
	switch p.SourceType {
	case store.TypeWeb:
		return h.web.Parse(ctx, p.Ref)
	case store.TypePDF:
		return h.pdf.Parse(ctx, p.Ref)
	default:
		return parse.Document{}, fmt.Errorf("%w: unsupported source type %q", asynq.SkipRetry, p.SourceType)
	}
}

// report records progress durably and pushes it to any live SSE listener.
func (h *Handler) report(ctx context.Context, p Payload, stage string, progress int) {
	if err := h.store.UpdateJobProgress(ctx, p.JobID, stage, progress); err != nil {
		// Progress reporting is not worth failing an ingest over.
		h.logger.Warn("could not record job progress",
			slog.String("job_id", p.JobID), slog.String("error", err.Error()))
	}
	h.publisher.Publish(p.JobID, events.Event{
		Type:     events.EventStatus,
		Stage:    stage,
		Progress: progress,
		SourceID: p.SourceID,
	})
}

// fail records a terminal failure on both the job and the source.
//
// It uses a background context: the task context may already be cancelled by
// the very timeout that caused the failure, and the error must still be
// persisted or the UI would wait forever on a job that already died.
func (h *Handler) fail(taskCtx context.Context, p Payload, cause error) {
	ctx := context.WithoutCancel(taskCtx)

	message := cause.Error()
	h.logger.Error("ingest failed",
		slog.String("job_id", p.JobID),
		slog.String("source_id", p.SourceID),
		slog.String("error", message))

	if err := h.store.MarkJobFailed(ctx, p.JobID, message); err != nil {
		h.logger.Error("could not mark job failed", slog.String("error", err.Error()))
	}
	if err := h.store.MarkSourceFailed(ctx, p.SourceID); err != nil {
		h.logger.Error("could not mark source failed", slog.String("error", err.Error()))
	}

	h.publisher.Publish(p.JobID, events.Event{
		Type:     events.EventFailed,
		Stage:    "Failed",
		Error:    message,
		SourceID: p.SourceID,
	})
}

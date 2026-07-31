package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/emmyf/cortex/backend/internal/blob"
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

// DocumentParser turns a reference — a URL or a file path — into markdown.
//
// An interface rather than the concrete parsers so a test can substitute a
// stub. The alternative would be relaxing the web parser's SSRF guard to let
// tests reach a loopback server, which would weaken a real protection for the
// convenience of the test suite.
type DocumentParser interface {
	Parse(ctx context.Context, ref string) (parse.Document, error)
}

// Handler runs the ingestion pipeline for one source.
type Handler struct {
	store     *store.Store
	blobs     *blob.Store
	queue     *asynq.Client
	web       DocumentParser
	pdf       DocumentParser
	embedder  *embed.Client
	splitter  *chunk.Splitter
	publisher *events.Publisher
	logger    *slog.Logger
}

func NewHandler(
	st *store.Store,
	blobs *blob.Store,
	queue *asynq.Client,
	web DocumentParser,
	pdf DocumentParser,
	embedder *embed.Client,
	splitter *chunk.Splitter,
	publisher *events.Publisher,
	logger *slog.Logger,
) *Handler {
	return &Handler{
		store: st, blobs: blobs, queue: queue, web: web, pdf: pdf,
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

	// The uploaded original is deliberately kept: it is the stored file the
	// user can reopen. Only the source row's deletion removes it.

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

	h.queueExtraction(ctx, p.SourceID)
	return nil
}

// queueExtraction hands the document off for graph building.
//
// Failures here are logged, never returned: the ingestion itself succeeded and
// the document is searchable. Reporting the job as failed because a follow-up
// could not be queued would be a lie about work that did complete.
func (h *Handler) queueExtraction(ctx context.Context, sourceID string) {
	if h.queue == nil {
		return // extraction disabled
	}

	jobID, err := h.store.CreateJob(ctx, sourceID)
	if err != nil {
		h.logger.Warn("could not create concept extraction job",
			slog.String("source_id", sourceID), slog.String("error", err.Error()))
		return
	}

	task, err := NewExtractTask(ExtractPayload{JobID: jobID, SourceID: sourceID})
	if err == nil {
		_, err = h.queue.EnqueueContext(ctx, task)
	}
	if err != nil {
		h.logger.Warn("could not queue concept extraction",
			slog.String("source_id", sourceID), slog.String("error", err.Error()))
		_ = h.store.MarkJobFailed(ctx, jobID, "Could not queue concept extraction")
		return
	}

	h.logger.Info("queued concept extraction",
		slog.String("source_id", sourceID), slog.String("job_id", jobID))
}

func (h *Handler) parseSource(ctx context.Context, p Payload) (parse.Document, error) {
	switch p.SourceType {
	case store.TypeWeb:
		return h.web.Parse(ctx, p.Ref)

	case store.TypePDF:
		if p.BlobHash == "" {
			return parse.Document{}, fmt.Errorf("%w: pdf payload has no blob hash", asynq.SkipRetry)
		}
		hash, err := blob.HexToHash(p.BlobHash)
		if err != nil {
			return parse.Document{}, fmt.Errorf("%w: %v", asynq.SkipRetry, err)
		}
		// pdftotext reads the stored blob in place; it is never modified or
		// removed here.
		path, err := h.blobs.Path(hash)
		if err != nil {
			return parse.Document{}, fmt.Errorf("locate stored upload: %w", err)
		}
		return h.pdf.Parse(ctx, path)

	default:
		return parse.Document{}, fmt.Errorf("%w: unsupported source type %q", asynq.SkipRetry, p.SourceType)
	}
}

// userFacingError renders a failure for the person who uploaded the document.
//
// Errors marked non-retryable are wrapped with asynq.SkipRetry, which prefixes
// the message with "skip retry for the task: ". That is queue plumbing and
// means nothing to a user staring at a failed upload, so it is stripped before
// the message is stored or streamed.
func userFacingError(err error) string {
	message := err.Error()

	if prefix := asynq.SkipRetry.Error() + ": "; strings.HasPrefix(message, prefix) {
		message = strings.TrimPrefix(message, prefix)
	}
	if message == "" {
		return "ingestion failed"
	}

	// Capitalise the first letter so the message reads as a sentence in the UI.
	return strings.ToUpper(message[:1]) + message[1:]
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

	message := userFacingError(cause)
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

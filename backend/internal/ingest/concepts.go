package ingest

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/emmyf/cortex/backend/internal/embed"
	"github.com/emmyf/cortex/backend/internal/events"
	"github.com/emmyf/cortex/backend/internal/extract"
	"github.com/emmyf/cortex/backend/internal/store"
	"github.com/hibiken/asynq"
)

// Progress checkpoints for the two phases: reading passages for concepts, then
// judging which concepts relate.
const (
	progressEmbeddingConcepts = 80
	progressJudging           = 88
)

// maxConceptsToEmbed bounds one pass so a large backlog cannot stall a job.
// Anything left over is picked up by the next document's extraction.
const maxConceptsToEmbed = 500

// ConceptHandler builds graph structure from an already-ingested document.
type ConceptHandler struct {
	store     *store.Store
	extractor *extract.Client
	embedder  *embed.Client
	publisher *events.Publisher
	logger    *slog.Logger
}

func NewConceptHandler(
	st *store.Store,
	extractor *extract.Client,
	embedder *embed.Client,
	publisher *events.Publisher,
	logger *slog.Logger,
) *ConceptHandler {
	return &ConceptHandler{
		store: st, extractor: extractor, embedder: embedder,
		publisher: publisher, logger: logger,
	}
}

// ProcessTask is the asynq entry point for concept extraction.
func (h *ConceptHandler) ProcessTask(ctx context.Context, task *asynq.Task) error {
	payload, err := decodeExtractPayload(task.Payload())
	if err != nil {
		return fmt.Errorf("%w: %v", asynq.SkipRetry, err)
	}

	if err := h.run(ctx, payload); err != nil {
		h.fail(ctx, payload, err)
		return err
	}
	return nil
}

func (h *ConceptHandler) run(ctx context.Context, p ExtractPayload) error {
	source, err := h.store.GetSource(ctx, p.SourceID)
	if err != nil {
		return fmt.Errorf("%w: %v", asynq.SkipRetry, err)
	}

	chunks, err := h.store.ListChunksForSource(ctx, p.SourceID)
	if err != nil {
		return err
	}
	if len(chunks) == 0 {
		return fmt.Errorf("%w: source has no passages to extract from", asynq.SkipRetry)
	}

	h.report(ctx, p, fmt.Sprintf("Reading %d passages for concepts", len(chunks)), 5)

	passages := make([]store.PassageConcepts, 0, len(chunks))
	extracted := 0

	for i, c := range chunks {
		concepts, err := h.extractor.FromPassage(ctx, c.Content)
		if err != nil {
			// One bad passage should not discard the work already done on the
			// rest of the document. Record it and keep going; a document that
			// yields nothing at all still fails below.
			h.logger.Warn("concept extraction failed for a passage",
				slog.String("source_id", p.SourceID),
				slog.String("chunk_id", c.ID),
				slog.String("error", err.Error()))
			continue
		}

		if len(concepts) > 0 {
			passages = append(passages, store.PassageConcepts{
				ChunkID:     c.ID,
				HeadingPath: c.HeadingPath,
				Concepts:    concepts,
			})
			extracted += len(concepts)
		}

		// Progress is reported per passage because extraction is slow enough
		// that a single jump from 0 to 100 would look like a hang.
		progress := 5 + (70 * (i + 1) / len(chunks))
		h.report(ctx, p, fmt.Sprintf("Extracted concepts from %d of %d passages", i+1, len(chunks)), progress)
	}

	if len(passages) == 0 {
		return fmt.Errorf("%w: no concepts could be extracted from this document", asynq.SkipRetry)
	}

	if err := h.store.SaveExtraction(ctx, p.SourceID, source.Title, passages); err != nil {
		return err
	}

	unique, err := h.store.CountConceptsForSource(ctx, p.SourceID)
	if err != nil {
		return err
	}

	// Edges are built separately from concepts, and a failure here must not
	// discard the concepts and provenance already committed.
	edges, err := h.buildRelationships(ctx, p)
	if err != nil {
		h.logger.Warn("relationship judging failed; concepts kept without new edges",
			slog.String("source_id", p.SourceID),
			slog.String("error", err.Error()))
	}

	if err := h.store.MarkJobSucceeded(ctx, p.JobID); err != nil {
		return err
	}

	h.publisher.Publish(p.JobID, events.Event{
		Type:     events.EventComplete,
		Stage:    "Concepts extracted",
		Progress: 100,
		SourceID: p.SourceID,
		Concepts: unique,
	})

	h.logger.Info("concept extraction complete",
		slog.String("source_id", p.SourceID),
		slog.String("title", source.Title),
		slog.Int("mentions", extracted),
		slog.Int("unique_concepts", unique),
		slog.Int("relationships", edges))
	return nil
}

// buildRelationships turns concepts into graph edges.
//
// Candidates are generated cheaply and permissively, then a model decides which
// pairs are genuinely related and states why. Nothing unjudged becomes an edge:
// a graph where proximity counts as relatedness is the problem this replaces.
func (h *ConceptHandler) buildRelationships(ctx context.Context, p ExtractPayload) (int, error) {
	h.report(ctx, p, "Embedding concepts", progressEmbeddingConcepts)

	if err := h.embedNewConcepts(ctx); err != nil {
		// Similarity candidates need embeddings, but co-occurrence does not —
		// carry on with a within-document graph rather than none.
		h.logger.Warn("could not embed concepts; cross-document links unavailable",
			slog.String("error", err.Error()))
	}

	cooccurring, err := h.store.CooccurrenceCandidates(ctx, p.SourceID)
	if err != nil {
		return 0, err
	}
	similar, err := h.store.SimilarityCandidates(ctx, p.SourceID)
	if err != nil {
		return 0, err
	}

	candidates := append(cooccurring, similar...)
	if len(candidates) == 0 {
		return 0, nil
	}

	h.report(ctx, p, fmt.Sprintf("Judging %d possible relationships", len(candidates)), progressJudging)

	accepted, err := h.extractor.Judge(ctx, candidates)
	if err != nil {
		return 0, err
	}

	written, err := h.store.SaveRelationships(ctx, accepted)
	if err != nil {
		return 0, err
	}

	h.logger.Info("relationships judged",
		slog.String("source_id", p.SourceID),
		slog.Int("candidates", len(candidates)),
		slog.Int("cooccurrence", len(cooccurring)),
		slog.Int("similarity", len(similar)),
		slog.Int("accepted", len(accepted)),
		slog.Int("written", written))
	return written, nil
}

// embedNewConcepts fills in embeddings for concepts that lack them.
//
// Embedding "name: summary" rather than the name alone: two concepts named
// similarly can mean different things, and the summary is what carries meaning.
func (h *ConceptHandler) embedNewConcepts(ctx context.Context) error {
	pending, err := h.store.ConceptsMissingEmbeddings(ctx, maxConceptsToEmbed)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}

	texts := make([]string, len(pending))
	for i, c := range pending {
		texts[i] = c.Name + ": " + c.Summary
	}

	vectors, err := h.embedder.EmbedBatch(ctx, texts)
	if err != nil {
		return fmt.Errorf("embed concepts: %w", err)
	}

	for i, c := range pending {
		if err := h.store.SetConceptEmbedding(ctx, c.ID, vectors[i]); err != nil {
			return err
		}
	}
	return nil
}

func (h *ConceptHandler) report(ctx context.Context, p ExtractPayload, stage string, progress int) {
	if err := h.store.UpdateJobProgress(ctx, p.JobID, stage, progress); err != nil {
		h.logger.Warn("could not record extraction progress",
			slog.String("job_id", p.JobID), slog.String("error", err.Error()))
	}
	h.publisher.Publish(p.JobID, events.Event{
		Type:     events.EventStatus,
		Stage:    stage,
		Progress: progress,
		SourceID: p.SourceID,
	})
}

// fail records a terminal extraction failure.
//
// The source itself stays "ready": its chunks and embeddings are intact and
// searchable. Only the graph is missing, and marking the document failed would
// misrepresent that.
func (h *ConceptHandler) fail(taskCtx context.Context, p ExtractPayload, cause error) {
	ctx := context.WithoutCancel(taskCtx)
	message := userFacingError(cause)

	h.logger.Error("concept extraction failed",
		slog.String("job_id", p.JobID),
		slog.String("source_id", p.SourceID),
		slog.String("error", message))

	if err := h.store.MarkJobFailed(ctx, p.JobID, message); err != nil {
		h.logger.Error("could not mark extraction job failed", slog.String("error", err.Error()))
	}

	h.publisher.Publish(p.JobID, events.Event{
		Type:     events.EventFailed,
		Stage:    "Extraction failed",
		Error:    message,
		SourceID: p.SourceID,
	})
}

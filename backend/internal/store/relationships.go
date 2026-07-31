package store

import (
	"context"
	"fmt"

	"github.com/emmyf/cortex/backend/internal/embed"
	"github.com/emmyf/cortex/backend/internal/extract"
)

// Candidate generation proposes pairs; the model decides which become edges.
//
// Two generators, because they find different things:
//
//   - Co-occurrence finds pairs discussed together in one passage. Cheap and
//     high-recall, but blind beyond the document.
//   - Embedding similarity finds pairs whose descriptions are close in meaning.
//     This is the only generator that can connect concepts read in *different*
//     documents, which is the link the product exists to surface.
//
// Both over-produce on purpose. Precision is the judge's job.

// Tuning for candidate generation.
//
// These numbers are a time budget as much as a quality one. Each batch of 8
// candidates is one local-model call at several seconds, so the total cap sets
// how long a document takes to graph: 400 co-occurrence plus 400 similarity
// candidates measured at ~13 minutes per document, which is not usable.
//
// The similarity floor matters independently. At 0.55 the nearest-neighbour
// search returned pairs whose only commonality was subject area — the judge
// then accepted several with reasons like "both are related to electric
// vehicles", which is precisely the relationship the prompt rejects. Raising
// the floor removes those pairs before they cost a judgement.
const (
	similarNeighbours = 3
	similarityFloor   = 0.62

	// Shared across both generators, not per-generator.
	maxCandidatesTotal = 240

	// Co-occurrence gets the larger share: pairs discussed together in one
	// passage are a stronger prior than pairs that merely describe similarly.
	cooccurrenceShare = 160
	similarityShare   = maxCandidatesTotal - cooccurrenceShare
)

// ConceptRow is a stored concept with the fields the judge needs.
type ConceptRow struct {
	ID      string
	Name    string
	Summary string
}

// ConceptsForSource returns the distinct concepts a source contributed.
func (s *Store) ConceptsForSource(ctx context.Context, sourceID string) ([]ConceptRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT c.id, c.name, c.summary
		 FROM concept_mentions m
		 JOIN concepts c ON c.id = m.concept_id
		 WHERE m.source_id = $1`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("load concepts for source: %w", err)
	}
	defer rows.Close()

	var concepts []ConceptRow
	for rows.Next() {
		var c ConceptRow
		if err := rows.Scan(&c.ID, &c.Name, &c.Summary); err != nil {
			return nil, fmt.Errorf("scan concept: %w", err)
		}
		concepts = append(concepts, c)
	}
	return concepts, rows.Err()
}

// SetConceptEmbedding stores a concept's embedding for similarity search.
func (s *Store) SetConceptEmbedding(ctx context.Context, conceptID string, vector []float32) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE concepts SET embedding = $2::vector WHERE id = $1`,
		conceptID, embed.Format(vector))
	if err != nil {
		return fmt.Errorf("set concept embedding: %w", err)
	}
	return nil
}

// ConceptsMissingEmbeddings returns concepts that still need embedding.
func (s *Store) ConceptsMissingEmbeddings(ctx context.Context, limit int) ([]ConceptRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, summary FROM concepts WHERE embedding IS NULL LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("find concepts missing embeddings: %w", err)
	}
	defer rows.Close()

	var concepts []ConceptRow
	for rows.Next() {
		var c ConceptRow
		if err := rows.Scan(&c.ID, &c.Name, &c.Summary); err != nil {
			return nil, fmt.Errorf("scan concept: %w", err)
		}
		concepts = append(concepts, c)
	}
	return concepts, rows.Err()
}

// CooccurrenceCandidates proposes pairs that share at least one passage in this
// source, strongest first.
func (s *Store) CooccurrenceCandidates(ctx context.Context, sourceID string) ([]extract.Candidate, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT a.id, a.name, a.summary, b.id, b.name, b.summary
		 FROM concept_mentions m1
		 JOIN concept_mentions m2
		   ON m1.chunk_id = m2.chunk_id AND m1.concept_id < m2.concept_id
		 JOIN concepts a ON a.id = m1.concept_id
		 JOIN concepts b ON b.id = m2.concept_id
		 WHERE m1.source_id = $1 AND m2.source_id = $1
		   AND NOT EXISTS (
		     SELECT 1 FROM concept_connections cc
		     WHERE cc.concept_a_id = m1.concept_id AND cc.concept_b_id = m2.concept_id)
		 GROUP BY a.id, a.name, a.summary, b.id, b.name, b.summary
		 ORDER BY count(*) DESC
		 LIMIT $2`, sourceID, cooccurrenceShare)
	if err != nil {
		return nil, fmt.Errorf("find co-occurrence candidates: %w", err)
	}
	defer rows.Close()

	return scanCandidates(rows, "cooccurrence")
}

// SimilarityCandidates proposes cross-document pairs: for each concept in this
// source, its nearest neighbours by meaning anywhere in the graph.
//
// The join deliberately excludes concepts from the same source — those are
// already covered by co-occurrence, and re-proposing them would spend the
// judge's budget on pairs it has seen.
func (s *Store) SimilarityCandidates(ctx context.Context, sourceID string) ([]extract.Candidate, error) {
	rows, err := s.pool.Query(ctx,
		`WITH source_concepts AS (
		   SELECT DISTINCT c.id, c.name, c.summary, c.embedding
		   FROM concept_mentions m
		   JOIN concepts c ON c.id = m.concept_id
		   WHERE m.source_id = $1 AND c.embedding IS NOT NULL
		 )
		 SELECT DISTINCT ON (least(sc.id, n.id), greatest(sc.id, n.id))
		        sc.id, sc.name, sc.summary, n.id, n.name, n.summary
		 FROM source_concepts sc
		 CROSS JOIN LATERAL (
		   SELECT c.id, c.name, c.summary
		   FROM concepts c
		   WHERE c.embedding IS NOT NULL
		     AND c.id <> sc.id
		     AND NOT EXISTS (
		       SELECT 1 FROM concept_mentions m2
		       WHERE m2.concept_id = c.id AND m2.source_id = $1)
		     AND 1 - (c.embedding <=> sc.embedding) >= $3
		   ORDER BY c.embedding <=> sc.embedding
		   LIMIT $2
		 ) n
		 WHERE NOT EXISTS (
		   SELECT 1 FROM concept_connections cc
		   WHERE cc.concept_a_id = least(sc.id, n.id)
		     AND cc.concept_b_id = greatest(sc.id, n.id))
		 LIMIT $4`,
		sourceID, similarNeighbours, similarityFloor, similarityShare)
	if err != nil {
		return nil, fmt.Errorf("find similarity candidates: %w", err)
	}
	defer rows.Close()

	return scanCandidates(rows, "similarity")
}

type candidateScanner interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func scanCandidates(rows candidateScanner, origin string) ([]extract.Candidate, error) {
	var candidates []extract.Candidate
	for rows.Next() {
		var c extract.Candidate
		if err := rows.Scan(&c.AID, &c.AName, &c.ASummary, &c.BID, &c.BName, &c.BSummary); err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		c.Origin = origin
		candidates = append(candidates, c)
	}
	return candidates, rows.Err()
}

// SaveRelationships writes judged edges.
//
// Only pairs a model accepted reach this point, and each carries the reason it
// gave — which is what relationship_summary was always meant to hold.
func (s *Store) SaveRelationships(ctx context.Context, relationships []extract.Relationship) (int, error) {
	if len(relationships) == 0 {
		return 0, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin relationships transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	written := 0
	for _, r := range relationships {
		// canonical_edge_order requires a < b; that ordering is what makes the
		// edge undirected.
		a, b := r.AID, r.BID
		if a > b {
			a, b = b, a
		}
		if a == b {
			continue
		}

		tag, err := tx.Exec(ctx,
			`INSERT INTO concept_connections (concept_a_id, concept_b_id, relationship_summary, origin)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (concept_a_id, concept_b_id) DO NOTHING`,
			a, b, r.Reason, r.Origin)
		if err != nil {
			return 0, fmt.Errorf("save relationship: %w", err)
		}
		written += int(tag.RowsAffected())
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit relationships: %w", err)
	}
	return written, nil
}

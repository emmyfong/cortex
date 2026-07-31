package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/emmyf/cortex/backend/internal/extract"
	"github.com/jackc/pgx/v5"
)

// PassageConcepts is what one chunk yielded.
type PassageConcepts struct {
	ChunkID     string
	HeadingPath string
	Concepts    []extract.Concept
}

// Concept is a graph node.
type Concept struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Slug            string `json:"slug"`
	Summary         string `json:"summary"`
	ConnectionCount int    `json:"connection_count"`
	MentionCount    int    `json:"mention_count,omitempty"`
}

// slugAttempts bounds the search for a free slug. Distinct concept names can
// slugify identically ("Solid-State Battery" and "Solid State Battery"), and
// slug is unique, so a suffix is needed to break the tie.
const slugAttempts = 20

// SaveExtraction persists concepts and their provenance for one source.
//
// It deliberately does not create edges. Co-occurrence alone produced a graph
// where 90% of edges rested on a single shared passage and every relationship
// summary was boilerplate; edges are now proposed as candidates and only
// written once a model has judged the pair related. See SaveRelationships.
//
// Everything here happens in one transaction: concepts without their mentions
// would leave provenance no one can reconstruct.
func (s *Store) SaveExtraction(ctx context.Context, sourceID, sourceTitle string, passages []PassageConcepts) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin extraction transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Re-extraction replaces this source's provenance. Concepts and edges
	// themselves are global knowledge and survive — another source may still
	// reference them.
	if _, err := tx.Exec(ctx, `DELETE FROM concept_mentions WHERE source_id = $1`, sourceID); err != nil {
		return fmt.Errorf("clear existing mentions: %w", err)
	}

	// Concept ids by lowercased name, so the same concept seen in several
	// passages resolves to one node.
	ids := make(map[string]string)

	for _, passage := range passages {
		for _, concept := range passage.Concepts {
			id, err := upsertConcept(ctx, tx, ids, concept)
			if err != nil {
				return err
			}

			if _, err := tx.Exec(ctx,
				`INSERT INTO concept_mentions (concept_id, chunk_id, source_id)
				 VALUES ($1, $2, $3)
				 ON CONFLICT (concept_id, chunk_id) DO NOTHING`,
				id, passage.ChunkID, sourceID); err != nil {
				return fmt.Errorf("record mention of %q: %w", concept.Name, err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit extraction: %w", err)
	}
	return nil
}

// upsertConcept resolves a concept name to an id, creating the node if needed.
func upsertConcept(ctx context.Context, tx pgx.Tx, cache map[string]string, concept extract.Concept) (string, error) {
	key := strings.ToLower(concept.Name)
	if id, ok := cache[key]; ok {
		return id, nil
	}

	// The unique index is on lower(name), so match the same way rather than
	// relying on the model to produce identical casing across passages.
	var id string
	err := tx.QueryRow(ctx,
		`SELECT id FROM concepts WHERE lower(name) = $1`, key).Scan(&id)
	if err == nil {
		cache[key] = id
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("look up concept %q: %w", concept.Name, err)
	}

	base := extract.Slug(concept.Name)
	for attempt := range slugAttempts {
		slug := base
		if attempt > 0 {
			slug = fmt.Sprintf("%s-%d", base, attempt+1)
		}

		// A savepoint keeps a slug collision from poisoning the outer
		// transaction: in Postgres any error aborts the whole transaction
		// unless it is rolled back to a savepoint first.
		nested, err := tx.Begin(ctx)
		if err != nil {
			return "", fmt.Errorf("open savepoint: %w", err)
		}

		err = nested.QueryRow(ctx,
			`INSERT INTO concepts (name, slug, summary) VALUES ($1, $2, $3) RETURNING id`,
			concept.Name, slug, concept.Summary).Scan(&id)
		if err == nil {
			if err := nested.Commit(ctx); err != nil {
				return "", fmt.Errorf("commit concept %q: %w", concept.Name, err)
			}
			cache[key] = id
			return id, nil
		}
		_ = nested.Rollback(ctx)

		if isUniqueViolation(err, "concepts_slug_key") {
			continue // try the next suffix
		}
		return "", fmt.Errorf("create concept %q: %w", concept.Name, err)
	}

	return "", fmt.Errorf("could not find a free slug for concept %q after %d attempts", concept.Name, slugAttempts)
}

// ListConcepts returns concepts ordered by how connected they are.
func (s *Store) ListConcepts(ctx context.Context, limit int) ([]Concept, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT c.id, c.name, c.slug, c.summary, c.connection_count,
		        (SELECT count(*) FROM concept_mentions m WHERE m.concept_id = c.id)
		 FROM concepts c
		 ORDER BY c.connection_count DESC, c.name ASC
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list concepts: %w", err)
	}
	defer rows.Close()

	var concepts []Concept
	for rows.Next() {
		var c Concept
		if err := rows.Scan(&c.ID, &c.Name, &c.Slug, &c.Summary, &c.ConnectionCount, &c.MentionCount); err != nil {
			return nil, fmt.Errorf("scan concept: %w", err)
		}
		concepts = append(concepts, c)
	}
	return concepts, rows.Err()
}

// GraphNode and GraphEdge are the force-graph payload.
type GraphNode struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Slug            string `json:"slug"`
	Summary         string `json:"summary"`
	ConnectionCount int    `json:"connection_count"`
}

type GraphEdge struct {
	Source  string `json:"source"`
	Target  string `json:"target"`
	Summary string `json:"summary,omitempty"`
}

type Graph struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

// LoadGraph returns the concept graph, capped at the most connected nodes.
//
// Edges are restricted to the returned nodes: a force-graph renderer given an
// edge whose endpoint is absent either crashes or silently invents a node.
func (s *Store) LoadGraph(ctx context.Context, limit int) (Graph, error) {
	graph := Graph{Nodes: []GraphNode{}, Edges: []GraphEdge{}}

	rows, err := s.pool.Query(ctx,
		`SELECT id, name, slug, summary, connection_count
		 FROM concepts ORDER BY connection_count DESC, name ASC LIMIT $1`, limit)
	if err != nil {
		return graph, fmt.Errorf("load graph nodes: %w", err)
	}
	defer rows.Close()

	included := make(map[string]bool)
	for rows.Next() {
		var n GraphNode
		if err := rows.Scan(&n.ID, &n.Name, &n.Slug, &n.Summary, &n.ConnectionCount); err != nil {
			return graph, fmt.Errorf("scan graph node: %w", err)
		}
		graph.Nodes = append(graph.Nodes, n)
		included[n.ID] = true
	}
	if err := rows.Err(); err != nil {
		return graph, err
	}
	if len(graph.Nodes) == 0 {
		return graph, nil
	}

	edgeRows, err := s.pool.Query(ctx,
		`SELECT concept_a_id, concept_b_id, COALESCE(relationship_summary, '')
		 FROM concept_connections`)
	if err != nil {
		return graph, fmt.Errorf("load graph edges: %w", err)
	}
	defer edgeRows.Close()

	for edgeRows.Next() {
		var e GraphEdge
		if err := edgeRows.Scan(&e.Source, &e.Target, &e.Summary); err != nil {
			return graph, fmt.Errorf("scan graph edge: %w", err)
		}
		if included[e.Source] && included[e.Target] {
			graph.Edges = append(graph.Edges, e)
		}
	}
	return graph, edgeRows.Err()
}

// ConceptDetail is a concept plus the passages that evidence it.
type ConceptDetail struct {
	Concept  Concept          `json:"concept"`
	Mentions []ConceptMention `json:"mentions"`
	Related  []Concept        `json:"related"`
}

// ConceptMention cites where a concept came from.
type ConceptMention struct {
	ChunkID     string `json:"chunk_id"`
	Content     string `json:"content"`
	HeadingPath string `json:"heading_path,omitempty"`
	SourceID    string `json:"source_id"`
	SourceTitle string `json:"source_title"`
	SourceType  string `json:"source_type"`
	SourceURL   string `json:"source_url,omitempty"`
}

// GetConcept returns a concept with its evidence and neighbours.
func (s *Store) GetConcept(ctx context.Context, slug string) (ConceptDetail, error) {
	var detail ConceptDetail

	err := s.pool.QueryRow(ctx,
		`SELECT id, name, slug, summary, connection_count FROM concepts WHERE slug = $1`, slug).
		Scan(&detail.Concept.ID, &detail.Concept.Name, &detail.Concept.Slug,
			&detail.Concept.Summary, &detail.Concept.ConnectionCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return detail, fmt.Errorf("concept %q not found", slug)
	}
	if err != nil {
		return detail, fmt.Errorf("get concept: %w", err)
	}

	detail.Mentions, err = s.conceptMentions(ctx, detail.Concept.ID)
	if err != nil {
		return detail, err
	}
	detail.Related, err = s.relatedConcepts(ctx, detail.Concept.ID)
	if err != nil {
		return detail, err
	}
	return detail, nil
}

func (s *Store) conceptMentions(ctx context.Context, conceptID string) ([]ConceptMention, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT ch.id, ch.content, COALESCE(ch.heading_path, ''),
		        s.id, s.title, s.source_type, COALESCE(s.url_or_path, '')
		 FROM concept_mentions m
		 JOIN chunks ch ON ch.id = m.chunk_id
		 JOIN sources s ON s.id = m.source_id
		 WHERE m.concept_id = $1
		 ORDER BY s.created_at, ch.chunk_index`, conceptID)
	if err != nil {
		return nil, fmt.Errorf("load concept mentions: %w", err)
	}
	defer rows.Close()

	mentions := []ConceptMention{}
	for rows.Next() {
		var m ConceptMention
		if err := rows.Scan(&m.ChunkID, &m.Content, &m.HeadingPath,
			&m.SourceID, &m.SourceTitle, &m.SourceType, &m.SourceURL); err != nil {
			return nil, fmt.Errorf("scan mention: %w", err)
		}
		mentions = append(mentions, m)
	}
	return mentions, rows.Err()
}

// relatedConcepts walks edges in both directions, since the edge table stores
// each pair once in canonical order rather than twice.
func (s *Store) relatedConcepts(ctx context.Context, conceptID string) ([]Concept, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT c.id, c.name, c.slug, c.summary, c.connection_count
		 FROM concept_connections cc
		 JOIN concepts c ON c.id = CASE
		     WHEN cc.concept_a_id = $1 THEN cc.concept_b_id
		     ELSE cc.concept_a_id
		 END
		 WHERE cc.concept_a_id = $1 OR cc.concept_b_id = $1
		 ORDER BY c.connection_count DESC, c.name ASC`, conceptID)
	if err != nil {
		return nil, fmt.Errorf("load related concepts: %w", err)
	}
	defer rows.Close()

	related := []Concept{}
	for rows.Next() {
		var c Concept
		if err := rows.Scan(&c.ID, &c.Name, &c.Slug, &c.Summary, &c.ConnectionCount); err != nil {
			return nil, fmt.Errorf("scan related concept: %w", err)
		}
		related = append(related, c)
	}
	return related, rows.Err()
}

// CountConcepts reports how many concepts a source contributed mentions for.
func (s *Store) CountConceptsForSource(ctx context.Context, sourceID string) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx,
		`SELECT count(DISTINCT concept_id) FROM concept_mentions WHERE source_id = $1`,
		sourceID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count concepts for source: %w", err)
	}
	return count, nil
}

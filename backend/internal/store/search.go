package store

import (
	"context"
	"fmt"

	"github.com/emmyf/cortex/backend/internal/embed"
)

// SearchResult is one retrieved passage plus the source it came from.
type SearchResult struct {
	ChunkID     string  `json:"chunk_id"`
	Content     string  `json:"content"`
	HeadingPath string  `json:"heading_path,omitempty"`
	ChunkIndex  int     `json:"chunk_index"`
	Similarity  float64 `json:"similarity"`

	SourceID    string `json:"source_id"`
	SourceTitle string `json:"source_title"`
	SourceType  string `json:"source_type"`
	SourceURL   string `json:"source_url,omitempty"`
}

// SearchSimilar returns the k chunks nearest the query vector by cosine
// distance.
//
// <=> is pgvector's cosine distance operator and is what the HNSW index was
// built for; using a different operator here would silently fall back to a
// sequential scan. Distance runs 0 (identical) to 2 (opposite), so similarity
// is reported as 1 - distance to give the familiar 1..-1 range.
func (s *Store) SearchSimilar(ctx context.Context, query []float32, k int) ([]SearchResult, error) {
	if k < 1 {
		return nil, fmt.Errorf("k must be positive, got %d", k)
	}

	vector := embed.Format(query)

	rows, err := s.pool.Query(ctx,
		`SELECT c.id, c.content, COALESCE(c.heading_path, ''), c.chunk_index,
		        1 - (c.embedding <=> $1::vector) AS similarity,
		        s.id, s.title, s.source_type, COALESCE(s.url_or_path, '')
		 FROM chunks c
		 JOIN sources s ON s.id = c.source_id
		 WHERE c.embedding IS NOT NULL
		 ORDER BY c.embedding <=> $1::vector
		 LIMIT $2`, vector, k)
	if err != nil {
		return nil, fmt.Errorf("search chunks: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(
			&r.ChunkID, &r.Content, &r.HeadingPath, &r.ChunkIndex, &r.Similarity,
			&r.SourceID, &r.SourceTitle, &r.SourceType, &r.SourceURL,
		); err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate search results: %w", err)
	}
	return results, nil
}

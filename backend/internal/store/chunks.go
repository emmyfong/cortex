package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/emmyf/cortex/backend/internal/chunk"
	"github.com/emmyf/cortex/backend/internal/embed"
	"github.com/jackc/pgx/v5/pgconn"
)

// ReplaceChunks writes a source's chunks and embeddings in one transaction,
// removing any previous chunks for that source first.
//
// Doing this transactionally matters: a partial write would leave the source
// marked ready while holding only some of its content, and searches would
// silently miss the rest.
func (s *Store) ReplaceChunks(ctx context.Context, sourceID string, chunks []chunk.Chunk, vectors [][]float32) error {
	if len(chunks) != len(vectors) {
		return fmt.Errorf("have %d chunks but %d vectors", len(chunks), len(vectors))
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	// Rollback is a no-op once the transaction commits.
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM chunks WHERE source_id = $1`, sourceID); err != nil {
		return fmt.Errorf("clear existing chunks: %w", err)
	}

	for i, c := range chunks {
		_, err := tx.Exec(ctx,
			`INSERT INTO chunks (source_id, chunk_index, content, token_count, heading_path, embedding)
			 VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6::vector)`,
			sourceID, c.Index, c.Content, c.TokenCount, c.HeadingPath, embed.Format(vectors[i]))
		if err != nil {
			return fmt.Errorf("insert chunk %d: %w", c.Index, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit chunks: %w", err)
	}
	return nil
}

// CountChunks reports how many chunks a source has.
func (s *Store) CountChunks(ctx context.Context, sourceID string) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM chunks WHERE source_id = $1`, sourceID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count chunks: %w", err)
	}
	return count, nil
}

// isUniqueViolation reports whether err is a Postgres unique violation,
// optionally for a specific constraint or index.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	if pgErr.Code != "23505" { // unique_violation
		return false
	}
	return constraint == "" || pgErr.ConstraintName == constraint
}

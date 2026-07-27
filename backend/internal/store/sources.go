package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrDuplicateSource means an identical document has already been ingested.
var ErrDuplicateSource = errors.New("source already ingested")

type Source struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	SourceType string `json:"source_type"`
	URLOrPath  string `json:"url_or_path,omitempty"`
	Status     string `json:"status"`
}

// CreateSource inserts a pending source. Content arrives later: the row must
// exist before the worker starts so the job has something to point at.
func (s *Store) CreateSource(ctx context.Context, title, sourceType, urlOrPath string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO sources (title, source_type, url_or_path, status)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id`,
		title, sourceType, urlOrPath, SourcePending).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("create source: %w", err)
	}
	return id, nil
}

// SetSourceContent stores extracted markdown and its hash, marking the source
// processing.
//
// The hash is what makes re-ingesting the same document detectable. A unique
// violation here means a different source row already holds identical content.
func (s *Store) SetSourceContent(ctx context.Context, id, title, content string) error {
	sum := sha256.Sum256([]byte(content))

	_, err := s.pool.Exec(ctx,
		`UPDATE sources
		 SET raw_content = $2, content_hash = $3, title = COALESCE(NULLIF($4, ''), title),
		     status = $5
		 WHERE id = $1`,
		id, content, sum[:], title, SourceProcessing)
	if err != nil {
		if isUniqueViolation(err, "idx_sources_hash") {
			return ErrDuplicateSource
		}
		return fmt.Errorf("set source content: %w", err)
	}
	return nil
}

func (s *Store) MarkSourceReady(ctx context.Context, id string) error {
	return s.setSourceStatus(ctx, id, SourceReady)
}

func (s *Store) MarkSourceFailed(ctx context.Context, id string) error {
	return s.setSourceStatus(ctx, id, SourceFailed)
}

func (s *Store) setSourceStatus(ctx context.Context, id, status string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE sources SET status = $2 WHERE id = $1`, id, status); err != nil {
		return fmt.Errorf("set source status to %s: %w", status, err)
	}
	return nil
}

func (s *Store) GetSource(ctx context.Context, id string) (Source, error) {
	var src Source
	var urlOrPath *string

	err := s.pool.QueryRow(ctx,
		`SELECT id, title, source_type, url_or_path, status FROM sources WHERE id = $1`, id).
		Scan(&src.ID, &src.Title, &src.SourceType, &urlOrPath, &src.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Source{}, fmt.Errorf("source %s not found", id)
	}
	if err != nil {
		return Source{}, fmt.Errorf("get source: %w", err)
	}
	if urlOrPath != nil {
		src.URLOrPath = *urlOrPath
	}
	return src, nil
}

// ListSources returns recently ingested sources, newest first.
func (s *Store) ListSources(ctx context.Context, limit int) ([]Source, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, title, source_type, COALESCE(url_or_path, ''), status
		 FROM sources ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	defer rows.Close()

	var sources []Source
	for rows.Next() {
		var src Source
		if err := rows.Scan(&src.ID, &src.Title, &src.SourceType, &src.URLOrPath, &src.Status); err != nil {
			return nil, fmt.Errorf("scan source: %w", err)
		}
		sources = append(sources, src)
	}
	return sources, rows.Err()
}

// DeleteSource removes a source; chunks and mentions cascade.
func (s *Store) DeleteSource(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM sources WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete source: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("source %s not found", id)
	}
	return nil
}

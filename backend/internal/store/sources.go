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

	// HasFile tells the UI whether the original upload can be opened. The hash
	// itself stays server-side — it is a storage key, not something a client
	// needs, and exposing it would invite direct blob addressing.
	HasFile          bool   `json:"has_file"`
	OriginalFilename string `json:"original_filename,omitempty"`
	FileSize         int64  `json:"file_size,omitempty"`
}

// FileRef locates a source's stored original.
type FileRef struct {
	Hash     []byte
	Size     int64
	Filename string
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
		`SELECT id, title, source_type, COALESCE(url_or_path, ''), status,
		        file_hash IS NOT NULL, COALESCE(original_filename, ''), COALESCE(file_size, 0)
		 FROM sources ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	defer rows.Close()

	var sources []Source
	for rows.Next() {
		var src Source
		if err := rows.Scan(&src.ID, &src.Title, &src.SourceType, &src.URLOrPath, &src.Status,
			&src.HasFile, &src.OriginalFilename, &src.FileSize); err != nil {
			return nil, fmt.Errorf("scan source: %w", err)
		}
		sources = append(sources, src)
	}
	return sources, rows.Err()
}

// AttachFile records the stored original for a source.
func (s *Store) AttachFile(ctx context.Context, id string, hash []byte, size int64, filename string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sources SET file_hash = $2, file_size = $3, original_filename = NULLIF($4, '')
		 WHERE id = $1`, id, hash, size, filename)
	if err != nil {
		return fmt.Errorf("attach file to source: %w", err)
	}
	return nil
}

// GetFileRef returns a source's stored original, or ErrNoFile if it has none.
func (s *Store) GetFileRef(ctx context.Context, id string) (FileRef, error) {
	var (
		ref      FileRef
		hash     []byte
		size     *int64
		filename *string
	)

	err := s.pool.QueryRow(ctx,
		`SELECT file_hash, file_size, original_filename FROM sources WHERE id = $1`, id).
		Scan(&hash, &size, &filename)
	if errors.Is(err, pgx.ErrNoRows) {
		return FileRef{}, fmt.Errorf("source %s not found", id)
	}
	if err != nil {
		return FileRef{}, fmt.Errorf("get file reference: %w", err)
	}
	if hash == nil {
		return FileRef{}, ErrNoFile
	}

	ref.Hash = hash
	if size != nil {
		ref.Size = *size
	}
	if filename != nil {
		ref.Filename = *filename
	}
	return ref, nil
}

// ErrNoFile means the source has no stored original — the normal case for web
// sources, which are identified by URL instead.
var ErrNoFile = errors.New("source has no stored file")

// DeleteSource removes a source and reports the blob it referenced, if any.
//
// The hash is returned rather than deleted here because the blob is
// content-addressed and may be shared with other sources; the caller decides
// whether it is still referenced. Callers that ignore the return value simply
// leak a file, never corrupt a live one.
func (s *Store) DeleteSource(ctx context.Context, id string) ([]byte, error) {
	var hash []byte

	err := s.pool.QueryRow(ctx,
		`DELETE FROM sources WHERE id = $1 RETURNING file_hash`, id).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("source %s not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("delete source: %w", err)
	}
	return hash, nil
}

// IsBlobReferenced reports whether any source still points at a blob. Deleting
// a shared blob would silently break every other source using it.
func (s *Store) IsBlobReferenced(ctx context.Context, hash []byte) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM sources WHERE file_hash = $1)`, hash).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check blob references: %w", err)
	}
	return exists, nil
}

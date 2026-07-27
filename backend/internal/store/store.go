// Package store holds all database access for ingestion and search.
//
// Queries live here rather than in handlers or the worker so the SQL that
// depends on the schema's guarantees stays in one place.
package store

import (
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the repository facade over Postgres.
type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Pool exposes the underlying pool for callers that need a transaction.
func (s *Store) Pool() *pgxpool.Pool {
	return s.pool
}

// Source lifecycle states, mirroring the CHECK constraint on sources.status.
const (
	SourcePending    = "pending"
	SourceProcessing = "processing"
	SourceReady      = "ready"
	SourceFailed     = "failed"
)

// Job lifecycle states, mirroring the CHECK constraint on jobs.status.
const (
	JobQueued    = "queued"
	JobRunning   = "running"
	JobSucceeded = "succeeded"
	JobFailed    = "failed"
)

// Source types, mirroring the CHECK constraint on sources.source_type.
const (
	TypePDF     = "pdf"
	TypeWeb     = "web"
	TypeYouTube = "youtube"
)

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

type Job struct {
	ID       string
	SourceID string
	Status   string
	Stage    string
	Progress int
	Error    string
}

// CreateJob records a queued job. asynq owns retries and scheduling; this row
// is the durable status a reconnecting SSE client can read.
func (s *Store) CreateJob(ctx context.Context, sourceID string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO jobs (source_id, status, stage, progress)
		 VALUES ($1, $2, 'Queued', 0)
		 RETURNING id`, sourceID, JobQueued).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("create job: %w", err)
	}
	return id, nil
}

// UpdateJobProgress records the current stage. Progress is clamped rather than
// rejected: a caller miscomputing a percentage should not fail an ingest that
// is otherwise succeeding.
func (s *Store) UpdateJobProgress(ctx context.Context, id, stage string, progress int) error {
	progress = max(0, min(100, progress))

	_, err := s.pool.Exec(ctx,
		`UPDATE jobs SET status = $2, stage = $3, progress = $4 WHERE id = $1`,
		id, JobRunning, stage, progress)
	if err != nil {
		return fmt.Errorf("update job progress: %w", err)
	}
	return nil
}

func (s *Store) MarkJobSucceeded(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE jobs SET status = $2, stage = 'Complete', progress = 100, error = NULL
		 WHERE id = $1`, id, JobSucceeded)
	if err != nil {
		return fmt.Errorf("mark job succeeded: %w", err)
	}
	return nil
}

func (s *Store) MarkJobFailed(ctx context.Context, id, reason string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE jobs SET status = $2, error = $3 WHERE id = $1`, id, JobFailed, reason)
	if err != nil {
		return fmt.Errorf("mark job failed: %w", err)
	}
	return nil
}

func (s *Store) GetJob(ctx context.Context, id string) (Job, error) {
	var (
		job      Job
		stage    *string
		errorMsg *string
	)

	err := s.pool.QueryRow(ctx,
		`SELECT id, COALESCE(source_id::text, ''), status, stage, progress, error
		 FROM jobs WHERE id = $1`, id).
		Scan(&job.ID, &job.SourceID, &job.Status, &stage, &job.Progress, &errorMsg)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, fmt.Errorf("job %s not found", id)
	}
	if err != nil {
		return Job{}, fmt.Errorf("get job: %w", err)
	}

	if stage != nil {
		job.Stage = *stage
	}
	if errorMsg != nil {
		job.Error = *errorMsg
	}
	return job, nil
}

// IsTerminal reports whether a job has finished, either way. The SSE handler
// uses this to know when to stop streaming.
func (j Job) IsTerminal() bool {
	return j.Status == JobSucceeded || j.Status == JobFailed
}

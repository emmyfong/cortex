// Package events carries ingestion progress from the worker to any browser
// watching a job.
//
// The worker and the API are separate processes, so this cannot be an
// in-process channel: the goroutine producing progress and the handler
// streaming it never share memory. Redis pub/sub bridges them, and the durable
// jobs table covers the gap for a client that connects late or reconnects.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// Event types sent over SSE.
const (
	EventStatus   = "status"
	EventComplete = "complete"
	EventFailed   = "failed"
)

// Event is one progress update for a job.
type Event struct {
	Type     string `json:"type"`
	Stage    string `json:"stage,omitempty"`
	Progress int    `json:"progress"`
	SourceID string `json:"source_id,omitempty"`
	Chunks   int    `json:"chunks,omitempty"`
	Error    string `json:"error,omitempty"`
}

// IsTerminal reports whether this event ends the stream.
func (e Event) IsTerminal() bool {
	return e.Type == EventComplete || e.Type == EventFailed
}

const publishTimeout = 3 * time.Second

// Publisher pushes events. Used by the worker.
type Publisher struct {
	rdb    *redis.Client
	logger *slog.Logger
}

func NewPublisher(addr string, logger *slog.Logger) *Publisher {
	return &Publisher{
		rdb:    redis.NewClient(&redis.Options{Addr: addr}),
		logger: logger,
	}
}

func (p *Publisher) Close() error {
	return p.rdb.Close()
}

// Publish sends an event to whoever is watching this job.
//
// Failures are logged, never returned: progress delivery is best-effort, and an
// ingest that succeeded must not be reported as failed because a browser was
// not listening. The jobs table remains the source of truth.
func (p *Publisher) Publish(jobID string, event Event) {
	payload, err := json.Marshal(event)
	if err != nil {
		p.logger.Warn("could not encode event", slog.String("error", err.Error()))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
	defer cancel()

	if err := p.rdb.Publish(ctx, channel(jobID), payload).Err(); err != nil {
		p.logger.Warn("could not publish job event",
			slog.String("job_id", jobID), slog.String("error", err.Error()))
	}
}

// Subscriber receives events. Used by the API's SSE handler.
type Subscriber struct {
	rdb *redis.Client
}

func NewSubscriber(addr string) *Subscriber {
	return &Subscriber{rdb: redis.NewClient(&redis.Options{Addr: addr})}
}

func (s *Subscriber) Close() error {
	return s.rdb.Close()
}

// Subscribe streams events for a job until ctx is cancelled.
//
// The returned channel is closed when the subscription ends. The caller must
// cancel ctx to release the Redis connection.
func (s *Subscriber) Subscribe(ctx context.Context, jobID string) (<-chan Event, error) {
	pubsub := s.rdb.Subscribe(ctx, channel(jobID))

	// Confirm the subscription is live before returning, so a caller that
	// immediately reads does not miss events to a half-open connection.
	if _, err := pubsub.Receive(ctx); err != nil {
		_ = pubsub.Close()
		return nil, fmt.Errorf("subscribe to job %s: %w", jobID, err)
	}

	out := make(chan Event)
	go func() {
		defer close(out)
		defer func() { _ = pubsub.Close() }()

		for msg := range pubsub.Channel() {
			var event Event
			if err := json.Unmarshal([]byte(msg.Payload), &event); err != nil {
				continue
			}
			select {
			case out <- event:
			case <-ctx.Done():
				return
			}
		}
	}()

	return out, nil
}

func channel(jobID string) string {
	return "cortex:job:" + jobID
}

// Package queue wires the asynq client (producer, API side) and server
// (consumer, worker side).
//
// No task types are registered in M1. This exists to prove Redis connectivity
// and to give M2's ingestion tasks a settled place to land.
package queue

import (
	"context"
	"fmt"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

// concurrency caps in-flight tasks per worker process. Embedding is
// GPU-bound through a single Ollama instance, so a large pool would only
// queue up inside Ollama rather than add throughput.
const concurrency = 4

// RedisOpt builds the shared connection options for both client and server.
func RedisOpt(addr string) asynq.RedisClientOpt {
	return asynq.RedisClientOpt{Addr: addr}
}

// NewClient returns an asynq producer. Callers must Close it.
func NewClient(addr string) *asynq.Client {
	return asynq.NewClient(RedisOpt(addr))
}

// NewServer returns an asynq consumer with the project's concurrency and
// logging defaults applied.
func NewServer(addr string) *asynq.Server {
	return asynq.NewServer(RedisOpt(addr), asynq.Config{
		Concurrency: concurrency,
	})
}

// NewMux returns the task router. M2 registers handlers here.
func NewMux() *asynq.ServeMux {
	return asynq.NewServeMux()
}

// Ping verifies Redis is reachable. Used by the readiness probe.
func Ping(ctx context.Context, addr string) error {
	client := redis.NewClient(&redis.Options{Addr: addr})
	defer client.Close()

	if err := client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("ping redis: %w", err)
	}
	return nil
}

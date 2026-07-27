package httpx

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/emmyf/cortex/backend/internal/events"
	"github.com/emmyf/cortex/backend/internal/store"
	"github.com/go-chi/chi/v5"
)

// heartbeatInterval keeps proxies and browsers from closing an idle stream
// during a long embedding run that produces no events.
const heartbeatInterval = 20 * time.Second

// StreamHandler serves job progress over server-sent events.
type StreamHandler struct {
	Store      *store.Store
	Subscriber *events.Subscriber
	Logger     *slog.Logger
}

// Stream sends progress for one job until it finishes or the client leaves.
//
// It subscribes *before* reading current state from the database. Doing it the
// other way round leaves a window where the worker publishes between the read
// and the subscribe, and that update is lost forever — the stream would then
// hang on a job that had already completed.
func (h *StreamHandler) Stream() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jobID := chi.URLParam(r, "id")

		flusher, ok := w.(http.Flusher)
		if !ok {
			writeError(w, http.StatusInternalServerError, "streaming unsupported")
			return
		}

		ctx := r.Context()

		updates, err := h.Subscriber.Subscribe(ctx, jobID)
		if err != nil {
			h.Logger.Error("could not subscribe to job",
				slog.String("job_id", jobID), slog.String("error", err.Error()))
			writeError(w, http.StatusInternalServerError, "could not open stream")
			return
		}

		// Now that the subscription is live, replay current state. A client
		// reconnecting mid-ingest — or after it finished — gets the truth
		// instead of waiting for an event that may never come again.
		job, err := h.Store.GetJob(ctx, jobID)
		if err != nil {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		// Disable proxy buffering, which would otherwise hold events back.
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		writeEvent(w, flusher, eventFromJob(job))
		if job.IsTerminal() {
			return
		}

		heartbeat := time.NewTicker(heartbeatInterval)
		defer heartbeat.Stop()

		for {
			select {
			case <-ctx.Done():
				// The browser went away. Returning releases the subscription.
				return

			case event, open := <-updates:
				if !open {
					return
				}
				writeEvent(w, flusher, event)
				if event.IsTerminal() {
					return
				}

			case <-heartbeat.C:
				// SSE comment: ignored by the client, keeps the socket alive.
				fmt.Fprint(w, ": keep-alive\n\n")
				flusher.Flush()
			}
		}
	}
}

// eventFromJob converts persisted job state into the same shape the worker
// publishes, so a replayed state and a live update are indistinguishable.
func eventFromJob(job store.Job) events.Event {
	event := events.Event{
		Type:     events.EventStatus,
		Stage:    job.Stage,
		Progress: job.Progress,
		SourceID: job.SourceID,
		Error:    job.Error,
	}

	switch job.Status {
	case store.JobSucceeded:
		event.Type = events.EventComplete
		event.Progress = 100
	case store.JobFailed:
		event.Type = events.EventFailed
	}
	return event
}

// writeEvent emits one SSE frame.
//
// The payload is marshalled rather than interpolated: a stage string containing
// a quote or newline would otherwise produce malformed JSON, or break the frame
// entirely by injecting a blank line.
func writeEvent(w http.ResponseWriter, flusher http.Flusher, event events.Event) {
	payload, err := json.Marshal(event)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, payload)
	flusher.Flush()
}

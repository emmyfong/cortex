package httpx

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/emmyf/cortex/backend/internal/ingest"
	"github.com/emmyf/cortex/backend/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/hibiken/asynq"
)

// pdfMagic is the header every PDF starts with. Trusting the client's
// Content-Type or file extension would let any bytes through to the parser.
var pdfMagic = []byte("%PDF-")

// SourceHandler accepts documents and queues them for ingestion.
type SourceHandler struct {
	Store          *store.Store
	Queue          *asynq.Client
	Logger         *slog.Logger
	UploadDir      string
	MaxUploadBytes int64
}

type ingestResponse struct {
	JobID    string `json:"job_id"`
	SourceID string `json:"source_id"`
	Status   string `json:"status"`
}

type ingestURLRequest struct {
	URL string `json:"url"`
}

// CreateFromURL queues a web page for ingestion and returns immediately.
//
// The response is 202 Accepted, not 201: nothing has been ingested yet, only
// accepted for processing. The client follows the job stream for the outcome.
func (h *SourceHandler) CreateFromURL() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req ingestURLRequest
		if !decodeJSON(w, r, &req) {
			return
		}

		req.URL = strings.TrimSpace(req.URL)
		if req.URL == "" {
			writeError(w, http.StatusBadRequest, "url is required")
			return
		}

		// The URL is validated properly (scheme plus SSRF checks) inside the
		// parser; this is only a fast rejection of obvious nonsense.
		if !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {
			writeError(w, http.StatusBadRequest, "url must start with http:// or https://")
			return
		}

		h.enqueue(w, r, store.TypeWeb, req.URL, req.URL, "")
	}
}

// CreateFromUpload accepts a PDF upload and queues it.
func (h *SourceHandler) CreateFromUpload() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Cap the request body before parsing, so an oversized upload is
		// rejected rather than buffered.
		r.Body = http.MaxBytesReader(w, r.Body, h.MaxUploadBytes)

		if err := r.ParseMultipartForm(h.MaxUploadBytes); err != nil {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("upload too large or malformed (limit %d bytes)", h.MaxUploadBytes))
			return
		}

		file, header, err := r.FormFile("file")
		if err != nil {
			writeError(w, http.StatusBadRequest, "expected a file field named 'file'")
			return
		}
		defer file.Close()

		// Verify the magic bytes rather than the extension or Content-Type,
		// both of which the client controls.
		magic := make([]byte, len(pdfMagic))
		if _, err := io.ReadFull(file, magic); err != nil || string(magic) != string(pdfMagic) {
			writeError(w, http.StatusBadRequest, "file does not appear to be a PDF")
			return
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			writeError(w, http.StatusInternalServerError, "could not read upload")
			return
		}

		path, err := h.saveUpload(file)
		if err != nil {
			h.Logger.Error("could not save upload", slog.String("error", err.Error()))
			writeError(w, http.StatusInternalServerError, "could not save upload")
			return
		}

		// filepath.Base strips any directory components a crafted filename
		// carried, so it cannot escape into a path.
		title := strings.TrimSuffix(filepath.Base(header.Filename), filepath.Ext(header.Filename))

		h.enqueue(w, r, store.TypePDF, path, title, path)
	}
}

// saveUpload writes the upload to a temp file the worker will read and delete.
func (h *SourceHandler) saveUpload(src io.Reader) (string, error) {
	if err := os.MkdirAll(h.UploadDir, 0o755); err != nil {
		return "", fmt.Errorf("create upload directory: %w", err)
	}

	// The name is generated, never derived from the client's filename.
	dst, err := os.CreateTemp(h.UploadDir, "upload-*.pdf")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, io.LimitReader(src, h.MaxUploadBytes)); err != nil {
		_ = os.Remove(dst.Name())
		return "", fmt.Errorf("write upload: %w", err)
	}
	return dst.Name(), nil
}

// enqueue creates the source and job rows, then pushes the task.
func (h *SourceHandler) enqueue(w http.ResponseWriter, r *http.Request, sourceType, ref, title, cleanupPath string) {
	ctx := r.Context()

	sourceID, err := h.Store.CreateSource(ctx, title, sourceType, ref)
	if err != nil {
		h.Logger.Error("could not create source", slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "could not create source")
		return
	}

	jobID, err := h.Store.CreateJob(ctx, sourceID)
	if err != nil {
		h.Logger.Error("could not create job", slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "could not create job")
		return
	}

	task, err := ingest.NewTask(ingest.Payload{
		JobID:       jobID,
		SourceID:    sourceID,
		SourceType:  sourceType,
		Ref:         ref,
		TitleHint:   title,
		CleanupPath: cleanupPath,
	})
	if err == nil {
		_, err = h.Queue.EnqueueContext(ctx, task)
	}
	if err != nil {
		h.Logger.Error("could not enqueue ingest task", slog.String("error", err.Error()))
		// Record the failure so the row does not sit "queued" forever behind a
		// task that was never actually queued.
		_ = h.Store.MarkJobFailed(ctx, jobID, "could not queue for processing")
		_ = h.Store.MarkSourceFailed(ctx, sourceID)
		writeError(w, http.StatusInternalServerError, "could not queue document for processing")
		return
	}

	writeJSON(w, http.StatusAccepted, ingestResponse{
		JobID:    jobID,
		SourceID: sourceID,
		Status:   store.JobQueued,
	})
}

// List returns recently ingested sources.
func (h *SourceHandler) List() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sources, err := h.Store.ListSources(r.Context(), 100)
		if err != nil {
			h.Logger.Error("could not list sources", slog.String("error", err.Error()))
			writeError(w, http.StatusInternalServerError, "could not list sources")
			return
		}
		if sources == nil {
			sources = []store.Source{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"sources": sources})
	}
}

// Delete removes a source and everything derived from it.
func (h *SourceHandler) Delete() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if err := h.Store.DeleteSource(r.Context(), id); err != nil {
			writeError(w, http.StatusNotFound, "source not found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

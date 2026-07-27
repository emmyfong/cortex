package httpx

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/emmyf/cortex/backend/internal/blob"
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
	Blobs          *blob.Store
	Queue          *asynq.Client
	Logger         *slog.Logger
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

		h.enqueue(w, r, enqueueRequest{
			SourceType: store.TypeWeb,
			Title:      req.URL,
			URL:        req.URL,
		})
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

		// Store the original permanently, content-addressed. Unlike the
		// previous temp-file approach, this file is not deleted after
		// extraction — it is what makes the source viewable later.
		stored, err := h.Blobs.Put(file)
		if errors.Is(err, blob.ErrTooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "file is too large")
			return
		}
		if err != nil {
			h.Logger.Error("could not store upload", slog.String("error", err.Error()))
			writeError(w, http.StatusInternalServerError, "could not store upload")
			return
		}

		// filepath.Base strips any directory components a crafted filename
		// carried. The name is only ever metadata — the blob's location comes
		// from its hash, never from client input.
		filename := filepath.Base(header.Filename)
		title := strings.TrimSuffix(filename, filepath.Ext(filename))

		h.enqueue(w, r, enqueueRequest{
			SourceType: store.TypePDF,
			Title:      title,
			File: &fileUpload{
				Ref:      stored,
				Filename: filename,
			},
		})
	}
}

// enqueueRequest carries what the two ingest entry points have in common.
type enqueueRequest struct {
	SourceType string
	Title      string

	// URL is set for web sources; File for uploads. Exactly one applies.
	URL  string
	File *fileUpload
}

type fileUpload struct {
	Ref      blob.Ref
	Filename string
}

// enqueue creates the source and job rows, then pushes the task.
func (h *SourceHandler) enqueue(w http.ResponseWriter, r *http.Request, req enqueueRequest) {
	ctx := r.Context()

	// url_or_path holds a URL for web sources and stays NULL for uploads —
	// an upload has no meaningful path now that blobs are addressed by hash.
	sourceID, err := h.Store.CreateSource(ctx, req.Title, req.SourceType, req.URL)
	if err != nil {
		h.Logger.Error("could not create source", slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "could not create source")
		return
	}

	if req.File != nil {
		if err := h.Store.AttachFile(ctx, sourceID, req.File.Ref.Hash, req.File.Ref.Size, req.File.Filename); err != nil {
			h.Logger.Error("could not attach file to source", slog.String("error", err.Error()))
			writeError(w, http.StatusInternalServerError, "could not record upload")
			return
		}
	}

	jobID, err := h.Store.CreateJob(ctx, sourceID)
	if err != nil {
		h.Logger.Error("could not create job", slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "could not create job")
		return
	}

	payload := ingest.Payload{
		JobID:      jobID,
		SourceID:   sourceID,
		SourceType: req.SourceType,
		Ref:        req.URL,
		TitleHint:  req.Title,
	}
	if req.File != nil {
		payload.BlobHash = blob.HashToHex(req.File.Ref.Hash)
	}

	task, err := ingest.NewTask(payload)
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
		ctx := r.Context()
		id := chi.URLParam(r, "id")

		hash, err := h.Store.DeleteSource(ctx, id)
		if err != nil {
			writeError(w, http.StatusNotFound, "source not found")
			return
		}

		// Blobs are content-addressed and therefore shared: another source may
		// have uploaded the identical file. Only remove it once nothing points
		// at it. A leaked blob is recoverable; deleting a live one is not.
		if len(hash) > 0 {
			referenced, err := h.Store.IsBlobReferenced(ctx, hash)
			switch {
			case err != nil:
				h.Logger.Warn("could not check blob references; leaving file in place",
					slog.String("error", err.Error()))
			case referenced:
				h.Logger.Debug("blob still referenced by another source, keeping it")
			default:
				if err := h.Blobs.Delete(hash); err != nil {
					h.Logger.Warn("could not delete blob", slog.String("error", err.Error()))
				}
			}
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// File serves the original uploaded document.
//
// The blob's location is derived from a hash read out of the database, never
// from the request, so no path traversal is reachable here. The response is
// pinned to application/pdf with nosniff, because the alternative — letting a
// browser sniff a user-supplied file — is how an upload endpoint turns into
// stored XSS.
func (h *SourceHandler) File() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")

		ref, err := h.Store.GetFileRef(r.Context(), id)
		if errors.Is(err, store.ErrNoFile) {
			writeError(w, http.StatusNotFound, "this source has no stored file")
			return
		}
		if err != nil {
			writeError(w, http.StatusNotFound, "source not found")
			return
		}

		reader, err := h.Blobs.Open(ref.Hash)
		if errors.Is(err, blob.ErrNotFound) {
			// The row survived but the file did not — a backup restored
			// without the blob directory, or manual deletion.
			h.Logger.Error("blob missing for source",
				slog.String("source_id", id),
				slog.String("hash", blob.HashToHex(ref.Hash)))
			writeError(w, http.StatusNotFound, "the stored file is missing")
			return
		}
		if err != nil {
			h.Logger.Error("could not open blob", slog.String("error", err.Error()))
			writeError(w, http.StatusInternalServerError, "could not read stored file")
			return
		}
		defer reader.Close()

		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition",
			fmt.Sprintf("inline; filename=%q", safeFilename(ref.Filename)))
		if ref.Size > 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(ref.Size, 10))
		}
		// Content-addressed blobs are immutable, so they cache indefinitely.
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")

		if _, err := io.Copy(w, reader); err != nil {
			// Headers are already sent; the client most likely disconnected.
			h.Logger.Debug("blob stream interrupted", slog.String("error", err.Error()))
		}
	}
}

// safeFilename reduces a client-supplied filename to something safe to place in
// a quoted Content-Disposition value.
//
// Path separators are handled explicitly rather than via filepath.Base, which
// is OS-dependent: a Windows client's "C:\dir\file.pdf" keeps its directories
// on a Linux server but loses them on Windows. Behaviour here is the same
// everywhere.
func safeFilename(name string) string {
	// Take the segment after the last separator of either flavour.
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}

	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r < 32, r == 127: // control characters, including CR and LF
			return -1
		case r == '"': // would close the quoted header value
			return -1
		}
		return r
	}, name)

	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" || cleaned == "." || cleaned == ".." {
		return "document.pdf"
	}
	return cleaned
}

package store_test

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"

	"github.com/emmyf/cortex/backend/internal/chunk"
	"github.com/emmyf/cortex/backend/internal/config"
	"github.com/emmyf/cortex/backend/internal/db"
	"github.com/emmyf/cortex/backend/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

var testStore *store.Store

const dimensions = 768

// TestMain provisions the shared cortex_test database, the same one the schema
// tests use, so development data is never touched.
func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		os.Exit(m.Run())
	}

	config.LoadDotenv()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		base := os.Getenv("DATABASE_URL")
		if base == "" {
			fmt.Fprintln(os.Stderr, "skipping store integration tests: DATABASE_URL not set")
			os.Exit(m.Run())
		}
		slash := strings.LastIndex(base, "/")
		query := ""
		if q := strings.Index(base[slash+1:], "?"); q >= 0 {
			query = base[slash+1+q:]
		}
		dsn = base[:slash+1] + "cortex_test" + query
	}

	ctx := context.Background()
	if err := db.Migrate(ctx, dsn); err != nil {
		fmt.Fprintf(os.Stderr, "skipping store integration tests: %v\n", err)
		os.Exit(m.Run())
	}

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skipping store integration tests: %v\n", err)
		os.Exit(m.Run())
	}
	testStore = store.New(pool)

	code := m.Run()
	pool.Close()
	os.Exit(code)
}

func requireStore(t *testing.T) *store.Store {
	t.Helper()
	if testStore == nil {
		t.Skip("no test database available; run `npm run infra:up` first")
	}
	cleanup(t, testStore.Pool())
	return testStore
}

func cleanup(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`TRUNCATE master_notes, jobs, concept_mentions, concept_connections,
		          concepts, chunks, sources RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func randomVector() []float32 {
	v := make([]float32, dimensions)
	for i := range v {
		v[i] = rand.Float32()
	}
	return v
}

func makeChunks(n int) ([]chunk.Chunk, [][]float32) {
	chunks := make([]chunk.Chunk, n)
	vectors := make([][]float32, n)
	for i := range chunks {
		chunks[i] = chunk.Chunk{
			Index:       i,
			Content:     fmt.Sprintf("passage number %d", i),
			TokenCount:  10,
			HeadingPath: "Section",
		}
		vectors[i] = randomVector()
	}
	return chunks, vectors
}

func TestSourceLifecycle(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	id, err := st.CreateSource(ctx, "Test Doc", store.TypeWeb, "https://example.com")
	if err != nil {
		t.Fatalf("CreateSource: %v", err)
	}

	src, err := st.GetSource(ctx, id)
	if err != nil {
		t.Fatalf("GetSource: %v", err)
	}
	if src.Status != store.SourcePending {
		t.Errorf("initial status = %q, want %q", src.Status, store.SourcePending)
	}

	if err := st.SetSourceContent(ctx, id, "Better Title", "# Heading\n\nBody text."); err != nil {
		t.Fatalf("SetSourceContent: %v", err)
	}

	src, _ = st.GetSource(ctx, id)
	if src.Status != store.SourceProcessing {
		t.Errorf("status after content = %q, want %q", src.Status, store.SourceProcessing)
	}
	if src.Title != "Better Title" {
		t.Errorf("title = %q, want the parser-supplied title", src.Title)
	}

	if err := st.MarkSourceReady(ctx, id); err != nil {
		t.Fatalf("MarkSourceReady: %v", err)
	}
	src, _ = st.GetSource(ctx, id)
	if src.Status != store.SourceReady {
		t.Errorf("final status = %q, want %q", src.Status, store.SourceReady)
	}
}

// An empty title must not wipe the one already recorded at enqueue time.
func TestSetSourceContentKeepsExistingTitleWhenBlank(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	id, _ := st.CreateSource(ctx, "Original Title", store.TypePDF, "/tmp/x.pdf")
	if err := st.SetSourceContent(ctx, id, "", "content"); err != nil {
		t.Fatalf("SetSourceContent: %v", err)
	}

	src, _ := st.GetSource(ctx, id)
	if src.Title != "Original Title" {
		t.Errorf("title = %q, want it preserved", src.Title)
	}
}

// Re-ingesting identical content must be detectable rather than silently
// creating a duplicate copy of every chunk.
func TestSetSourceContentDetectsDuplicateContent(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	const body = "Identical content in both documents."

	first, _ := st.CreateSource(ctx, "First", store.TypeWeb, "https://example.com/a")
	if err := st.SetSourceContent(ctx, first, "First", body); err != nil {
		t.Fatalf("first SetSourceContent: %v", err)
	}

	second, _ := st.CreateSource(ctx, "Second", store.TypeWeb, "https://example.com/b")
	err := st.SetSourceContent(ctx, second, "Second", body)

	if !errors.Is(err, store.ErrDuplicateSource) {
		t.Errorf("error = %v, want ErrDuplicateSource", err)
	}
}

func TestReplaceChunksIsIdempotent(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	sourceID, _ := st.CreateSource(ctx, "Doc", store.TypeWeb, "https://example.com")
	chunks, vectors := makeChunks(5)

	if err := st.ReplaceChunks(ctx, sourceID, chunks, vectors); err != nil {
		t.Fatalf("first ReplaceChunks: %v", err)
	}
	// Re-running must replace, not append: a retried job would otherwise
	// double every passage and skew search results.
	if err := st.ReplaceChunks(ctx, sourceID, chunks, vectors); err != nil {
		t.Fatalf("second ReplaceChunks: %v", err)
	}

	count, err := st.CountChunks(ctx, sourceID)
	if err != nil {
		t.Fatalf("CountChunks: %v", err)
	}
	if count != 5 {
		t.Errorf("chunk count = %d, want 5 (chunks were appended, not replaced)", count)
	}
}

func TestReplaceChunksRejectsMismatchedVectors(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	sourceID, _ := st.CreateSource(ctx, "Doc", store.TypeWeb, "https://example.com")
	chunks, vectors := makeChunks(3)

	err := st.ReplaceChunks(ctx, sourceID, chunks, vectors[:2])

	if err == nil {
		t.Fatal("ReplaceChunks accepted 3 chunks with 2 vectors, want error")
	}
}

// A failure partway through must leave no chunks at all, rather than a source
// marked ready while holding only part of its content.
func TestReplaceChunksRollsBackOnFailure(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	sourceID, _ := st.CreateSource(ctx, "Doc", store.TypeWeb, "https://example.com")
	chunks, vectors := makeChunks(3)

	// A wrong-width vector fails at insert time, after earlier rows in the
	// same transaction have already been written.
	vectors[2] = make([]float32, 10)

	if err := st.ReplaceChunks(ctx, sourceID, chunks, vectors); err == nil {
		t.Fatal("ReplaceChunks accepted a malformed vector, want error")
	}

	count, _ := st.CountChunks(ctx, sourceID)
	if count != 0 {
		t.Errorf("%d chunks survived a failed transaction, want 0", count)
	}
}

func TestJobLifecycle(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	sourceID, _ := st.CreateSource(ctx, "Doc", store.TypeWeb, "https://example.com")
	jobID, err := st.CreateJob(ctx, sourceID)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	job, _ := st.GetJob(ctx, jobID)
	if job.Status != store.JobQueued {
		t.Errorf("initial status = %q, want %q", job.Status, store.JobQueued)
	}
	if job.IsTerminal() {
		t.Error("a queued job reported itself terminal")
	}

	if err := st.UpdateJobProgress(ctx, jobID, "Embedding", 50); err != nil {
		t.Fatalf("UpdateJobProgress: %v", err)
	}
	job, _ = st.GetJob(ctx, jobID)
	if job.Status != store.JobRunning || job.Progress != 50 || job.Stage != "Embedding" {
		t.Errorf("after update: status=%q stage=%q progress=%d",
			job.Status, job.Stage, job.Progress)
	}

	if err := st.MarkJobSucceeded(ctx, jobID); err != nil {
		t.Fatalf("MarkJobSucceeded: %v", err)
	}
	job, _ = st.GetJob(ctx, jobID)
	if !job.IsTerminal() || job.Progress != 100 {
		t.Errorf("succeeded job: terminal=%v progress=%d", job.IsTerminal(), job.Progress)
	}
}

// Progress is clamped, not rejected: a caller miscomputing a percentage must
// not fail an ingest that is otherwise succeeding.
func TestUpdateJobProgressClampsOutOfRange(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	sourceID, _ := st.CreateSource(ctx, "Doc", store.TypeWeb, "https://example.com")
	jobID, _ := st.CreateJob(ctx, sourceID)

	tests := []struct {
		name  string
		given int
		want  int
	}{
		{"above range", 150, 100},
		{"below range", -20, 0},
		{"in range", 42, 42},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := st.UpdateJobProgress(ctx, jobID, "Stage", tt.given); err != nil {
				t.Fatalf("UpdateJobProgress(%d): %v", tt.given, err)
			}
			job, _ := st.GetJob(ctx, jobID)
			if job.Progress != tt.want {
				t.Errorf("progress = %d, want %d", job.Progress, tt.want)
			}
		})
	}
}

func TestMarkJobFailedRecordsReason(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	sourceID, _ := st.CreateSource(ctx, "Doc", store.TypeWeb, "https://example.com")
	jobID, _ := st.CreateJob(ctx, sourceID)

	if err := st.MarkJobFailed(ctx, jobID, "ollama unreachable"); err != nil {
		t.Fatalf("MarkJobFailed: %v", err)
	}

	job, _ := st.GetJob(ctx, jobID)
	if job.Status != store.JobFailed {
		t.Errorf("status = %q, want %q", job.Status, store.JobFailed)
	}
	if job.Error != "ollama unreachable" {
		t.Errorf("error = %q, want the recorded reason", job.Error)
	}
	if !job.IsTerminal() {
		t.Error("a failed job must be terminal")
	}
}

func TestSearchSimilarRanksByDistance(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	sourceID, _ := st.CreateSource(ctx, "Doc", store.TypeWeb, "https://example.com")
	_ = st.SetSourceContent(ctx, sourceID, "Doc", "body")

	chunks, vectors := makeChunks(4)
	// Make chunk 2 an exact match for the query below.
	target := make([]float32, dimensions)
	target[0] = 1
	vectors[2] = target

	if err := st.ReplaceChunks(ctx, sourceID, chunks, vectors); err != nil {
		t.Fatalf("ReplaceChunks: %v", err)
	}

	results, err := st.SearchSimilar(ctx, target, 3)
	if err != nil {
		t.Fatalf("SearchSimilar: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}

	if results[0].Content != "passage number 2" {
		t.Errorf("top result = %q, want the exact match", results[0].Content)
	}
	if results[0].Similarity < 0.99 {
		t.Errorf("similarity to an identical vector = %v, want ~1", results[0].Similarity)
	}
	// Results must arrive sorted by descending similarity.
	for i := 1; i < len(results); i++ {
		if results[i].Similarity > results[i-1].Similarity {
			t.Errorf("results out of order at %d: %v > %v",
				i, results[i].Similarity, results[i-1].Similarity)
		}
	}
	if results[0].SourceTitle != "Doc" {
		t.Errorf("source title = %q, want it joined from sources", results[0].SourceTitle)
	}
}

func TestSearchSimilarRejectsNonPositiveK(t *testing.T) {
	st := requireStore(t)

	for _, k := range []int{0, -1} {
		if _, err := st.SearchSimilar(context.Background(), randomVector(), k); err == nil {
			t.Errorf("SearchSimilar(k=%d) = nil error, want error", k)
		}
	}
}

func TestListAndDeleteSources(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	first, _ := st.CreateSource(ctx, "One", store.TypeWeb, "https://example.com/1")
	_, _ = st.CreateSource(ctx, "Two", store.TypePDF, "/tmp/two.pdf")

	sources, err := st.ListSources(ctx, 10)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("listed %d sources, want 2", len(sources))
	}

	// DeleteSource returns the blob hash the row referenced, so the caller can
	// decide whether the file on disk is still needed.
	hash, err := st.DeleteSource(ctx, first)
	if err != nil {
		t.Fatalf("DeleteSource: %v", err)
	}
	if hash != nil {
		t.Errorf("hash = %x for a source with no file, want nil", hash)
	}

	sources, _ = st.ListSources(ctx, 10)
	if len(sources) != 1 {
		t.Errorf("after delete, listed %d sources, want 1", len(sources))
	}

	if _, err := st.DeleteSource(ctx, first); err == nil {
		t.Error("deleting a missing source returned nil, want an error")
	}
}

func TestFileAttachmentLifecycle(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	hash := bytes.Repeat([]byte{0xAB}, 32)

	id, _ := st.CreateSource(ctx, "Paper", store.TypePDF, "")

	// A source with no attached file must report that clearly, not return a
	// zero-valued reference that looks real.
	if _, err := st.GetFileRef(ctx, id); !errors.Is(err, store.ErrNoFile) {
		t.Errorf("GetFileRef before attach = %v, want ErrNoFile", err)
	}

	if err := st.AttachFile(ctx, id, hash, 2048, "paper.pdf"); err != nil {
		t.Fatalf("AttachFile: %v", err)
	}

	ref, err := st.GetFileRef(ctx, id)
	if err != nil {
		t.Fatalf("GetFileRef: %v", err)
	}
	if !bytes.Equal(ref.Hash, hash) {
		t.Errorf("hash = %x, want %x", ref.Hash, hash)
	}
	if ref.Size != 2048 || ref.Filename != "paper.pdf" {
		t.Errorf("ref = %+v, want size 2048 and filename paper.pdf", ref)
	}

	sources, _ := st.ListSources(ctx, 10)
	if len(sources) != 1 || !sources[0].HasFile {
		t.Errorf("ListSources did not report HasFile: %+v", sources)
	}
}

// Blobs are content-addressed and therefore shared. Deleting one source must
// not orphan a file another source still points at.
func TestIsBlobReferencedTracksSharing(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	hash := bytes.Repeat([]byte{0xCD}, 32)

	first, _ := st.CreateSource(ctx, "Copy A", store.TypePDF, "")
	second, _ := st.CreateSource(ctx, "Copy B", store.TypePDF, "")
	_ = st.AttachFile(ctx, first, hash, 100, "a.pdf")
	_ = st.AttachFile(ctx, second, hash, 100, "b.pdf")

	if _, err := st.DeleteSource(ctx, first); err != nil {
		t.Fatalf("delete first: %v", err)
	}

	referenced, err := st.IsBlobReferenced(ctx, hash)
	if err != nil {
		t.Fatalf("IsBlobReferenced: %v", err)
	}
	if !referenced {
		t.Error("blob reported unreferenced while a second source still uses it")
	}

	if _, err := st.DeleteSource(ctx, second); err != nil {
		t.Fatalf("delete second: %v", err)
	}

	referenced, _ = st.IsBlobReferenced(ctx, hash)
	if referenced {
		t.Error("blob still reported referenced after every source was deleted")
	}
}

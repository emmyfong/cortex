package db_test

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"

	"github.com/emmyf/cortex/backend/internal/config"
	"github.com/emmyf/cortex/backend/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testPool is shared across the package. Nil when integration tests are skipped.
var testPool *pgxpool.Pool

const embeddingDimensions = 768

// TestMain provisions a dedicated cortex_test database, migrates it, and runs
// the suite against it. A separate database keeps development data safe from a
// test run that drops and recreates schema.
func TestMain(m *testing.M) {
	// testing.Short() reads a flag, so flags must be parsed first. m.Run() would
	// do it, but the short check has to happen before we touch a database.
	flag.Parse()

	if testing.Short() {
		os.Exit(m.Run())
	}

	// A test binary runs from its own package directory, so the repo-root .env
	// is not in scope unless we go looking for it. Without this every
	// integration test below skips and the suite reports a hollow "ok".
	config.LoadDotenv()

	dsn, err := testDSN()
	if err != nil {
		fmt.Fprintf(os.Stderr, "skipping integration tests: %v\n", err)
		os.Exit(m.Run())
	}

	ctx := context.Background()
	if err := ensureTestDatabase(ctx, dsn); err != nil {
		fmt.Fprintf(os.Stderr, "skipping integration tests: %v\n", err)
		os.Exit(m.Run())
	}
	if err := db.Migrate(ctx, dsn); err != nil {
		fmt.Fprintf(os.Stderr, "migrate test database: %v\n", err)
		os.Exit(1)
	}

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect to test database: %v\n", err)
		os.Exit(1)
	}
	testPool = pool

	code := m.Run()
	pool.Close()
	os.Exit(code)
}

// testDSN derives the test database URL from DATABASE_URL by swapping the
// database name, unless TEST_DATABASE_URL is set explicitly.
func testDSN() (string, error) {
	if explicit := os.Getenv("TEST_DATABASE_URL"); explicit != "" {
		return explicit, nil
	}
	base := os.Getenv("DATABASE_URL")
	if base == "" {
		return "", fmt.Errorf("neither TEST_DATABASE_URL nor DATABASE_URL is set")
	}
	return swapDatabaseName(base, "cortex_test")
}

func swapDatabaseName(dsn, name string) (string, error) {
	slash := strings.LastIndex(dsn, "/")
	if slash < 0 {
		return "", fmt.Errorf("cannot locate database name in DSN")
	}
	rest := dsn[slash+1:]
	query := ""
	if q := strings.Index(rest, "?"); q >= 0 {
		query = rest[q:]
	}
	return dsn[:slash+1] + name + query, nil
}

// ensureTestDatabase creates cortex_test if it does not already exist.
func ensureTestDatabase(ctx context.Context, testURL string) error {
	adminURL, err := swapDatabaseName(testURL, "postgres")
	if err != nil {
		return err
	}
	admin, err := db.Connect(ctx, adminURL)
	if err != nil {
		return fmt.Errorf("connect to postgres database: %w", err)
	}
	defer admin.Close()

	var exists bool
	err = admin.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'cortex_test')`).Scan(&exists)
	if err != nil {
		return fmt.Errorf("check for test database: %w", err)
	}
	if exists {
		return nil
	}
	// CREATE DATABASE cannot run inside a transaction or take parameters.
	if _, err := admin.Exec(ctx, `CREATE DATABASE cortex_test`); err != nil {
		return fmt.Errorf("create test database: %w", err)
	}
	return nil
}

// requirePool skips a test when no database is available, so `go test -short`
// and a machine without Docker still get a green unit-test run.
func requirePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testPool == nil {
		t.Skip("no test database available; run `npm run infra:up` first")
	}
	return testPool
}

// cleanup truncates every table so each test starts from a known state.
func cleanup(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`TRUNCATE master_notes, jobs, concept_mentions, concept_connections,
		          concepts, chunks, sources RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func randomEmbedding() string {
	values := make([]string, embeddingDimensions)
	for i := range values {
		values[i] = fmt.Sprintf("%.6f", rand.Float64())
	}
	return "[" + strings.Join(values, ",") + "]"
}

func insertSource(t *testing.T, pool *pgxpool.Pool, title string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO sources (title, source_type, status)
		 VALUES ($1, 'pdf', 'ready') RETURNING id`, title).Scan(&id)
	if err != nil {
		t.Fatalf("insert source: %v", err)
	}
	return id
}

func insertConcept(t *testing.T, pool *pgxpool.Pool, name, slug string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO concepts (name, slug, summary)
		 VALUES ($1, $2, 'summary') RETURNING id`, name, slug).Scan(&id)
	if err != nil {
		t.Fatalf("insert concept %q: %v", name, err)
	}
	return id
}

// The core RAG guarantee: pgvector is installed, the column is the right width,
// and cosine-distance ordering works through the HNSW index.
func TestVectorSimilaritySearchOrdersByCosineDistance(t *testing.T) {
	pool := requirePool(t)
	cleanup(t, pool)
	ctx := context.Background()

	sourceID := insertSource(t, pool, "Vector Test Source")

	// Build an orthogonal-ish set: query matches chunk 0 exactly.
	target := make([]string, embeddingDimensions)
	for i := range target {
		target[i] = "0.0"
	}
	target[0] = "1.0"
	targetVec := "[" + strings.Join(target, ",") + "]"

	for i := range 5 {
		vec := randomEmbedding()
		if i == 0 {
			vec = targetVec
		}
		_, err := pool.Exec(ctx,
			`INSERT INTO chunks (source_id, chunk_index, content, token_count, embedding)
			 VALUES ($1, $2, $3, 10, $4::vector)`,
			sourceID, i, fmt.Sprintf("chunk %d", i), vec)
		if err != nil {
			t.Fatalf("insert chunk %d: %v", i, err)
		}
	}

	var topContent string
	var distance float64
	err := pool.QueryRow(ctx,
		`SELECT content, embedding <=> $1::vector AS distance
		 FROM chunks ORDER BY embedding <=> $1::vector LIMIT 1`, targetVec).
		Scan(&topContent, &distance)
	if err != nil {
		t.Fatalf("similarity query: %v", err)
	}

	if topContent != "chunk 0" {
		t.Errorf("nearest chunk = %q, want %q", topContent, "chunk 0")
	}
	if distance > 1e-6 {
		t.Errorf("distance to identical vector = %v, want ~0", distance)
	}
}

func TestChunkRejectsWrongEmbeddingDimensions(t *testing.T) {
	pool := requirePool(t)
	cleanup(t, pool)

	sourceID := insertSource(t, pool, "Dimension Test")

	_, err := pool.Exec(context.Background(),
		`INSERT INTO chunks (source_id, chunk_index, content, token_count, embedding)
		 VALUES ($1, 0, 'wrong width', 5, '[0.1,0.2,0.3]'::vector)`, sourceID)

	if err == nil {
		t.Fatal("inserting a 3-dimension embedding succeeded, want dimension error")
	}
}

// canonical_edge_order is what actually makes the graph undirected. Without it
// (A,B) and (B,A) both insert and the graph silently double-counts.
func TestConceptConnectionsAreUndirected(t *testing.T) {
	pool := requirePool(t)
	cleanup(t, pool)
	ctx := context.Background()

	first := insertConcept(t, pool, "Battery Degradation", "battery-degradation")
	second := insertConcept(t, pool, "Consumer Incentives", "consumer-incentives")

	low, high := first, second
	if low > high {
		low, high = high, low
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO concept_connections (concept_a_id, concept_b_id, relationship_summary)
		 VALUES ($1, $2, 'related')`, low, high); err != nil {
		t.Fatalf("insert canonical edge: %v", err)
	}

	t.Run("rejects reversed duplicate", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO concept_connections (concept_a_id, concept_b_id)
			 VALUES ($1, $2)`, high, low)
		if err == nil {
			t.Fatal("reversed edge inserted, want canonical_edge_order violation")
		}
		if !strings.Contains(err.Error(), "canonical_edge_order") {
			t.Errorf("error = %v, want canonical_edge_order violation", err)
		}
	})

	t.Run("rejects exact duplicate", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO concept_connections (concept_a_id, concept_b_id)
			 VALUES ($1, $2)`, low, high)
		if err == nil {
			t.Fatal("duplicate edge inserted, want unique_connection violation")
		}
	})

	t.Run("rejects self loop", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO concept_connections (concept_a_id, concept_b_id)
			 VALUES ($1, $1)`, low)
		if err == nil {
			t.Fatal("self loop inserted, want canonical_edge_order violation")
		}
	})
}

func TestConnectionCountMaintainedByTrigger(t *testing.T) {
	pool := requirePool(t)
	cleanup(t, pool)
	ctx := context.Background()

	first := insertConcept(t, pool, "Alpha", "alpha")
	second := insertConcept(t, pool, "Beta", "beta")
	low, high := first, second
	if low > high {
		low, high = high, low
	}

	countOf := func(id string) int {
		t.Helper()
		var count int
		if err := pool.QueryRow(ctx,
			`SELECT connection_count FROM concepts WHERE id = $1`, id).Scan(&count); err != nil {
			t.Fatalf("read connection_count: %v", err)
		}
		return count
	}

	if got := countOf(low); got != 0 {
		t.Fatalf("initial connection_count = %d, want 0", got)
	}

	var edgeID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO concept_connections (concept_a_id, concept_b_id)
		 VALUES ($1, $2) RETURNING id`, low, high).Scan(&edgeID); err != nil {
		t.Fatalf("insert edge: %v", err)
	}

	for _, id := range []string{low, high} {
		if got := countOf(id); got != 1 {
			t.Errorf("after insert, connection_count = %d, want 1", got)
		}
	}

	if _, err := pool.Exec(ctx, `DELETE FROM concept_connections WHERE id = $1`, edgeID); err != nil {
		t.Fatalf("delete edge: %v", err)
	}

	for _, id := range []string{low, high} {
		if got := countOf(id); got != 0 {
			t.Errorf("after delete, connection_count = %d, want 0", got)
		}
	}
}

// An LLM will emit both casings for the same idea; they must collapse to one node.
func TestConceptNamesAreCaseInsensitivelyUnique(t *testing.T) {
	pool := requirePool(t)
	cleanup(t, pool)

	insertConcept(t, pool, "Battery Degradation", "battery-degradation")

	_, err := pool.Exec(context.Background(),
		`INSERT INTO concepts (name, slug, summary)
		 VALUES ('battery degradation', 'battery-degradation-2', 'dupe')`)

	if err == nil {
		t.Fatal("lowercase duplicate inserted, want unique violation on lower(name)")
	}
	if !strings.Contains(err.Error(), "idx_concepts_name_ci") {
		t.Errorf("error = %v, want idx_concepts_name_ci violation", err)
	}
}

func TestUpdatedAtTriggerFires(t *testing.T) {
	pool := requirePool(t)
	cleanup(t, pool)
	ctx := context.Background()

	sourceID := insertSource(t, pool, "Timestamp Test")

	var before, after string
	if err := pool.QueryRow(ctx,
		`SELECT updated_at::text FROM sources WHERE id = $1`, sourceID).Scan(&before); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE sources SET status = 'processing' WHERE id = $1`, sourceID); err != nil {
		t.Fatalf("update source: %v", err)
	}

	if err := pool.QueryRow(ctx,
		`SELECT updated_at::text FROM sources WHERE id = $1`, sourceID).Scan(&after); err != nil {
		t.Fatalf("re-read updated_at: %v", err)
	}

	if before == after {
		t.Errorf("updated_at unchanged after UPDATE (%s); trigger did not fire", after)
	}
}

func TestDeletingSourceCascadesToChunksAndMentions(t *testing.T) {
	pool := requirePool(t)
	cleanup(t, pool)
	ctx := context.Background()

	sourceID := insertSource(t, pool, "Cascade Test")
	conceptID := insertConcept(t, pool, "Cascading Concept", "cascading-concept")

	var chunkID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO chunks (source_id, chunk_index, content, token_count, embedding)
		 VALUES ($1, 0, 'body', 3, $2::vector) RETURNING id`,
		sourceID, randomEmbedding()).Scan(&chunkID); err != nil {
		t.Fatalf("insert chunk: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO concept_mentions (concept_id, chunk_id, source_id)
		 VALUES ($1, $2, $3)`, conceptID, chunkID, sourceID); err != nil {
		t.Fatalf("insert mention: %v", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM sources WHERE id = $1`, sourceID); err != nil {
		t.Fatalf("delete source: %v", err)
	}

	for _, tc := range []struct{ name, query string }{
		{"chunks", `SELECT count(*) FROM chunks WHERE source_id = $1`},
		{"concept_mentions", `SELECT count(*) FROM concept_mentions WHERE source_id = $1`},
	} {
		var count int
		if err := pool.QueryRow(ctx, tc.query, sourceID).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", tc.name, err)
		}
		if count != 0 {
			t.Errorf("%s rows remaining after source delete = %d, want 0", tc.name, count)
		}
	}

	// The concept itself must survive: deleting a document should not delete
	// an idea that other documents may also reference.
	var conceptCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM concepts WHERE id = $1`, conceptID).Scan(&conceptCount); err != nil {
		t.Fatalf("count concepts: %v", err)
	}
	if conceptCount != 1 {
		t.Errorf("concept rows after source delete = %d, want 1", conceptCount)
	}
}

func TestSourceContentHashUniqueOnlyWhenPresent(t *testing.T) {
	pool := requirePool(t)
	cleanup(t, pool)
	ctx := context.Background()

	// Many rows may sit with a NULL hash while awaiting extraction.
	for i := range 3 {
		if _, err := pool.Exec(ctx,
			`INSERT INTO sources (title, source_type) VALUES ($1, 'web')`,
			fmt.Sprintf("pending %d", i)); err != nil {
			t.Fatalf("insert pending source %d: %v", i, err)
		}
	}

	hash := []byte("0123456789abcdef0123456789abcdef")
	if _, err := pool.Exec(ctx,
		`INSERT INTO sources (title, source_type, content_hash)
		 VALUES ('first', 'web', $1)`, hash); err != nil {
		t.Fatalf("insert hashed source: %v", err)
	}

	_, err := pool.Exec(ctx,
		`INSERT INTO sources (title, source_type, content_hash)
		 VALUES ('duplicate', 'web', $1)`, hash)
	if err == nil {
		t.Fatal("duplicate content_hash inserted, want unique violation")
	}
}

func TestChunkIndexUniquePerSource(t *testing.T) {
	pool := requirePool(t)
	cleanup(t, pool)
	ctx := context.Background()

	first := insertSource(t, pool, "Source One")
	second := insertSource(t, pool, "Source Two")

	insert := func(sourceID string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO chunks (source_id, chunk_index, content, token_count)
			 VALUES ($1, 0, 'body', 3)`, sourceID)
		return err
	}

	if err := insert(first); err != nil {
		t.Fatalf("insert first chunk: %v", err)
	}
	// Same index under a different source is legitimate.
	if err := insert(second); err != nil {
		t.Fatalf("insert chunk for second source: %v", err)
	}
	if err := insert(first); err == nil {
		t.Fatal("duplicate (source_id, chunk_index) inserted, want unique violation")
	}
}

func TestJobProgressBoundsEnforced(t *testing.T) {
	pool := requirePool(t)
	cleanup(t, pool)
	ctx := context.Background()

	sourceID := insertSource(t, pool, "Job Test")

	tests := []struct {
		name     string
		progress int
		wantErr  bool
	}{
		{"lower bound", 0, false},
		{"upper bound", 100, false},
		{"below range", -1, true},
		{"above range", 101, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := pool.Exec(ctx,
				`INSERT INTO jobs (source_id, progress) VALUES ($1, $2)`,
				sourceID, tt.progress)

			if tt.wantErr && err == nil {
				t.Errorf("progress %d accepted, want check violation", tt.progress)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("progress %d rejected: %v", tt.progress, err)
			}
		})
	}
}

func TestStatusValuesConstrained(t *testing.T) {
	pool := requirePool(t)
	cleanup(t, pool)

	_, err := pool.Exec(context.Background(),
		`INSERT INTO sources (title, source_type, status)
		 VALUES ('bad status', 'pdf', 'not-a-real-status')`)

	if err == nil {
		t.Fatal("invalid status accepted, want check violation")
	}
}

func TestSourceTypeConstrained(t *testing.T) {
	pool := requirePool(t)
	cleanup(t, pool)

	_, err := pool.Exec(context.Background(),
		`INSERT INTO sources (title, source_type) VALUES ('bad type', 'spreadsheet')`)

	if err == nil {
		t.Fatal("invalid source_type accepted, want check violation")
	}
}

// Package integration exercises the parts of Cortex that only exist when the
// pieces run together: Redis pub/sub between two processes, the asynq task
// round trip, and the ingestion pipeline end to end.
//
// Unit tests cannot cover these — they are precisely the seams *between* units.
// Postgres and Redis are real (the compose stack); Ollama is stubbed, because
// the goal here is wiring, not model quality, and a real model would make the
// suite slow and non-deterministic.
package integration

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/emmyf/cortex/backend/internal/config"
	"github.com/emmyf/cortex/backend/internal/db"
	"github.com/emmyf/cortex/backend/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

const embeddingDimensions = 768

var (
	testPool  *pgxpool.Pool
	testStore *store.Store
	redisAddr string
)

// TestMain wires up the shared dependencies once. Every prerequisite is
// optional: a machine without Docker skips the suite rather than failing it,
// which keeps `npm run test:short` and a fresh clone green.
func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		os.Exit(m.Run())
	}

	config.LoadDotenv()
	ctx := context.Background()

	dsn, err := testDSN()
	if err != nil {
		fmt.Fprintf(os.Stderr, "skipping integration tests: %v\n", err)
		os.Exit(m.Run())
	}
	if err := db.Migrate(ctx, dsn); err != nil {
		fmt.Fprintf(os.Stderr, "skipping integration tests: %v\n", err)
		os.Exit(m.Run())
	}
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skipping integration tests: %v\n", err)
		os.Exit(m.Run())
	}
	testPool = pool
	testStore = store.New(pool)

	redisAddr = os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	if err := pingRedis(ctx, redisAddr); err != nil {
		fmt.Fprintf(os.Stderr, "skipping integration tests: redis unreachable: %v\n", err)
		testPool = nil
	}

	code := m.Run()
	pool.Close()
	os.Exit(code)
}

func testDSN() (string, error) {
	if explicit := os.Getenv("TEST_DATABASE_URL"); explicit != "" {
		return explicit, nil
	}
	base := os.Getenv("DATABASE_URL")
	if base == "" {
		return "", fmt.Errorf("neither TEST_DATABASE_URL nor DATABASE_URL is set")
	}
	slash := strings.LastIndex(base, "/")
	if slash < 0 {
		return "", fmt.Errorf("cannot locate database name in DSN")
	}
	rest := base[slash+1:]
	query := ""
	if q := strings.Index(rest, "?"); q >= 0 {
		query = rest[q:]
	}
	return base[:slash+1] + "cortex_test" + query, nil
}

func pingRedis(ctx context.Context, addr string) error {
	client := redis.NewClient(&redis.Options{Addr: addr})
	defer client.Close()

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return client.Ping(ctx).Err()
}

// requireStack skips when the compose stack is not running.
func requireStack(t *testing.T) *store.Store {
	t.Helper()
	if testPool == nil {
		t.Skip("integration stack unavailable; run `npm run infra:up` first")
	}
	truncate(t)
	return testStore
}

func truncate(t *testing.T) {
	t.Helper()
	_, err := testPool.Exec(context.Background(),
		`TRUNCATE master_notes, jobs, concept_mentions, concept_connections,
		          concepts, chunks, sources RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stubOllama serves both the embedding and the chat endpoints, so the pipeline
// can run end to end without a real model. Handlers are per-test so a case can
// make the model fail on demand.
type stubOllama struct {
	*httptest.Server

	// EmbedFail and ChatFail make the respective endpoint return an error.
	EmbedFail bool
	ChatFail  bool

	// Concepts is what the chat endpoint claims to have extracted.
	Concepts []map[string]string

	// JudgeRelated decides how the stub answers relationship judging. Edges no
	// longer follow from co-occurrence, so a test that wants edges has to say
	// so — which is the behaviour change being guarded.
	JudgeRelated bool
}

func newStubOllama(t *testing.T) *stubOllama {
	t.Helper()
	stub := &stubOllama{Concepts: []map[string]string{}}

	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/api/embed":
			if stub.EmbedFail {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "embedding model unavailable"})
				return
			}
			var req struct {
				Input []string `json:"input"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)

			vectors := make([][]float32, len(req.Input))
			for i := range vectors {
				v := make([]float32, embeddingDimensions)
				// A distinct first component per passage keeps vectors from
				// being identical, so ordering assertions mean something.
				v[0] = float32(i+1) / 100
				v[1] = 0.5
				vectors[i] = v
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": vectors})

		case "/api/chat":
			if stub.ChatFail {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "concept model unavailable"})
				return
			}

			var req struct {
				Messages []struct {
					Content string `json:"content"`
				} `json:"messages"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)

			// One endpoint serves two prompts: extracting concepts from a
			// passage, and judging whether a pair of concepts is related.
			prompt := ""
			if len(req.Messages) > 1 {
				prompt = req.Messages[1].Content
			}

			var payload []byte
			if strings.Contains(prompt, "Judge each pair") {
				payload, _ = json.Marshal(map[string]any{
					"judgements": stub.judgements(prompt),
				})
			} else {
				payload, _ = json.Marshal(map[string]any{"concepts": stub.Concepts})
			}

			_ = json.NewEncoder(w).Encode(map[string]any{
				"message": map[string]string{"role": "assistant", "content": string(payload)},
			})

		case "/api/tags":
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{}})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	t.Cleanup(stub.Close)
	return stub
}

// judgements answers one judging batch, one entry per numbered pair in the
// prompt, according to JudgeRelated.
func (s *stubOllama) judgements(prompt string) []map[string]any {
	pairs := strings.Count(prompt, "<->")

	out := make([]map[string]any, pairs)
	for i := range out {
		out[i] = map[string]any{
			"pair":    i + 1,
			"related": s.JudgeRelated,
			"reason":  "One is a mechanism of the other.",
		}
	}
	return out
}

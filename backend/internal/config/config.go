// Package config loads and validates service configuration from the environment.
//
// Configuration is required, not defaulted: a missing DATABASE_URL fails at
// startup rather than falling back to a guessed connection string. Secrets are
// never returned by String() or logged — use Redacted helpers when emitting
// configuration to logs or HTTP responses.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
)

// maxDotenvSearchDepth bounds the walk up the directory tree when locating .env,
// so a service started from a deep subdirectory still finds the repo-root file.
const maxDotenvSearchDepth = 4

type Config struct {
	DatabaseURL    string `env:"DATABASE_URL,required"`
	RedisAddr      string `env:"REDIS_ADDR,required"`
	OllamaURL      string `env:"OLLAMA_URL,required"`
	EmbeddingModel string `env:"EMBEDDING_MODEL" envDefault:"nomic-embed-text"`

	// ConceptModel extracts graph concepts. A local instruct model keeps this
	// free; it must support structured (JSON schema) output.
	ConceptModel     string `env:"CONCEPT_MODEL" envDefault:"llama3.1:8b"`
	APIPort          int    `env:"API_PORT" envDefault:"8080"`
	WorkerHealthPort int    `env:"WORKER_HEALTH_PORT" envDefault:"8081"`
	LogLevel         string `env:"LOG_LEVEL" envDefault:"info"`
	CORSOrigin       string `env:"CORS_ORIGIN" envDefault:"http://localhost:3000"`

	// Chunking. These are the single biggest lever on retrieval quality and
	// cannot be tuned correctly before searching real content, so they are
	// configuration rather than constants. Changing them requires a re-ingest.
	ChunkMaxTokens int `env:"CHUNK_MAX_TOKENS" envDefault:"600"`
	ChunkOverlap   int `env:"CHUNK_OVERLAP_TOKENS" envDefault:"50"`

	// Ingestion.
	PDFToTextPath  string `env:"PDFTOTEXT_PATH" envDefault:"pdftotext"`
	MaxUploadBytes int64  `env:"MAX_UPLOAD_BYTES" envDefault:"52428800"` // 50 MiB
	MaxFetchBytes  int64  `env:"MAX_FETCH_BYTES" envDefault:"10485760"`  // 10 MiB

	// Blob store. Original uploads are kept here so a source can be reopened
	// as it was received, not only as extracted text. Back this directory up
	// alongside the database — neither is complete without the other.
	BlobDir string `env:"BLOB_DIR" envDefault:"./data/blobs"`

	// Search.
	SearchDefaultK int `env:"SEARCH_DEFAULT_K" envDefault:"5"`
	SearchMaxK     int `env:"SEARCH_MAX_K" envDefault:"50"`
}

// Load reads .env (if present) then parses the environment. Real environment
// variables always win over .env, so production/CI can override without a file.
func Load() (Config, error) {
	LoadDotenv()

	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse environment: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// MustLoad loads configuration or exits non-zero with a readable message.
// Intended for main() only.
func MustLoad() Config {
	cfg, err := Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration error: %v\n\nCopy .env.example to .env and fill it in.\n", err)
		os.Exit(1)
	}
	return cfg
}

// LoadDotenv walks up from the working directory looking for a .env file and
// loads it into the environment. A missing file is not an error: the
// environment may already be populated.
//
// Exported so tests can populate the environment without requiring the full
// Config to validate — a test binary runs from its own package directory and
// would otherwise never see the repo-root .env.
func LoadDotenv() {
	dir, err := os.Getwd()
	if err != nil {
		return
	}
	for range maxDotenvSearchDepth {
		candidate := filepath.Join(dir, ".env")
		if _, statErr := os.Stat(candidate); statErr == nil {
			_ = godotenv.Load(candidate)
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
}

func (c Config) validate() error {
	var errs []error

	if _, err := url.Parse(c.OllamaURL); err != nil {
		errs = append(errs, fmt.Errorf("OLLAMA_URL is not a valid URL: %w", err))
	}
	// Parse rather than string-match so a malformed DSN is caught at startup
	// instead of on the first query.
	if _, err := url.Parse(c.DatabaseURL); err != nil {
		errs = append(errs, errors.New("DATABASE_URL is not a valid connection URL"))
	}
	if c.APIPort < 1 || c.APIPort > 65535 {
		errs = append(errs, fmt.Errorf("API_PORT out of range: %d", c.APIPort))
	}
	if c.WorkerHealthPort < 1 || c.WorkerHealthPort > 65535 {
		errs = append(errs, fmt.Errorf("WORKER_HEALTH_PORT out of range: %d", c.WorkerHealthPort))
	}
	if c.ChunkMaxTokens < 1 {
		errs = append(errs, fmt.Errorf("CHUNK_MAX_TOKENS must be positive, got %d", c.ChunkMaxTokens))
	}
	if c.ChunkOverlap < 0 {
		errs = append(errs, fmt.Errorf("CHUNK_OVERLAP_TOKENS cannot be negative, got %d", c.ChunkOverlap))
	}
	// Overlap at or above chunk size means each chunk re-emits everything the
	// previous one held, so the splitter would never advance.
	if c.ChunkOverlap >= c.ChunkMaxTokens {
		errs = append(errs, fmt.Errorf(
			"CHUNK_OVERLAP_TOKENS (%d) must be less than CHUNK_MAX_TOKENS (%d)",
			c.ChunkOverlap, c.ChunkMaxTokens))
	}
	if c.SearchDefaultK < 1 || c.SearchDefaultK > c.SearchMaxK {
		errs = append(errs, fmt.Errorf(
			"SEARCH_DEFAULT_K (%d) must be between 1 and SEARCH_MAX_K (%d)",
			c.SearchDefaultK, c.SearchMaxK))
	}

	return errors.Join(errs...)
}

// RedactedDatabaseURL returns the DSN with its password masked, safe for logs.
// If the DSN cannot be parsed it returns a constant rather than risking a leak.
func (c Config) RedactedDatabaseURL() string {
	return RedactURL(c.DatabaseURL)
}

// RedactURL masks the password in a URL-style connection string.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable-url-redacted>"
	}
	if u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			u.User = url.UserPassword(u.User.Username(), "xxxxx")
		}
	}
	return u.String()
}

// LogValue implements slog.LogValuer so that logging a Config never emits a
// password, no matter how it is interpolated at the call site.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("database_url", c.RedactedDatabaseURL()),
		slog.String("redis_addr", c.RedisAddr),
		slog.String("ollama_url", c.OllamaURL),
		slog.String("embedding_model", c.EmbeddingModel),
		slog.String("concept_model", c.ConceptModel),
		slog.Int("api_port", c.APIPort),
		slog.Int("worker_health_port", c.WorkerHealthPort),
		slog.String("log_level", c.LogLevel),
		slog.String("cors_origin", c.CORSOrigin),
	)
}

// String implements fmt.Stringer with the same redaction guarantee as LogValue.
func (c Config) String() string {
	return "Config" + c.LogValue().String()
}

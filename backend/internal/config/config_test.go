package config

import (
	"strings"
	"testing"
)

func TestRedactURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "masks password in postgres dsn",
			raw:  "postgres://cortex:supersecret@localhost:5432/cortex?sslmode=disable",
			want: "postgres://cortex:xxxxx@localhost:5432/cortex?sslmode=disable",
		},
		{
			name: "leaves dsn without password untouched",
			raw:  "postgres://cortex@localhost:5432/cortex",
			want: "postgres://cortex@localhost:5432/cortex",
		},
		{
			name: "leaves url without userinfo untouched",
			raw:  "http://localhost:11434",
			want: "http://localhost:11434",
		},
		{
			name: "redacts wholesale when url cannot be parsed",
			raw:  "postgres://user:pw@host:not-a-port/db",
			want: "<unparseable-url-redacted>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactURL(tt.raw)

			if got != tt.want {
				t.Errorf("RedactURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The whole point of redaction is that a password cannot reach a log sink,
// regardless of which formatting path the caller happens to use.
func TestConfigNeverExposesPasswordWhenFormatted(t *testing.T) {
	const password = "hunter2_should_never_appear"
	cfg := Config{
		DatabaseURL: "postgres://cortex:" + password + "@localhost:5432/cortex",
		RedisAddr:   "localhost:6379",
		OllamaURL:   "http://localhost:11434",
	}

	renderings := map[string]string{
		"String()":                cfg.String(),
		"LogValue()":              cfg.LogValue().String(),
		"RedactedDatabaseURL()":   cfg.RedactedDatabaseURL(),
	}

	for name, rendered := range renderings {
		if strings.Contains(rendered, password) {
			t.Errorf("%s leaked the password: %s", name, rendered)
		}
	}
}

func TestLoadAppliesDefaultsAndValidates(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr bool
		check   func(*testing.T, Config)
	}{
		{
			name: "applies defaults for optional values",
			env: map[string]string{
				"DATABASE_URL": "postgres://cortex:pw@localhost:5432/cortex",
				"REDIS_ADDR":   "localhost:6379",
				"OLLAMA_URL":   "http://localhost:11434",
			},
			check: func(t *testing.T, c Config) {
				if c.APIPort != 8080 {
					t.Errorf("APIPort = %d, want 8080", c.APIPort)
				}
				if c.EmbeddingModel != "nomic-embed-text" {
					t.Errorf("EmbeddingModel = %q, want nomic-embed-text", c.EmbeddingModel)
				}
				if c.CORSOrigin != "http://localhost:3000" {
					t.Errorf("CORSOrigin = %q, want http://localhost:3000", c.CORSOrigin)
				}
			},
		},
		{
			name: "rejects out-of-range port",
			env: map[string]string{
				"DATABASE_URL": "postgres://cortex:pw@localhost:5432/cortex",
				"REDIS_ADDR":   "localhost:6379",
				"OLLAMA_URL":   "http://localhost:11434",
				"API_PORT":     "70000",
			},
			wantErr: true,
		},
		{
			name: "rejects non-numeric port",
			env: map[string]string{
				"DATABASE_URL": "postgres://cortex:pw@localhost:5432/cortex",
				"REDIS_ADDR":   "localhost:6379",
				"OLLAMA_URL":   "http://localhost:11434",
				"API_PORT":     "not-a-number",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear inherited values so a developer's real environment cannot
			// change the outcome of the test.
			for _, key := range []string{
				"DATABASE_URL", "REDIS_ADDR", "OLLAMA_URL", "EMBEDDING_MODEL",
				"API_PORT", "WORKER_HEALTH_PORT", "LOG_LEVEL", "CORS_ORIGIN",
			} {
				t.Setenv(key, "")
			}
			for key, value := range tt.env {
				t.Setenv(key, value)
			}

			cfg, err := Load()

			if tt.wantErr {
				if err == nil {
					t.Fatal("Load() returned nil error, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v, want nil", err)
			}
			if tt.check != nil {
				tt.check(t, cfg)
			}
		})
	}
}

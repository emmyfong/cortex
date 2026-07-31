// Package db owns the Postgres connection pool and schema migrations.
//
// Migrations are embedded into the binary so cmd/migrate ships self-contained
// and cannot drift from the code that expects the schema.
package db

import (
	"context"
	"embed"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed all:migrations
var migrationsFS embed.FS

const (
	connectTimeout  = 10 * time.Second
	maxConns        = 10
	minConns        = 1
	maxConnLifetime = time.Hour
)

// Connect opens a pgx pool and verifies it with a ping, so a bad DSN or a
// database that is not yet accepting connections fails here rather than on the
// first real query.
//
// Errors are wrapped without the DSN: pgx error strings can otherwise carry the
// full connection string, password included, into logs.
func Connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("invalid database configuration: %w", sanitize(err, dsn))
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = minConns
	cfg.MaxConnLifetime = maxConnLifetime

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", sanitize(err, dsn))
	}

	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", sanitize(err, dsn))
	}
	return pool, nil
}

// Migrate applies all pending migrations against the given DSN.
func Migrate(ctx context.Context, dsn string) error {
	sqlDB, err := goose.OpenDBWithDriver("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open database for migration: %w", sanitize(err, dsn))
	}
	defer sqlDB.Close()

	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}
	if err := goose.UpContext(ctx, sqlDB, "migrations"); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// MigrateDown rolls back the most recent migration. Used by db:reset and tests.
func MigrateDown(ctx context.Context, dsn string) error {
	sqlDB, err := goose.OpenDBWithDriver("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open database for rollback: %w", sanitize(err, dsn))
	}
	defer sqlDB.Close()

	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}
	if err := goose.DownContext(ctx, sqlDB, "migrations"); err != nil {
		return fmt.Errorf("roll back migration: %w", err)
	}
	return nil
}

// Status writes the migration status table to stdout.
func Status(ctx context.Context, dsn string) error {
	sqlDB, err := goose.OpenDBWithDriver("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open database for status: %w", sanitize(err, dsn))
	}
	defer sqlDB.Close()

	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}
	return goose.StatusContext(ctx, sqlDB, "migrations")
}

// ensure the pgx stdlib driver is linked in for goose's database/sql usage.
var _ = stdlib.GetDefaultDriver

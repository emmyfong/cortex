// Command migrate applies, rolls back, or reports on database migrations.
//
//	migrate up      apply all pending migrations (default)
//	migrate down    roll back the most recent migration
//	migrate status  show which migrations have been applied
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/emmyf/cortex/backend/internal/config"
	"github.com/emmyf/cortex/backend/internal/db"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "migration failed: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.MustLoad()
	ctx := context.Background()

	command := "up"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}

	switch command {
	case "up":
		if err := db.Migrate(ctx, cfg.DatabaseURL); err != nil {
			return err
		}
		fmt.Println("migrations applied")
	case "down":
		if err := db.MigrateDown(ctx, cfg.DatabaseURL); err != nil {
			return err
		}
		fmt.Println("migration rolled back")
	case "status":
		return db.Status(ctx, cfg.DatabaseURL)
	default:
		return fmt.Errorf("unknown command %q (want: up, down, status)", command)
	}
	return nil
}

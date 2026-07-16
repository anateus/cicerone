package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

//go:embed schema/*.sql
var migrationFiles embed.FS

func migrate(ctx context.Context, db *sql.DB) error {
	entries, err := migrationFiles.ReadDir("schema")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var current int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&current); err != nil {
		return err
	}
	for _, entry := range entries {
		version, err := migrationVersion(entry.Name())
		if err != nil {
			return err
		}
		if version <= current {
			continue
		}
		body, err := migrationFiles.ReadFile("schema/" + entry.Name())
		if err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, string(body)); err == nil {
			_, err = tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, version))
		}
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: %w", entry.Name(), err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		current = version
	}
	return nil
}

func migrationVersion(name string) (int, error) {
	base := filepath.Base(name)
	parts := strings.SplitN(base, "_", 2)
	if len(parts) != 2 || !strings.HasSuffix(parts[1], ".sql") || strings.TrimSuffix(parts[1], ".sql") == "" {
		return 0, fmt.Errorf("malformed embedded migration filename %q: want NNN_description.sql", name)
	}
	version, err := strconv.Atoi(parts[0])
	if err != nil || version <= 0 {
		return 0, fmt.Errorf("malformed embedded migration filename %q: version must be a positive integer", name)
	}
	return version, nil
}

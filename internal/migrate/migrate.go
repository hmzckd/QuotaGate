package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

var migrationName = regexp.MustCompile(`^[0-9]{4}_[a-z0-9_]+\.sql$`)

// Run serializes migration runners and applies each file and its checksum in one transaction.
func Run(ctx context.Context, conn *pgx.Conn, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !migrationName.MatchString(entry.Name()) {
			continue
		}
		files = append(files, entry.Name())
	}
	if len(files) == 0 {
		return errors.New("no migration files found")
	}

	// The session lock protects setup and migrations from a second runner.
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(7401001)"); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, "SELECT pg_advisory_unlock(7401001)")
	}()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS public.schema_migrations (
		version text PRIMARY KEY,
		checksum text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}
	for _, file := range files {
		if err := apply(ctx, conn, filepath.Join(dir, file), file); err != nil {
			return err
		}
	}
	return nil
}

func apply(ctx context.Context, conn *pgx.Conn, path, version string) error {
	sql, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", version, err)
	}
	sum := sha256.Sum256(sql)
	checksum := hex.EncodeToString(sum[:])
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin %s: %w", version, err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var appliedChecksum string
	err = tx.QueryRow(ctx, "SELECT checksum FROM public.schema_migrations WHERE version = $1", version).Scan(&appliedChecksum)
	if err == nil {
		if appliedChecksum != checksum {
			return fmt.Errorf("migration %s changed after application", version)
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("check %s: %w", version, err)
	}
	if _, err := tx.Exec(ctx, string(sql)); err != nil {
		return fmt.Errorf("apply %s: %w", version, err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO public.schema_migrations (version, checksum) VALUES ($1, $2)", version, checksum); err != nil {
		return fmt.Errorf("record %s: %w", version, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit %s: %w", version, err)
	}
	return nil
}

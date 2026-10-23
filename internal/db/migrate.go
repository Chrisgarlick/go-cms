package db

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"cms/migrations"
)

// MigrationStatus is one row of `cms migrate status`.
type MigrationStatus struct {
	Version int64  `json:"version"`
	Name    string `json:"name"`
	Tag     string `json:"tag"`
	Applied bool   `json:"applied"`
}

// Migrate applies goose and River migrations as the migrator role, then
// (re)applies least-privilege grants to the runtime role (spec 7.2).
func Migrate(ctx context.Context, migratorURL, appRole string) ([]MigrationStatus, error) {
	pool, err := pgxpool.New(ctx, migratorURL)
	if err != nil {
		return nil, err
	}
	defer pool.Close()

	sqldb := stdlib.OpenDBFromPool(pool)
	defer sqldb.Close()
	p, err := provider(sqldb)
	if err != nil {
		return nil, err
	}
	if _, err := p.Up(ctx); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	rm, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return nil, err
	}
	if _, err := rm.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return nil, fmt.Errorf("migrate river: %w", err)
	}
	if appRole != "" {
		if err := ApplyGrants(ctx, pool, appRole); err != nil {
			return nil, err
		}
	}
	return Status(ctx, sqldb)
}

// Down rolls back one migration. Development only (spec 9.1).
func Down(ctx context.Context, migratorURL string) error {
	pool, err := pgxpool.New(ctx, migratorURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	sqldb := stdlib.OpenDBFromPool(pool)
	defer sqldb.Close()
	p, err := provider(sqldb)
	if err != nil {
		return err
	}
	_, err = p.Down(ctx)
	return err
}

func provider(sqldb *sql.DB) (*goose.Provider, error) {
	return goose.NewProvider(goose.DialectPostgres, sqldb, migrations.FS)
}

// Status reports every known migration and whether it is applied.
func Status(ctx context.Context, sqldb *sql.DB) ([]MigrationStatus, error) {
	p, err := provider(sqldb)
	if err != nil {
		return nil, err
	}
	st, err := p.Status(ctx)
	if err != nil {
		return nil, err
	}
	tags, err := Tags(migrations.FS)
	if err != nil {
		return nil, err
	}
	out := make([]MigrationStatus, 0, len(st))
	for _, s := range st {
		name := s.Source.Path
		out = append(out, MigrationStatus{
			Version: s.Source.Version,
			Name:    name,
			Tag:     tags[name],
			Applied: s.State == goose.StateApplied,
		})
	}
	return out, nil
}

// StatusURL opens a short-lived pool for Status.
func StatusURL(ctx context.Context, url string) ([]MigrationStatus, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	defer pool.Close()
	sqldb := stdlib.OpenDBFromPool(pool)
	defer sqldb.Close()
	return Status(ctx, sqldb)
}

// Tags returns each migration file's "+cms expand|contract" tag. A missing or
// unknown tag is an error: CI relies on every migration being classified.
func Tags(fsys fs.FS) (map[string]string, error) {
	files, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, err
	}
	tags := map[string]string{}
	for _, f := range files {
		fh, err := fsys.Open(f)
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(fh)
		tag := ""
		for i := 0; i < 5 && sc.Scan(); i++ {
			line := strings.TrimSpace(sc.Text())
			if rest, ok := strings.CutPrefix(line, "-- +cms "); ok {
				tag = strings.TrimSpace(rest)
				break
			}
		}
		fh.Close()
		if tag != "expand" && tag != "contract" {
			return nil, fmt.Errorf("migration %s: first lines must contain \"-- +cms expand\" or \"-- +cms contract\"", f)
		}
		tags[f] = tag
	}
	return tags, nil
}

// ApplyGrants gives the runtime role DML on every table, then removes UPDATE,
// DELETE and TRUNCATE on the audit log and its partitions so the log is
// append-only from the application's point of view.
func ApplyGrants(ctx context.Context, pool *pgxpool.Pool, appRole string) error {
	role := pgx.Identifier{appRole}.Sanitize()
	stmts := []string{
		"REVOKE CREATE ON SCHEMA public FROM PUBLIC",
		"GRANT USAGE ON SCHEMA public TO " + role,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO " + role,
		"GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO " + role,
		"REVOKE UPDATE, DELETE, TRUNCATE ON audit_log FROM " + role,
		"REVOKE ALL ON goose_db_version FROM " + role,
		"GRANT SELECT ON goose_db_version TO " + role,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			return fmt.Errorf("grants: %s: %w", s, err)
		}
	}
	rows, err := pool.Query(ctx, `SELECT c.relname FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		JOIN pg_class p ON p.oid = i.inhparent WHERE p.relname = 'audit_log'`)
	if err != nil {
		return err
	}
	parts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, part := range parts {
		if _, err := pool.Exec(ctx, "REVOKE ALL ON "+pgx.Identifier{part}.Sanitize()+" FROM "+role); err != nil {
			return err
		}
	}
	return nil
}

// Package db owns the connection pools, migrations, grants and transactions.
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"cms/internal/db/dbq"
)

// DB wraps the web pool. Jobs get a separate, smaller pool so background work
// can never starve requests (spec 8.3).
type DB struct {
	Pool *pgxpool.Pool
	Q    *dbq.Queries
}

type PoolOptions struct {
	MaxConns         int32
	StatementTimeout time.Duration
	AppName          string
}

func Open(ctx context.Context, url string, o PoolOptions) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("db: parse url: %w", err)
	}
	if o.MaxConns > 0 {
		cfg.MaxConns = o.MaxConns
	}
	cfg.MaxConnIdleTime = 5 * time.Minute
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	if o.StatementTimeout > 0 {
		cfg.ConnConfig.RuntimeParams["statement_timeout"] = fmt.Sprint(o.StatementTimeout.Milliseconds())
	}
	cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "60000"
	if o.AppName != "" {
		cfg.ConnConfig.RuntimeParams["application_name"] = o.AppName
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: connect: %w", err)
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return &DB{Pool: pool, Q: dbq.New(pool)}, nil
}

func (d *DB) Close() { d.Pool.Close() }

// Tx runs fn in a transaction. The function's error rolls back.
func (d *DB) Tx(ctx context.Context, fn func(tx pgx.Tx, q *dbq.Queries) error) error {
	return pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		return fn(tx, d.Q.WithTx(tx))
	})
}

// IsUniqueViolation reports a unique-constraint failure, optionally on a named constraint.
func IsUniqueViolation(err error, constraint string) bool {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return constraint == "" || pe.ConstraintName == constraint
	}
	return false
}

// IsNotFound reports pgx.ErrNoRows.
func IsNotFound(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

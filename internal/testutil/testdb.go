// Package testutil starts one embedded Postgres per test binary and hands out
// fresh, migrated databases copied from a template (fast: no re-migration).
package testutil

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cms/internal/db"
	"cms/internal/devdb"
)

var (
	once     sync.Once
	cluster  *devdb.Cluster
	startErr error
	counter  atomic.Int64
	baseDir  string
)

const template = "cms_template"

// Main wraps testing.M so the cluster is stopped and its files removed.
func Main(m *testing.M) {
	code := m.Run()
	if cluster != nil {
		_ = cluster.Stop()
	}
	if baseDir != "" {
		_ = os.RemoveAll(baseDir)
	}
	os.Exit(code)
}

func start() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	baseDir, startErr = os.MkdirTemp("", "cms-test-pg-")
	if startErr != nil {
		return
	}
	// Share downloaded binaries between runs.
	if home, err := os.UserHomeDir(); err == nil {
		_ = os.MkdirAll(filepath.Join(home, ".embedded-postgres-go"), 0o755)
	}
	cluster, startErr = devdb.Start(baseDir, 0, nil)
	if startErr != nil {
		return
	}
	if startErr = cluster.EnsureDatabase(ctx, template); startErr != nil {
		return
	}
	_, startErr = db.Migrate(ctx, cluster.MigratorURL(template), devdb.AppRole)
}

// DB is a fresh database for one test.
type DB struct {
	*db.DB
	AppURL      string
	MigratorURL string
	Name        string
}

// NewDB returns a migrated database connected as the least-privileged app role.
func NewDB(t testing.TB) *DB {
	t.Helper()
	if testing.Short() {
		t.Skip("database test skipped in -short mode")
	}
	once.Do(start)
	if startErr != nil {
		t.Fatalf("test postgres: %v", startErr)
	}
	ctx := context.Background()
	name := fmt.Sprintf("cms_t%d_%d", os.Getpid(), counter.Add(1))
	if err := cluster.CreateFromTemplate(ctx, name, template); err != nil {
		t.Fatalf("create db: %v", err)
	}
	d, err := db.Open(ctx, cluster.AppURL(name), db.PoolOptions{MaxConns: 8, StatementTimeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		d.Close()
		_ = cluster.DropDatabase(context.Background(), name)
	})
	return &DB{DB: d, AppURL: cluster.AppURL(name), MigratorURL: cluster.MigratorURL(name), Name: name}
}

package db_test

import (
	"context"
	"strings"
	"testing"

	"cms/internal/db"
	"cms/internal/testutil"
	"cms/migrations"
)

func TestMain(m *testing.M) { testutil.Main(m) }

func TestMigrationsTagged(t *testing.T) {
	tags, err := db.Tags(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) == 0 {
		t.Fatal("no migrations found")
	}
}

func TestAppRoleIsLeastPrivileged(t *testing.T) {
	d := testutil.NewDB(t)
	ctx := context.Background()
	// DDL must fail for the runtime role.
	if _, err := d.Pool.Exec(ctx, "CREATE TABLE nope (id int)"); err == nil {
		t.Fatal("app role could create a table")
	}
	// The audit log is append-only.
	_, err := d.Pool.Exec(ctx, `INSERT INTO audit_log (prev_hash, row_hash, actor_type, actor_id, action, resource_type, resource_id)
		VALUES ('\x00', '\x01', 'system', 'test', 'test', 'test', '1')`)
	if err != nil {
		t.Fatalf("insert audit: %v", err)
	}
	for _, q := range []string{"UPDATE audit_log SET action = 'x'", "DELETE FROM audit_log", "DELETE FROM audit_log_default"} {
		if _, err := d.Pool.Exec(ctx, q); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("%s: expected permission denied, got %v", q, err)
		}
	}
}

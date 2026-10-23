package audit_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"cms/internal/actor"
	"cms/internal/audit"
	"cms/internal/db/dbq"
	"cms/internal/devdb"
	"cms/internal/testutil"
)

func TestMain(m *testing.M) { testutil.Main(m) }

func TestChainDetectsTampering(t *testing.T) {
	d := testutil.NewDB(t)
	ctx := actor.With(context.Background(), actor.System("test"))
	for i := 0; i < 5; i++ {
		if err := audit.RecordTx(ctx, d.DB, audit.Event{Action: "entry.update", ResourceType: "entry", ResourceID: "e1",
			Diff: map[string]any{"fields": []string{"title"}, "n": i}}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := audit.Verify(ctx, d.Q)
	if err != nil || !res.OK || res.Rows != 5 {
		t.Fatalf("verify clean chain: %+v %v", res, err)
	}
	// Tamper as the migrator (the app role cannot).
	conn, err := pgx.Connect(ctx, d.MigratorURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "UPDATE audit_log SET action = 'entry.delete' WHERE id = 3"); err != nil {
		t.Fatal(err)
	}
	res, _ = audit.Verify(ctx, dbq.New(d.Pool))
	if res.OK || res.BrokenAt != 3 {
		t.Fatalf("tampering not detected: %+v", res)
	}
	_ = devdb.AppRole
}

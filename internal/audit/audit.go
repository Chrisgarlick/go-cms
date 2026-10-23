// Package audit writes the append-only, hash-chained audit log (spec 4.18).
// Each row's hash covers the previous row's hash, so any edit or deletion made
// outside the application (the runtime role cannot UPDATE/DELETE) is detected
// by Verify.
package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5"

	"cms/internal/actor"
	"cms/internal/db"
	"cms/internal/db/dbq"
)

var genesis = make([]byte, sha256.Size)

// Event is one audit record. Diff should hold field names and non-personal
// before/after values; never copy user profile fields into it.
type Event struct {
	Action       string
	ResourceType string
	ResourceID   string
	Diff         any
}

// Record appends an event inside tx, attributed to the actor in ctx. It takes a
// transaction-scoped advisory lock so the chain stays linear.
func Record(ctx context.Context, q *dbq.Queries, e Event) error {
	a := actor.From(ctx)
	if err := q.LockAuditChain(ctx); err != nil {
		return err
	}
	prev, err := q.LastAuditHash(ctx)
	if err != nil {
		if !db.IsNotFound(err) {
			return err
		}
		prev = genesis
	}
	diff := json.RawMessage("{}")
	if e.Diff != nil {
		b, err := json.Marshal(e.Diff)
		if err != nil {
			return fmt.Errorf("audit: diff: %w", err)
		}
		diff = b
	}
	// Postgres stores microseconds; truncate so the hash is reproducible.
	at := time.Now().UTC().Truncate(time.Microsecond)
	var ip *netip.Addr
	if a.IP.IsValid() {
		addr := a.IP
		ip = &addr
	}
	row := dbq.AuditLog{
		PrevHash: prev, ActorType: string(a.Kind), ActorID: a.ID(), ActorLabel: a.Label,
		Action: e.Action, ResourceType: e.ResourceType, ResourceID: e.ResourceID,
		Diff: diff, Ip: ip, CreatedAt: at,
	}
	row.RowHash = hash(row)
	return q.InsertAudit(ctx, dbq.InsertAuditParams{
		PrevHash: row.PrevHash, RowHash: row.RowHash, ActorType: row.ActorType, ActorID: row.ActorID,
		ActorLabel: row.ActorLabel, Action: row.Action, ResourceType: row.ResourceType,
		ResourceID: row.ResourceID, Diff: row.Diff, Ip: row.Ip, CreatedAt: row.CreatedAt,
	})
}

// RecordTx is Record for callers holding only a pool: it opens its own transaction.
func RecordTx(ctx context.Context, d *db.DB, e Event) error {
	return d.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error { return Record(ctx, q, e) })
}

func hash(r dbq.AuditLog) []byte {
	ip := ""
	if r.Ip != nil {
		ip = r.Ip.String()
	}
	// jsonb normalises key order and whitespace; re-encode for a stable form.
	var diff any
	_ = json.Unmarshal(r.Diff, &diff)
	canon, _ := json.Marshal([]any{
		r.ActorType, r.ActorID, r.ActorLabel, r.Action, r.ResourceType, r.ResourceID,
		diff, ip, r.CreatedAt.UTC().Format(time.RFC3339Nano),
	})
	h := sha256.New()
	h.Write(r.PrevHash)
	h.Write(canon)
	return h.Sum(nil)
}

// VerifyResult reports the outcome of a chain check.
type VerifyResult struct {
	Rows     int64  `json:"rows"`
	OK       bool   `json:"ok"`
	BrokenAt int64  `json:"broken_at,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// Verify walks the whole chain in id order.
func Verify(ctx context.Context, q *dbq.Queries) (VerifyResult, error) {
	res := VerifyResult{OK: true}
	prev := genesis
	var after int64
	for {
		rows, err := q.AuditChainPage(ctx, dbq.AuditChainPageParams{AfterID: after, Lim: 1000})
		if err != nil {
			return res, err
		}
		if len(rows) == 0 {
			return res, nil
		}
		for _, r := range rows {
			res.Rows++
			if !bytes.Equal(r.PrevHash, prev) {
				return VerifyResult{Rows: res.Rows, BrokenAt: r.ID, Reason: "previous hash mismatch (row deleted or reordered)"}, nil
			}
			if !bytes.Equal(hash(r), r.RowHash) {
				return VerifyResult{Rows: res.Rows, BrokenAt: r.ID, Reason: "row hash mismatch (row modified)"}, nil
			}
			prev = r.RowHash
			after = r.ID
		}
	}
}

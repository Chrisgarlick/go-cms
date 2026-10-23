-- name: LockAuditChain :exec
SELECT pg_advisory_xact_lock(7212001);

-- name: LastAuditHash :one
SELECT row_hash FROM audit_log ORDER BY id DESC LIMIT 1;

-- name: InsertAudit :exec
INSERT INTO audit_log (prev_hash, row_hash, actor_type, actor_id, actor_label, action,
    resource_type, resource_id, diff, ip, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11);

-- name: ListAudit :many
SELECT * FROM audit_log
WHERE (sqlc.narg(actor_id)::text IS NULL OR actor_id = sqlc.narg(actor_id))
  AND (sqlc.narg(resource_type)::text IS NULL OR resource_type = sqlc.narg(resource_type))
  AND (sqlc.narg(resource_id)::text IS NULL OR resource_id = sqlc.narg(resource_id))
  AND (sqlc.narg(since)::timestamptz IS NULL OR created_at >= sqlc.narg(since))
  AND (sqlc.narg(before_id)::bigint IS NULL OR id < sqlc.narg(before_id))
ORDER BY id DESC
LIMIT @lim;

-- name: AuditChainPage :many
SELECT * FROM audit_log WHERE id > @after_id ORDER BY id LIMIT @lim;

-- name: EnsureAuditPartitions :exec
SELECT cms_audit_ensure_partitions();

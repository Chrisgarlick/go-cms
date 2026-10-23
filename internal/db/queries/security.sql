-- name: UpsertFinding :one
INSERT INTO security_findings (id, category, severity, title, detail, remediation)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (id) DO UPDATE SET severity = EXCLUDED.severity, title = EXCLUDED.title, detail = EXCLUDED.detail,
    remediation = EXCLUDED.remediation, last_seen = now(), resolved_at = NULL
RETURNING (xmax = 0) AS inserted;

-- name: ResolveFindingsNotIn :execrows
UPDATE security_findings SET resolved_at = now()
WHERE resolved_at IS NULL AND category = ANY(@categories::text[]) AND NOT (id = ANY(@ids::text[]));

-- name: ListFindings :many
SELECT * FROM security_findings
WHERE (@include_resolved::boolean OR resolved_at IS NULL)
ORDER BY CASE severity WHEN 'critical' THEN 0 WHEN 'high' THEN 1 WHEN 'medium' THEN 2 WHEN 'low' THEN 3 ELSE 4 END, first_seen DESC;

-- name: AckFinding :exec
UPDATE security_findings SET ack_by = $2, ack_reason = $3, ack_until = $4 WHERE id = $1;

-- name: UsersWithout2FA :many
SELECT id, role FROM users WHERE status = 'active' AND NOT totp_confirmed;

-- name: StaleTokens :many
SELECT id, name FROM api_tokens WHERE revoked_at IS NULL AND expires_at > now()
  AND COALESCE(last_used_at, created_at) < now() - interval '90 days';

-- name: CreateWebhook :one
INSERT INTO webhooks (id, url, secret_enc, events, active) VALUES ($1, $2, $3, $4, true) RETURNING *;

-- name: ListWebhooks :many
SELECT * FROM webhooks ORDER BY created_at;

-- name: GetWebhook :one
SELECT * FROM webhooks WHERE id = $1;

-- name: DeleteWebhook :exec
DELETE FROM webhooks WHERE id = $1;

-- name: SetWebhookActive :exec
UPDATE webhooks SET active = $2 WHERE id = $1;

-- name: WebhooksFor :many
SELECT * FROM webhooks WHERE active AND (@event::text = ANY(events) OR '*' = ANY(events));

-- name: CreateDelivery :exec
INSERT INTO webhook_deliveries (id, webhook_id, event) VALUES ($1, $2, $3);

-- name: UpdateDelivery :exec
UPDATE webhook_deliveries SET status = $2, attempts = $3, response = $4 WHERE id = $1;

-- name: ListDeliveries :many
SELECT * FROM webhook_deliveries WHERE webhook_id = $1 ORDER BY created_at DESC LIMIT 50;

-- name: PruneDeliveries :execrows
DELETE FROM webhook_deliveries WHERE created_at < now() - interval '30 days';

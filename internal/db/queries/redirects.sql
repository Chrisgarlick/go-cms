-- name: CreateRedirect :one
INSERT INTO redirects (id, host, source, match_type, target, status_code, preserve_query, is_auto, note, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (host, source, match_type) DO UPDATE SET
    target = EXCLUDED.target, status_code = EXCLUDED.status_code, preserve_query = EXCLUDED.preserve_query,
    is_auto = EXCLUDED.is_auto, note = EXCLUDED.note
RETURNING *;

-- name: UpdateRedirect :one
UPDATE redirects SET host = $2, source = $3, match_type = $4, target = $5, status_code = $6,
    preserve_query = $7, note = $8
WHERE id = $1 RETURNING *;

-- name: GetRedirect :one
SELECT * FROM redirects WHERE id = $1;

-- name: ListRedirects :many
SELECT * FROM redirects ORDER BY created_at DESC;

-- name: DeleteRedirect :exec
DELETE FROM redirects WHERE id = $1;

-- name: DeleteRedirectBySource :exec
DELETE FROM redirects WHERE host = $1 AND source = $2 AND match_type = 'exact';

-- name: RetargetRedirects :execrows
-- Flatten chains: anything pointing at the old URL now points at the new one.
UPDATE redirects SET target = @new_target::text WHERE target = @old_target::text AND match_type = 'exact';

-- name: AddRedirectHits :exec
UPDATE redirects SET hits = hits + @n::bigint, last_hit_at = now() WHERE id = @id;

-- name: RecordNotFound :exec
INSERT INTO not_found_log (host, path, referrer, hits) VALUES ($1, $2, $3, @n::bigint)
ON CONFLICT (host, path) DO UPDATE SET hits = not_found_log.hits + EXCLUDED.hits,
    referrer = CASE WHEN EXCLUDED.referrer <> '' THEN EXCLUDED.referrer ELSE not_found_log.referrer END,
    last_seen = now();

-- name: TopNotFound :many
SELECT * FROM not_found_log ORDER BY hits DESC, last_seen DESC LIMIT $1;

-- name: DeleteNotFound :exec
DELETE FROM not_found_log WHERE host = $1 AND path = $2;

-- name: ClearNotFound :exec
DELETE FROM not_found_log;

-- name: PruneNotFound :execrows
DELETE FROM not_found_log WHERE last_seen < now() - (@age::text)::interval;

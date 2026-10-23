-- name: CreateMedia :one
INSERT INTO media (id, sha256, filename, mime, size, width, height, storage_key, alt, title, uploader_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- name: GetMedia :one
SELECT * FROM media WHERE id = $1 AND deleted_at IS NULL;

-- name: GetMediaBySHA :one
SELECT * FROM media WHERE sha256 = $1 AND deleted_at IS NULL LIMIT 1;

-- name: ListMedia :many
SELECT m.*, count(*) OVER () AS total,
    (SELECT count(*) FROM media_usage u WHERE u.media_id = m.id) AS usage_count
FROM media m
WHERE m.deleted_at IS NULL
  AND (sqlc.narg(q)::text IS NULL OR m.filename ILIKE '%' || sqlc.narg(q) || '%' OR m.alt ILIKE '%' || sqlc.narg(q) || '%' OR m.title ILIKE '%' || sqlc.narg(q) || '%')
  AND (sqlc.narg(kind)::text IS NULL OR m.mime LIKE sqlc.narg(kind) || '%')
  AND (NOT @unused::boolean OR NOT EXISTS (SELECT 1 FROM media_usage u WHERE u.media_id = m.id))
ORDER BY m.created_at DESC
LIMIT @lim OFFSET @off;

-- name: MediaByIDs :many
SELECT * FROM media WHERE id = ANY(@ids::uuid[]) AND deleted_at IS NULL;

-- name: UpdateMediaMeta :one
UPDATE media SET alt = $2, title = $3, caption = $4, credit = $5, focal_x = $6, focal_y = $7, fields = $8
WHERE id = $1 RETURNING *;

-- name: ReplaceMediaFile :one
UPDATE media SET sha256 = $2, filename = $3, mime = $4, size = $5, width = $6, height = $7, storage_key = $8
WHERE id = $1 RETURNING *;

-- name: SoftDeleteMedia :exec
UPDATE media SET deleted_at = now() WHERE id = $1;

-- name: PurgeDeletedMedia :many
DELETE FROM media WHERE deleted_at < now() - (@age::text)::interval RETURNING id, storage_key;

-- name: MediaUsage :many
SELECT u.field_path, e.id, e.type, e.draft_title FROM media_usage u JOIN entries e ON e.id = u.entry_id
WHERE u.media_id = $1;

-- name: UpsertVariant :exec
INSERT INTO media_variants (media_id, preset, format, storage_key, width, height, size)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (media_id, preset, format) DO UPDATE SET storage_key = EXCLUDED.storage_key,
    width = EXCLUDED.width, height = EXCLUDED.height, size = EXCLUDED.size, created_at = now();

-- name: VariantsFor :many
SELECT * FROM media_variants WHERE media_id = ANY(@ids::uuid[]) ORDER BY media_id, width;

-- name: DeleteVariants :many
DELETE FROM media_variants WHERE media_id = $1 RETURNING storage_key;

-- name: EntriesUsingMedia :many
SELECT DISTINCT entry_id FROM media_usage WHERE media_id = $1;

-- name: UnusedMediaCount :one
SELECT count(*) FROM media m WHERE m.deleted_at IS NULL AND NOT EXISTS (SELECT 1 FROM media_usage u WHERE u.media_id = m.id);

-- name: MediaMissingAlt :one
SELECT count(*) FROM media WHERE deleted_at IS NULL AND mime LIKE 'image/%' AND alt = '';

-- name: UpsertContentType :exec
INSERT INTO content_types (name, config, source_hash) VALUES ($1, $2, $3)
ON CONFLICT (name) DO UPDATE SET config = EXCLUDED.config, source_hash = EXCLUDED.source_hash, updated_at = now()
WHERE content_types.source_hash <> EXCLUDED.source_hash;

-- name: UpsertTaxonomy :exec
INSERT INTO taxonomies (name, config, source_hash) VALUES ($1, $2, $3)
ON CONFLICT (name) DO UPDATE SET config = EXCLUDED.config, source_hash = EXCLUDED.source_hash, updated_at = now()
WHERE taxonomies.source_hash <> EXCLUDED.source_hash;

-- name: UpsertFieldGroup :exec
INSERT INTO field_groups (name, config, source_hash) VALUES ($1, $2, $3)
ON CONFLICT (name) DO UPDATE SET config = EXCLUDED.config, source_hash = EXCLUDED.source_hash, updated_at = now()
WHERE field_groups.source_hash <> EXCLUDED.source_hash;

-- name: UpsertBlockDefinition :exec
INSERT INTO block_definitions (name, config, source_hash) VALUES ($1, $2, $3)
ON CONFLICT (name) DO UPDATE SET config = EXCLUDED.config, source_hash = EXCLUDED.source_hash, updated_at = now()
WHERE block_definitions.source_hash <> EXCLUDED.source_hash;

-- name: ListContentTypeHashes :many
SELECT name, source_hash FROM content_types;

-- name: ListTaxonomyHashes :many
SELECT name, source_hash FROM taxonomies;

-- name: ListFieldGroupHashes :many
SELECT name, source_hash FROM field_groups;

-- name: ListBlockDefinitionHashes :many
SELECT name, source_hash FROM block_definitions;

-- name: DeleteFieldGroup :exec
DELETE FROM field_groups WHERE name = $1;

-- name: DeleteBlockDefinition :exec
DELETE FROM block_definitions WHERE name = $1;

-- name: CountEntriesOfType :one
SELECT count(*) FROM entries WHERE type = $1;

-- name: CountTermsOfTaxonomy :one
SELECT count(*) FROM terms WHERE taxonomy = $1;

-- name: DeleteContentType :exec
DELETE FROM content_types WHERE name = $1;

-- name: DeleteTaxonomy :exec
DELETE FROM taxonomies WHERE name = $1;

------------------------------------------------------------------------------
-- Entries
------------------------------------------------------------------------------

-- name: CreateEntry :one
INSERT INTO entries (id, type, status, slug, parent_id, author_id, locale, menu_order,
    draft_title, draft_slug, draft_meta, draft_fields, draft_blocks)
VALUES ($1, $2, 'draft', $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING *;

-- name: GetEntry :one
SELECT * FROM entries WHERE id = $1;

-- name: GetEntryForUpdate :one
SELECT * FROM entries WHERE id = $1 FOR UPDATE;

-- name: SaveWorkingCopy :one
UPDATE entries SET
    draft_title = $2, draft_slug = $3, draft_meta = $4, draft_fields = $5, draft_blocks = $6,
    version = version + 1, updated_at = now()
WHERE id = $1 AND version = @expected_version
RETURNING *;

-- name: SetEntryAuthor :exec
UPDATE entries SET author_id = $2, updated_at = now() WHERE id = $1;

-- name: SetEntryOrder :exec
UPDATE entries SET menu_order = $2, updated_at = now() WHERE id = $1;

-- name: PublishEntry :one
UPDATE entries SET
    status = 'published',
    slug = $2,
    parent_id = $3,
    published_revision_id = $4,
    published_version = version,
    published_at = COALESCE(published_at, @published_at::timestamptz),
    scheduled_at = NULL,
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: SetPublishedAt :exec
UPDATE entries SET published_at = $2 WHERE id = $1;

-- name: ScheduleEntry :exec
-- A published entry stays live until its scheduled update publishes.
UPDATE entries SET
    status = CASE WHEN published_revision_id IS NULL THEN 'scheduled' ELSE status END,
    scheduled_at = $2, updated_at = now()
WHERE id = $1;

-- name: SetUnpublishAt :exec
UPDATE entries SET unpublish_at = $2, updated_at = now() WHERE id = $1;

-- name: UnscheduleEntry :exec
UPDATE entries SET
    status = CASE WHEN published_revision_id IS NULL THEN 'draft' ELSE 'published' END,
    scheduled_at = NULL, updated_at = now()
WHERE id = $1 AND scheduled_at IS NOT NULL;

-- name: SetEntryStatus :exec
UPDATE entries SET status = $2, updated_at = now() WHERE id = $1;

-- name: UnpublishEntry :exec
UPDATE entries SET status = 'draft', published_revision_id = NULL, published_version = NULL,
    unpublish_at = NULL, updated_at = now()
WHERE id = $1;

-- name: DeleteSearchVector :exec
DELETE FROM entry_search WHERE entry_id = $1;

-- name: TrashEntry :exec
UPDATE entries SET status = 'trashed', deleted_at = now(), published_revision_id = NULL,
    published_version = NULL, scheduled_at = NULL, unpublish_at = NULL, updated_at = now()
WHERE id = $1;

-- name: RestoreEntry :exec
UPDATE entries SET status = 'draft', deleted_at = NULL, updated_at = now() WHERE id = $1;

-- name: DeleteEntry :exec
DELETE FROM entries WHERE id = $1 AND status = 'trashed';

-- name: ResetWorkingCopy :one
UPDATE entries e SET
    draft_title = r.title, draft_slug = r.slug, draft_meta = r.meta, draft_fields = r.fields,
    draft_blocks = r.blocks, version = e.version + 1, published_version = e.version + 1, updated_at = now()
FROM entry_revisions r
WHERE e.id = $1 AND r.id = e.published_revision_id
RETURNING e.*;

-- name: UpdateSearchVector :exec
INSERT INTO entry_search (entry_id, vector) VALUES (@id,
    setweight(to_tsvector(@cfg::regconfig, cms_unaccent(@title::text)), 'A') ||
    setweight(to_tsvector(@cfg::regconfig, cms_unaccent(@summary::text)), 'B') ||
    setweight(to_tsvector(@cfg::regconfig, cms_unaccent(@body::text)), 'C'))
ON CONFLICT (entry_id) DO UPDATE SET vector = EXCLUDED.vector;

-- name: SlugTaken :one
SELECT EXISTS (
    SELECT 1 FROM entries
    WHERE type = $1 AND locale = $2 AND parent_id IS NOT DISTINCT FROM sqlc.narg(parent_id)::uuid
      AND slug = $3 AND id <> $4 AND deleted_at IS NULL
);

-- name: DraftSlugTaken :one
SELECT EXISTS (
    SELECT 1 FROM entries
    WHERE type = $1 AND locale = $2 AND id <> $4 AND deleted_at IS NULL
      AND (slug = $3 OR draft_slug = $3)
      AND parent_id IS NOT DISTINCT FROM sqlc.narg(parent_id)::uuid
);

-- name: ListEntries :many
SELECT e.id, e.type, e.status, e.version, e.slug, e.parent_id, e.author_id, e.locale, e.menu_order,
    e.draft_title, e.draft_slug, e.published_version, e.published_at, e.scheduled_at,
    e.created_at, e.updated_at, e.published_revision_id,
    COALESCE(u.display_name, '') AS author_name,
    count(*) OVER () AS total
FROM entries e
LEFT JOIN users u ON u.id = e.author_id
WHERE e.type = @type
  AND (sqlc.narg(status)::text IS NULL OR e.status = sqlc.narg(status))
  AND (sqlc.narg(status)::text = 'trashed' OR e.status <> 'trashed')
  AND (sqlc.narg(q)::text IS NULL OR e.draft_title ILIKE '%' || sqlc.narg(q) || '%')
  AND (sqlc.narg(author_id)::uuid IS NULL OR e.author_id = sqlc.narg(author_id))
  AND (sqlc.narg(parent_id)::uuid IS NULL OR e.parent_id = sqlc.narg(parent_id))
ORDER BY
  CASE WHEN @sort::text = 'title' THEN lower(e.draft_title) END ASC,
  CASE WHEN @sort::text = 'menu_order' THEN e.menu_order END ASC,
  CASE WHEN @sort::text = 'published_at' THEN e.published_at END DESC NULLS LAST,
  e.updated_at DESC
LIMIT @lim OFFSET @off;

-- name: CountEntriesByStatus :many
SELECT type, status, count(*) AS n FROM entries GROUP BY type, status;

-- name: RecentEntries :many
SELECT e.id, e.type, e.status, e.draft_title, e.updated_at, e.version, e.published_version,
    COALESCE(u.display_name, '') AS author_name
FROM entries e LEFT JOIN users u ON u.id = e.author_id
WHERE e.status <> 'trashed'
ORDER BY e.updated_at DESC LIMIT $1;

-- name: ScheduledEntries :many
SELECT id, type, draft_title, scheduled_at FROM entries
WHERE scheduled_at IS NOT NULL AND status <> 'trashed' ORDER BY scheduled_at LIMIT $1;

-- name: DueScheduled :many
SELECT id FROM entries WHERE scheduled_at <= now() AND status IN ('draft', 'scheduled', 'published')
ORDER BY scheduled_at LIMIT 100;

-- name: DueUnpublish :many
SELECT id FROM entries WHERE status = 'published' AND unpublish_at <= now() ORDER BY unpublish_at LIMIT 100;

-- name: SearchEntriesAdmin :many
SELECT id, type, status, draft_title, updated_at FROM entries
WHERE status <> 'trashed' AND (draft_title ILIKE '%' || @q::text || '%' OR draft_title % @q::text)
ORDER BY similarity(draft_title, @q::text) DESC, updated_at DESC
LIMIT @lim;

-- name: EntryChildren :many
SELECT id, draft_title, slug, status, menu_order FROM entries
WHERE parent_id = $1 AND deleted_at IS NULL ORDER BY menu_order, draft_title;

-- name: EntryAncestors :many
WITH RECURSIVE chain AS (
    SELECT e.id, e.parent_id, e.slug, 0 AS depth FROM entries e WHERE e.id = $1
    UNION ALL
    SELECT p.id, p.parent_id, p.slug, c.depth + 1 FROM entries p JOIN chain c ON p.id = c.parent_id
    WHERE c.depth < 32
)
SELECT id, slug, depth FROM chain ORDER BY depth DESC;

-- name: EntryPickerOptions :many
SELECT id, type, draft_title, status FROM entries
WHERE type = ANY(@types::text[]) AND status <> 'trashed'
  AND (@q::text = '' OR draft_title ILIKE '%' || @q::text || '%')
ORDER BY draft_title LIMIT 50;

-- name: EntriesByIDs :many
SELECT id, type, status, draft_title, slug FROM entries WHERE id = ANY(@ids::uuid[]);

------------------------------------------------------------------------------
-- Revisions
------------------------------------------------------------------------------

-- name: NextRevisionNumber :one
SELECT COALESCE(max(number), 0) + 1 FROM entry_revisions WHERE entry_id = $1;

-- name: CreateRevision :one
INSERT INTO entry_revisions (id, entry_id, number, kind, author_id, title, slug, meta, fields, blocks)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: GetRevision :one
SELECT * FROM entry_revisions WHERE id = $1;

-- name: ListRevisions :many
SELECT r.id, r.number, r.kind, r.title, r.created_at, r.author_id, COALESCE(u.display_name, '') AS author_name
FROM entry_revisions r LEFT JOIN users u ON u.id = r.author_id
WHERE r.entry_id = $1 ORDER BY r.number DESC LIMIT $2;

-- name: PruneRevisions :execrows
DELETE FROM entry_revisions r
WHERE r.id IN (
    SELECT x.id FROM entry_revisions x
    JOIN entries e ON e.id = x.entry_id
    WHERE x.id IS DISTINCT FROM e.published_revision_id
      AND x.number <= (SELECT max(y.number) FROM entry_revisions y WHERE y.entry_id = x.entry_id) - @keep::int
      AND x.created_at < now() - (@min_age::text)::interval
);

------------------------------------------------------------------------------
-- Published content (rendering)
------------------------------------------------------------------------------

-- name: PublishedBySlug :many
SELECT e.id, e.type, e.slug, e.parent_id, e.author_id, e.locale, e.menu_order, e.published_at, e.updated_at,
    r.title, r.meta, r.fields, r.blocks, r.created_at AS revised_at,
    COALESCE(u.display_name, '') AS author_name, COALESCE(u.bio, '') AS author_bio
FROM entries e
JOIN entry_revisions r ON r.id = e.published_revision_id
LEFT JOIN users u ON u.id = e.author_id
WHERE e.type = $1 AND e.locale = $2 AND e.slug = $3 AND e.status = 'published'
LIMIT 50;

-- name: PublishedByID :one
SELECT e.id, e.type, e.slug, e.parent_id, e.author_id, e.locale, e.menu_order, e.published_at, e.updated_at,
    r.title, r.meta, r.fields, r.blocks, r.created_at AS revised_at,
    COALESCE(u.display_name, '') AS author_name, COALESCE(u.bio, '') AS author_bio
FROM entries e
JOIN entry_revisions r ON r.id = e.published_revision_id
LEFT JOIN users u ON u.id = e.author_id
WHERE e.id = $1 AND e.status = 'published';

-- name: PublishedList :many
SELECT e.id, e.type, e.slug, e.parent_id, e.author_id, e.locale, e.menu_order, e.published_at, e.updated_at,
    r.title, r.meta, r.fields, r.created_at AS revised_at,
    COALESCE(u.display_name, '') AS author_name,
    count(*) OVER () AS total
FROM entries e
JOIN entry_revisions r ON r.id = e.published_revision_id
LEFT JOIN users u ON u.id = e.author_id
WHERE e.type = @type AND e.locale = @locale AND e.status = 'published'
  AND (sqlc.narg(term_id)::uuid IS NULL OR EXISTS (
        SELECT 1 FROM entry_terms et WHERE et.entry_id = e.id AND et.term_id = sqlc.narg(term_id)))
  AND (sqlc.narg(author_id)::uuid IS NULL OR e.author_id = sqlc.narg(author_id))
  AND (sqlc.narg(parent_id)::uuid IS NULL OR e.parent_id = sqlc.narg(parent_id))
  AND (sqlc.narg(exclude_id)::uuid IS NULL OR e.id <> sqlc.narg(exclude_id))
ORDER BY
  CASE WHEN @sort::text = 'title' THEN lower(r.title) END ASC,
  CASE WHEN @sort::text = 'menu_order' THEN e.menu_order END ASC,
  CASE WHEN @sort::text = 'oldest' THEN e.published_at END ASC,
  e.published_at DESC, e.id
LIMIT @lim OFFSET @off;

-- name: PublishedIDsByType :many
SELECT e.id, e.slug, e.parent_id, e.published_at, e.updated_at FROM entries e
WHERE e.type = $1 AND e.status = 'published' ORDER BY e.published_at DESC;

-- name: PublishedSearch :many
SELECT e.id, e.type, e.slug, e.parent_id, e.published_at, r.title, r.meta,
    ts_rank(s.vector, q) AS rank, count(*) OVER () AS total
FROM entries e
JOIN entry_search s ON s.entry_id = e.id
JOIN entry_revisions r ON r.id = e.published_revision_id,
     websearch_to_tsquery(@cfg::regconfig, cms_unaccent(@query::text)) q
WHERE e.status = 'published' AND s.vector @@ q AND e.type = ANY(@types::text[])
ORDER BY rank DESC, e.published_at DESC
LIMIT @lim OFFSET @off;

-- name: AdjacentPublished :many
(SELECT e.id, e.slug, e.parent_id, e.published_at, r.title, 'prev'::text AS dir FROM entries e
  JOIN entry_revisions r ON r.id = e.published_revision_id
  WHERE e.type = @type AND e.status = 'published' AND e.published_at < @at::timestamptz
  ORDER BY e.published_at DESC LIMIT 1)
UNION ALL
(SELECT e.id, e.slug, e.parent_id, e.published_at, r.title, 'next'::text AS dir FROM entries e
  JOIN entry_revisions r ON r.id = e.published_revision_id
  WHERE e.type = @type AND e.status = 'published' AND e.published_at > @at::timestamptz
  ORDER BY e.published_at ASC LIMIT 1);

------------------------------------------------------------------------------
-- Terms
------------------------------------------------------------------------------

-- name: CreateTerm :one
INSERT INTO terms (id, taxonomy, parent_id, slug, name, description, fields)
VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING *;

-- name: UpdateTerm :one
UPDATE terms SET parent_id = $2, slug = $3, name = $4, description = $5, fields = $6
WHERE id = $1 RETURNING *;

-- name: GetTerm :one
SELECT * FROM terms WHERE id = $1;

-- name: GetTermBySlug :one
SELECT * FROM terms WHERE taxonomy = $1 AND slug = $2 LIMIT 1;

-- name: ListTerms :many
SELECT t.*, (SELECT count(*) FROM entry_terms et JOIN entries e ON e.id = et.entry_id
             WHERE et.term_id = t.id AND e.status = 'published') AS published_count
FROM terms t WHERE t.taxonomy = $1 ORDER BY lower(t.name);

-- name: TermsByIDs :many
SELECT * FROM terms WHERE id = ANY(@ids::uuid[]);

-- name: DeleteTerm :exec
DELETE FROM terms WHERE id = $1;

-- name: ReassignTerm :exec
INSERT INTO entry_terms (entry_id, term_id)
SELECT entry_id, @to_id::uuid FROM entry_terms WHERE term_id = @from_id::uuid
ON CONFLICT DO NOTHING;

-- name: SetEntryTerms :exec
DELETE FROM entry_terms WHERE entry_id = $1;

-- name: AddEntryTerm :exec
INSERT INTO entry_terms (entry_id, term_id) VALUES ($1, $2) ON CONFLICT DO NOTHING;

-- name: TermsForEntries :many
SELECT et.entry_id, t.id, t.taxonomy, t.slug, t.name
FROM entry_terms et JOIN terms t ON t.id = et.term_id
WHERE et.entry_id = ANY(@ids::uuid[]) ORDER BY t.taxonomy, lower(t.name);

-- name: EntriesWithTerm :many
SELECT entry_id FROM entry_terms WHERE term_id = $1;

------------------------------------------------------------------------------
-- Relations and media usage
------------------------------------------------------------------------------

-- name: ClearEntryRelations :exec
DELETE FROM entry_relations WHERE source_id = $1;

-- name: AddEntryRelation :exec
INSERT INTO entry_relations (source_id, field, position, target_id) VALUES ($1, $2, $3, $4)
ON CONFLICT DO NOTHING;

-- name: ReferencingEntries :many
SELECT DISTINCT e.id, e.type, e.draft_title FROM entry_relations r JOIN entries e ON e.id = r.source_id
WHERE r.target_id = $1 AND e.deleted_at IS NULL;

-- name: ClearMediaUsage :exec
DELETE FROM media_usage WHERE entry_id = $1;

-- name: AddMediaUsage :exec
INSERT INTO media_usage (media_id, entry_id, field_path)
SELECT $1, $2, $3 WHERE EXISTS (SELECT 1 FROM media WHERE id = $1)
ON CONFLICT DO NOTHING;

-- name: EntryByTypeSlug :one
SELECT * FROM entries WHERE type = $1 AND draft_slug = $2 AND deleted_at IS NULL ORDER BY created_at LIMIT 1;

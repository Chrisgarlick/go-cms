-- name: GetSetting :one
SELECT value FROM settings WHERE key = $1;

-- name: ListSettings :many
SELECT * FROM settings ORDER BY key;

-- name: SetSetting :exec
INSERT INTO settings (key, value) VALUES ($1, $2)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();

-- name: DeleteSetting :exec
DELETE FROM settings WHERE key = $1;

-- name: GetOptions :one
SELECT * FROM options_pages WHERE name = $1;

-- name: ListOptions :many
SELECT * FROM options_pages;

-- name: SaveOptions :exec
INSERT INTO options_pages (name, fields) VALUES ($1, $2)
ON CONFLICT (name) DO UPDATE SET fields = EXCLUDED.fields, version = options_pages.version + 1, updated_at = now();

-- name: GetMenu :one
SELECT * FROM menus WHERE name = $1;

-- name: ListMenus :many
SELECT * FROM menus ORDER BY name;

-- name: SaveMenu :exec
INSERT INTO menus (name, label, items) VALUES ($1, $2, $3)
ON CONFLICT (name) DO UPDATE SET label = EXCLUDED.label, items = EXCLUDED.items, updated_at = now();

-- name: DeleteMenu :exec
DELETE FROM menus WHERE name = $1;

-- name: GetSecret :one
SELECT * FROM secrets WHERE name = $1;

-- name: SaveSecret :exec
INSERT INTO secrets (name, value_enc) VALUES ($1, $2)
ON CONFLICT (name) DO UPDATE SET value_enc = EXCLUDED.value_enc, rotated_at = now();

-- name: ListSecrets :many
SELECT * FROM secrets;

-- name: ClearRenderDeps :exec
DELETE FROM render_deps WHERE page_key = $1;

-- name: AddRenderDep :exec
INSERT INTO render_deps (page_key, dep_type, dep_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING;

-- name: PagesDependingOn :many
SELECT DISTINCT page_key FROM render_deps WHERE dep_type = $1 AND dep_id = ANY(@ids::text[]);

-- name: AllRenderedPages :many
SELECT DISTINCT page_key FROM render_deps;

-- name: ClearAllRenderDeps :exec
DELETE FROM render_deps;

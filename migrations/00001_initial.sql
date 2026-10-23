-- +cms expand
-- +goose Up

CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS unaccent;

-- Immutable wrapper so unaccent can be used in generated columns / indexes.
CREATE OR REPLACE FUNCTION cms_unaccent(text) RETURNS text
    LANGUAGE sql IMMUTABLE PARALLEL SAFE STRICT
    AS $$ SELECT public.unaccent('public.unaccent'::regdictionary, $1) $$;

----------------------------------------------------------------------------
-- Users and authentication
----------------------------------------------------------------------------

CREATE TABLE users (
    id              uuid PRIMARY KEY,
    email           text NOT NULL,
    password_hash   text NOT NULL DEFAULT '',
    role            text NOT NULL CHECK (role IN ('owner', 'admin', 'editor')),
    display_name    text NOT NULL DEFAULT '',
    bio             text NOT NULL DEFAULT '',
    status          text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled', 'invited', 'deleted')),
    locale          text NOT NULL DEFAULT 'en',
    totp_enc        bytea,
    totp_confirmed  boolean NOT NULL DEFAULT false,
    -- A replacement secret awaiting its first code during re-enrolment.
    totp_pending_enc bytea,
    last_login_at   timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_email_key ON users (lower(email)) WHERE status <> 'deleted';

CREATE TABLE user_recovery_codes (
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_sha256 bytea NOT NULL,
    used_at     timestamptz,
    PRIMARY KEY (user_id, code_sha256)
);

CREATE TABLE webauthn_credentials (
    id            bytea PRIMARY KEY,
    user_id       uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name          text NOT NULL DEFAULT '',
    credential    jsonb NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_used_at  timestamptz
);
CREATE INDEX webauthn_credentials_user_idx ON webauthn_credentials (user_id);

-- One-time tokens: invites, password resets, magic links. Bound to a browser nonce.
CREATE TABLE auth_tokens (
    id                  uuid PRIMARY KEY,
    user_id             uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    purpose             text NOT NULL CHECK (purpose IN ('invite', 'reset', 'magic')),
    token_sha256        bytea NOT NULL UNIQUE,
    browser_nonce_sha256 bytea,
    expires_at          timestamptz NOT NULL,
    used_at             timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX auth_tokens_user_idx ON auth_tokens (user_id);

-- Sessions: the cookie holds a random token; only its SHA-256 is stored.
CREATE TABLE sessions (
    id_sha256      bytea PRIMARY KEY,
    user_id        uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    csrf_token     text NOT NULL,
    -- 'mfa': password accepted, second factor pending; 'enroll': must set up
    -- 2FA before anything else; 'full': signed in.
    stage          text NOT NULL DEFAULT 'full' CHECK (stage IN ('mfa', 'enroll', 'full')),
    user_agent     text NOT NULL DEFAULT '',
    ip             inet,
    sudo_until     timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    last_seen_at   timestamptz NOT NULL DEFAULT now(),
    expires_at     timestamptz NOT NULL
);
CREATE INDEX sessions_user_idx ON sessions (user_id);
CREATE INDEX sessions_expires_idx ON sessions (expires_at);

CREATE TABLE api_tokens (
    id             uuid PRIMARY KEY,
    user_id        uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name           text NOT NULL,
    secret_sha256  bytea NOT NULL,
    role           text NOT NULL CHECK (role IN ('owner', 'admin', 'editor')),
    expires_at     timestamptz NOT NULL,
    last_used_at   timestamptz,
    revoked_at     timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX api_tokens_user_idx ON api_tokens (user_id);

----------------------------------------------------------------------------
-- Schema definitions (loaded from the site bundle; read-only in production)
----------------------------------------------------------------------------

CREATE TABLE content_types (
    name         text PRIMARY KEY,
    config       jsonb NOT NULL,
    source_hash  text NOT NULL,
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE field_groups (
    name         text PRIMARY KEY,
    config       jsonb NOT NULL,
    source_hash  text NOT NULL,
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE block_definitions (
    name         text PRIMARY KEY,
    config       jsonb NOT NULL,
    source_hash  text NOT NULL,
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE taxonomies (
    name         text PRIMARY KEY,
    config       jsonb NOT NULL,
    source_hash  text NOT NULL,
    updated_at   timestamptz NOT NULL DEFAULT now()
);

----------------------------------------------------------------------------
-- Content
----------------------------------------------------------------------------

CREATE TABLE entries (
    id                    uuid PRIMARY KEY,
    type                  text NOT NULL REFERENCES content_types (name) ON UPDATE CASCADE,
    status                text NOT NULL DEFAULT 'draft'
                          CHECK (status IN ('draft', 'scheduled', 'published', 'archived', 'trashed')),
    version               integer NOT NULL DEFAULT 1,
    -- live identity (routing)
    slug                  text NOT NULL,
    parent_id             uuid REFERENCES entries (id) ON DELETE SET NULL,
    author_id             uuid REFERENCES users (id) ON DELETE SET NULL,
    locale                text NOT NULL DEFAULT 'en',
    translation_group_id  uuid,
    menu_order            integer NOT NULL DEFAULT 0,
    -- working copy
    draft_title           text NOT NULL DEFAULT '',
    draft_slug            text NOT NULL DEFAULT '',
    draft_meta            jsonb NOT NULL DEFAULT '{}',
    draft_fields          jsonb NOT NULL DEFAULT '{}',
    draft_blocks          jsonb NOT NULL DEFAULT '[]',
    -- published pointer
    published_revision_id uuid,
    published_version     integer,
    published_at          timestamptz,
    scheduled_at          timestamptz,
    unpublish_at          timestamptz,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    deleted_at            timestamptz
);
CREATE UNIQUE INDEX entries_route_key ON entries (type, locale, parent_id, slug) NULLS NOT DISTINCT
    WHERE deleted_at IS NULL;
CREATE INDEX entries_listing_idx ON entries (type, status, published_at DESC);
CREATE INDEX entries_updated_idx ON entries (updated_at DESC);
CREATE INDEX entries_parent_idx ON entries (parent_id) WHERE parent_id IS NOT NULL;
CREATE INDEX entries_author_idx ON entries (author_id);
CREATE INDEX entries_scheduled_idx ON entries (scheduled_at) WHERE scheduled_at IS NOT NULL;
CREATE INDEX entries_unpublish_idx ON entries (unpublish_at) WHERE unpublish_at IS NOT NULL;
CREATE INDEX entries_title_trgm_idx ON entries USING gin (draft_title gin_trgm_ops);

CREATE TABLE entry_revisions (
    id          uuid PRIMARY KEY,
    entry_id    uuid NOT NULL REFERENCES entries (id) ON DELETE CASCADE,
    number      integer NOT NULL,
    kind        text NOT NULL CHECK (kind IN ('save', 'publish', 'restore', 'scheduled')),
    author_id   uuid REFERENCES users (id) ON DELETE SET NULL,
    title       text NOT NULL,
    slug        text NOT NULL,
    meta        jsonb NOT NULL,
    fields      jsonb NOT NULL,
    blocks      jsonb NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (entry_id, number)
);
ALTER TABLE entries ADD CONSTRAINT entries_published_revision_fk
    FOREIGN KEY (published_revision_id) REFERENCES entry_revisions (id) ON DELETE SET NULL;

-- Revision snapshots compress with lz4. Postgres built without lz4
-- (some embedded test binaries) keeps the default pglz instead.
-- +goose StatementBegin
DO $$
BEGIN
    ALTER TABLE entries ALTER COLUMN draft_fields SET COMPRESSION lz4;
    ALTER TABLE entries ALTER COLUMN draft_blocks SET COMPRESSION lz4;
    ALTER TABLE entry_revisions ALTER COLUMN fields SET COMPRESSION lz4;
    ALTER TABLE entry_revisions ALTER COLUMN blocks SET COMPRESSION lz4;
EXCEPTION WHEN feature_not_supported THEN
    RAISE NOTICE 'lz4 is not available: revision columns use pglz';
END
$$;
-- +goose StatementEnd

-- Search vectors live apart from entries: rows stay small and the driver never
-- has to decode tsvector values.
CREATE TABLE entry_search (
    entry_id  uuid PRIMARY KEY REFERENCES entries (id) ON DELETE CASCADE,
    vector    tsvector NOT NULL
);
CREATE INDEX entry_search_idx ON entry_search USING gin (vector);

CREATE TABLE entry_relations (
    source_id  uuid NOT NULL REFERENCES entries (id) ON DELETE CASCADE,
    field      text NOT NULL,
    position   integer NOT NULL,
    target_id  uuid NOT NULL REFERENCES entries (id) ON DELETE CASCADE,
    PRIMARY KEY (source_id, field, position)
);
CREATE INDEX entry_relations_target_idx ON entry_relations (target_id);

CREATE TABLE terms (
    id           uuid PRIMARY KEY,
    taxonomy     text NOT NULL REFERENCES taxonomies (name) ON UPDATE CASCADE,
    parent_id    uuid REFERENCES terms (id) ON DELETE SET NULL,
    slug         text NOT NULL,
    name         text NOT NULL,
    description  text NOT NULL DEFAULT '',
    fields       jsonb NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX terms_slug_key ON terms (taxonomy, parent_id, slug) NULLS NOT DISTINCT;

-- Live (published) term assignments. Draft assignments live in entries.draft_meta.
CREATE TABLE entry_terms (
    entry_id  uuid NOT NULL REFERENCES entries (id) ON DELETE CASCADE,
    term_id   uuid NOT NULL REFERENCES terms (id) ON DELETE CASCADE,
    PRIMARY KEY (entry_id, term_id)
);
CREATE INDEX entry_terms_term_idx ON entry_terms (term_id);

----------------------------------------------------------------------------
-- Media
----------------------------------------------------------------------------

CREATE TABLE media (
    id           uuid PRIMARY KEY,
    sha256       bytea NOT NULL,
    filename     text NOT NULL,
    mime         text NOT NULL,
    size         bigint NOT NULL,
    width        integer,
    height       integer,
    storage_key  text NOT NULL,
    alt          text NOT NULL DEFAULT '',
    title        text NOT NULL DEFAULT '',
    caption      text NOT NULL DEFAULT '',
    credit       text NOT NULL DEFAULT '',
    focal_x      real NOT NULL DEFAULT 0.5,
    focal_y      real NOT NULL DEFAULT 0.5,
    fields       jsonb NOT NULL DEFAULT '{}',
    uploader_id  uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    deleted_at   timestamptz
);
CREATE INDEX media_sha256_idx ON media (sha256);
CREATE INDEX media_created_idx ON media (created_at DESC);
CREATE INDEX media_filename_trgm_idx ON media USING gin (filename gin_trgm_ops);

CREATE TABLE media_variants (
    media_id     uuid NOT NULL REFERENCES media (id) ON DELETE CASCADE,
    preset       text NOT NULL,
    format       text NOT NULL,
    storage_key  text NOT NULL,
    width        integer NOT NULL,
    height       integer NOT NULL,
    size         bigint NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (media_id, preset, format)
);

CREATE TABLE media_usage (
    media_id    uuid NOT NULL REFERENCES media (id) ON DELETE CASCADE,
    entry_id    uuid NOT NULL REFERENCES entries (id) ON DELETE CASCADE,
    field_path  text NOT NULL,
    PRIMARY KEY (media_id, entry_id, field_path)
);
CREATE INDEX media_usage_entry_idx ON media_usage (entry_id);

----------------------------------------------------------------------------
-- Redirects and 404s
----------------------------------------------------------------------------

CREATE TABLE redirects (
    id              uuid PRIMARY KEY,
    host            text NOT NULL DEFAULT '',
    source          text NOT NULL,
    match_type      text NOT NULL CHECK (match_type IN ('exact', 'prefix', 'regex')),
    target          text NOT NULL,
    status_code     integer NOT NULL CHECK (status_code IN (301, 302, 307, 308, 410, 451)),
    preserve_query  boolean NOT NULL DEFAULT true,
    is_auto         boolean NOT NULL DEFAULT false,
    note            text NOT NULL DEFAULT '',
    hits            bigint NOT NULL DEFAULT 0,
    last_hit_at     timestamptz,
    created_by      uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (host, source, match_type)
);

CREATE TABLE not_found_log (
    host        text NOT NULL,
    path        text NOT NULL,
    referrer    text NOT NULL DEFAULT '',
    hits        bigint NOT NULL DEFAULT 0,
    first_seen  timestamptz NOT NULL DEFAULT now(),
    last_seen   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (host, path)
);
CREATE INDEX not_found_log_hits_idx ON not_found_log (hits DESC);

----------------------------------------------------------------------------
-- Menus, settings, options
----------------------------------------------------------------------------

CREATE TABLE menus (
    name      text PRIMARY KEY,
    label     text NOT NULL,
    items     jsonb NOT NULL DEFAULT '[]',
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE settings (
    key         text PRIMARY KEY,
    value       jsonb NOT NULL,
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE options_pages (
    name        text PRIMARY KEY,
    fields      jsonb NOT NULL DEFAULT '{}',
    version     integer NOT NULL DEFAULT 1,
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- Envelope-encrypted secrets (SMTP, S3, provider credentials).
CREATE TABLE secrets (
    name        text PRIMARY KEY,
    value_enc   bytea NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    rotated_at  timestamptz
);

----------------------------------------------------------------------------
-- Rendering
----------------------------------------------------------------------------

-- Which rendered pages read which records, for targeted invalidation.
CREATE TABLE render_deps (
    page_key  text NOT NULL,
    dep_type  text NOT NULL,
    dep_id    text NOT NULL,
    PRIMARY KEY (page_key, dep_type, dep_id)
);
CREATE INDEX render_deps_dep_idx ON render_deps (dep_type, dep_id);

----------------------------------------------------------------------------
-- Webhooks
----------------------------------------------------------------------------

CREATE TABLE webhooks (
    id          uuid PRIMARY KEY,
    url         text NOT NULL,
    secret_enc  bytea NOT NULL,
    events      text[] NOT NULL,
    active      boolean NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE webhook_deliveries (
    id           uuid PRIMARY KEY,
    webhook_id   uuid NOT NULL REFERENCES webhooks (id) ON DELETE CASCADE,
    event        text NOT NULL,
    status       integer,
    attempts     integer NOT NULL DEFAULT 0,
    response     text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX webhook_deliveries_webhook_idx ON webhook_deliveries (webhook_id, created_at DESC);
CREATE INDEX webhook_deliveries_created_idx ON webhook_deliveries (created_at);

----------------------------------------------------------------------------
-- Security self-audit
----------------------------------------------------------------------------

CREATE TABLE security_findings (
    id             text PRIMARY KEY,           -- stable key, e.g. "vuln:GO-2026-1234" or "config:debug"
    category       text NOT NULL,
    severity       text NOT NULL CHECK (severity IN ('info', 'low', 'medium', 'high', 'critical')),
    title          text NOT NULL,
    detail         text NOT NULL DEFAULT '',
    remediation    text NOT NULL DEFAULT '',
    first_seen     timestamptz NOT NULL DEFAULT now(),
    last_seen      timestamptz NOT NULL DEFAULT now(),
    resolved_at    timestamptz,
    ack_by         uuid REFERENCES users (id) ON DELETE SET NULL,
    ack_reason     text NOT NULL DEFAULT '',
    ack_until      timestamptz
);

----------------------------------------------------------------------------
-- Audit log: append-only, hash-chained, partitioned by month
----------------------------------------------------------------------------

CREATE TABLE audit_log (
    id             bigint GENERATED ALWAYS AS IDENTITY,
    prev_hash      bytea NOT NULL,
    row_hash       bytea NOT NULL,
    actor_type     text NOT NULL,
    actor_id       text NOT NULL,
    actor_label    text NOT NULL DEFAULT '',
    action         text NOT NULL,
    resource_type  text NOT NULL,
    resource_id    text NOT NULL,
    diff           jsonb NOT NULL DEFAULT '{}',
    ip             inet,
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);
CREATE TABLE audit_log_default PARTITION OF audit_log DEFAULT;
CREATE INDEX audit_log_resource_idx ON audit_log (resource_type, resource_id);
CREATE INDEX audit_log_actor_idx ON audit_log (actor_id, created_at DESC);

-- Creates monthly partitions for the current and next two months. SECURITY
-- DEFINER so the runtime role (no DDL rights) can call it from a job.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cms_audit_ensure_partitions() RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp
AS $$
DECLARE
    m date := date_trunc('month', now())::date;
    i int;
    lo date;
    hi date;
    part text;
BEGIN
    FOR i IN 0..2 LOOP
        lo := (m + make_interval(months => i))::date;
        hi := (lo + interval '1 month')::date;
        part := format('audit_log_%s', to_char(lo, 'YYYYMM'));
        IF to_regclass(part) IS NULL THEN
            -- Only safe while the default partition holds no rows for that range.
            IF NOT EXISTS (SELECT 1 FROM audit_log_default WHERE created_at >= lo AND created_at < hi) THEN
                EXECUTE format('CREATE TABLE %I PARTITION OF audit_log FOR VALUES FROM (%L) TO (%L)', part, lo, hi);
            END IF;
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd
SELECT cms_audit_ensure_partitions();

-- +goose Down
DROP FUNCTION IF EXISTS cms_audit_ensure_partitions();
DROP TABLE IF EXISTS audit_log, security_findings, webhook_deliveries, webhooks, render_deps,
    secrets, options_pages, settings, menus, not_found_log, redirects, media_usage,
    media_variants, media, entry_terms, terms, entry_relations, entry_search, entries, entry_revisions,
    taxonomies, block_definitions, field_groups, content_types, api_tokens, sessions,
    auth_tokens, webauthn_credentials, user_recovery_codes, users CASCADE;
DROP FUNCTION IF EXISTS cms_unaccent(text);

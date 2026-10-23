# Go CMS: Technical Specification

**Status:** v0.4 (v1 core built; see "Changes in v0.4" for deviations and deferred items)
**Working name:** `cms` (placeholder)
**Supersedes:** v0.2. Rationale for the v0.1 → v0.2 changes for every change is in the [v0.1 review](https://claude.ai/artifact/1ziL6GJKxJ3HHBfgzr4k7y).

> All open questions from v0.1 are decided (section 12).

### Changes in v0.4 (as built)
Where the implementation differs from v0.3, the code won and this document was updated:
- **Admin UI:** server-rendered `html/template` (no templ), hand-written CSS (no Tailwind), and small vanilla JS modules bundled with esbuild (no htmx or Alpine). Tiptap and SortableJS are the only front-end dependencies. Built assets are committed under `web/dist` and embedded (4.13).
- **Blocks:** edited in a JS block editor (cards with collapse, duplicate, drag reorder), saved as JSON with the entry, not as separate htmx forms (4.3).
- **Passkeys:** optional, as a second factor alongside mandatory TOTP. Passwordless sign-in is v1.x (4.7).
- **Sessions:** own implementation (hashed session IDs in Postgres with `mfa`/`enroll`/`full` stages), not `scs`.
- **Search:** `tsvector` lives in a separate `entry_search` table, maintained by the content service.
- **CLI:** adds `cms check` (validate a site repo and render every page, used by site CI), `cms caddy` (print Caddy config) and `cms build` (static export) (5.1).
- **Migrations:** `00001_initial` plus `00002_webauthn_state` (expand).
- **Added while moving chrisgarlick.com onto cms:**
  - **Forms** (4.8) are in v1, defined in `forms/<name>.yaml` and validated with the content field types. The signed timing token is fetched on first focus, plus a honeypot and a per-IP limit. Submissions are stored encrypted, emailed with Reply-To, listed in the admin (Admin and Owner only), audited on delete and pruned after `retention_days`. Not yet built: export, webhooks, CAPTCHA, file uploads.
  - **`trailing_slash: false`** in `site.yaml` publishes `/about` rather than `/about/`. Paths stay slash-terminated inside the CMS, and only published URLs, the canonical redirect and the generated Caddy config change.
  - **Theme `static/`** files are served at the site root (favicons, `.well-known/`). `static/og/<path>.png` is a page's social image when it has none of its own.
  - **CSS `url()` fingerprinting:** relative references in theme stylesheets (fonts, images) are rewritten to fingerprinted asset URLs.
  - **Block definitions** can set `wrapper: false` to drop the `<div class="b b-name">` wrapper.
  - **Template helpers:** `.Refs` / `.Entry.Refs` (entry reference fields), `.EntryBySlug`, `.Entry.Blocks`, `.Form`, and the functions `inline` (`*em*` / `**strong**` in text fields), `cond`, `append` and `pad`.
  - **Monorepo-safe release feeds.** Site bundles are picked by tag prefix (`upgrade.site_tag_prefix`, tags `<prefix>-YYYYMMDD-HHMMSS-<sha7>`), and core releases strictly by `vX.Y.Z` plus channel. Feeds are paginated, and selection fails closed when nothing matches.
    - Each bundle carries `bundle.json` and is refused if it was built for another site.
    - Bundles can be verified with their own key (`upgrade.site_public_key`).
    - The reusable `site.yml` workflow takes `path`, `tag_prefix` and `publish` inputs and never marks a bundle release "Latest".
  - **`cms content import`** loads a content file in the fixtures format: Markdown or ProseMirror bodies, exact dates, SEO, entry references by slug, and exact or prefix redirects. This is how a site moves onto cms.
- **Deferred, not yet built:** magic-link login, S3 storage driver and imgproxy (local disk only for now), point-in-time recovery (`cms backup` is pg_dump plus media), reusable blocks, admin command palette, saved list views, admin i18n, `cms schema export --go|--ts`. These stay on the roadmap and are not required to run a blog or brochure site.
- **Tests:** Go tests run against embedded Postgres; Playwright E2E checks every admin and public page for CSP violations and axe errors, plus the passkey flow (10).

### Changes in v0.3
- **Deployment model:** one instance per site. Your own website plus, optionally, one server per client. Site-specific themes and schemas live in per-site repos and deploy as signed site bundles (2.6).
- **Users:** three fixed roles (Owner, Admin, Editor). Custom roles, per-type permissions, ownership rules, impersonation and the review workflow are removed. 2FA is mandatory for every account (4.7).
- **Rollout rings:** staging → your own site (canary) → client sites (9.4).
- **Project scaffolding:** `cms new` creates a site repo from a starter built into the binary (`brochure`, `blog`, `blank`); `cms make` generates types, blocks and field groups. Client themes extend a built-in base theme. Site repos call one shared CI workflow. `cms init` is renamed `cms install` (5.4, 5.5).
- **Ecommerce:** recorded as a later feature ("shop-lite" with Stripe Checkout; Shopify for larger stores) (4.21).
- **Scope:** comments dropped; WordPress importer and the write API move to v1.x; GDPR tooling reduced. Roadmap is now 18 weeks to v1.0 (11).

### Changes in v0.2
- **Content model:** published entries now have a separate working copy. Autosave never touches the live site. Optimistic locking added (4.1, 6).
- **Secrets:** webhook secrets, TOTP seeds and provider credentials are encrypted, not hashed. Key management defined (7.3).
- **Media:** the core binary is pure Go. Image processing runs the `vips` CLI as a short-lived, resource-limited subprocess per job. "Static binary" now applies to the core only (1.1, 4.4).
- **Migrations:** forward-only in production. `migrate down` is dev-only. Expand/contract is enforced in CI (9.2).
- **Rendering:** public pages render to pre-compressed files that Caddy serves directly. Themes and blocks use `html/template` only. Cacheability rules defined per feature (4.12, 8).
- **Security:** two DB roles, append-only audit log, trusted proxies, Alpine CSP build, `http.CrossOriginProtection`, sudo mode, token format, impersonation limits (7).
- **Performance:** memory budget, batched counters, bounded caches, pool sizing (8).
- **CI/CD:** full pipeline, signed and attested releases, blue/green zero-downtime deploys with automatic rollback, Renovate, restore drills (9).
- **Ownership:** private, single-owner project. No public distribution or licence. Signing and upgrades adapted for a private repo (9.3, 9.6).
- **Scope:** WASM plugins, forms, comments, OIDC/SAML and the multilingual UI move to v1.x. GraphQL, `cms shell` and the "password" field type are dropped.

Feature tags: **[v1]** ships in v1.0. **[v1.x]** comes after launch but is designed for now. **[later]** is unscheduled.

---

## 1. Overview

A security-first, CLI-first content management system written in Go. The core ships as a single static binary, stores data in PostgreSQL and renders HTML server-side. Public pages are pre-rendered to disk and served by Caddy. It offers WordPress-style flexibility (custom post types, custom fields, blocks, media, redirects) without WordPress's in-process plugin model.

It is a private, single-owner project. It runs the owner's own website, and optionally client websites, with **one instance per site**: each site has its own server, database and admin users. Clients log in as Admins of their own site; the owner holds the Owner role on every site.

### 1.1 Goals
- Single static core binary (`CGO_ENABLED=0`) with embedded assets; trivial deploys and instant rollbacks
- Every capability available via CLI, admin UI and API through one service layer
- Strong defaults for security: typed queries, auto-escaped output, central authorisation, strict CSP, least-privilege database roles
- Flexible content modelling: custom post types, field groups and blocks defined in versioned files
- Fast public site: pages pre-rendered to pre-compressed files, served without touching Go or the database
- Low memory: runs comfortably on a 1 GB server alongside Postgres (budgets in 8.2)
- Zero-downtime, self-verifying deploys: every release is signed, proven rollback-safe in CI, and rolls back automatically on failure
- Scriptable and AI-agent friendly: `--json` output on every command

### 1.2 Non-goals (v1)
- Drop-in compatibility with WordPress themes or plugins
- Arbitrary third-party code running inside the core process
- Multi-database support (Postgres only)
- A visual page builder with free-form drag-and-drop layout (structured blocks only)
- GraphQL (REST with field selection and includes covers the use cases)
- Multi-site / multi-tenant (single site per instance; avoid global singletons so it can be added later)
- Self-patching: the CMS never modifies its own code. Upgrades install signed releases only (9.6)
- Public distribution: this is a private, single-owner project. No open-source release, public installers or third-party support

### 1.3 Design principles
1. **Core logic lives in services.** CLI, HTTP handlers, jobs and the API are thin adapters.
2. **Authorisation lives in services**, never in handlers.
3. **Explicit over magic.** Plain constructor injection, no reflection-based container.
4. **Everything auditable.** Mutations flow through commands that can be logged, queued and replayed.
5. **Boring dependencies.** Few, well-maintained libraries. Every new dependency needs a reason in the PR.
6. **Every commit is deployable.** If CI is green, the release is safe to ship and safe to roll back.
7. **Bounded everything.** Every cache, queue, pool, upload and user-supplied limit has a hard cap.

---

## 2. Architecture

### 2.1 Style
Modular monolith with a service layer and thin adapters. MVC appears only in the admin UI layer (handler → template).

```
cmd/cms/main.go            Wiring: config, DB, services, cobra root
internal/
  config/                  Env + YAML config loading and strict validation
  db/                      Migrations, sqlc generated code, tx helpers
  keys/                    KEK loading, DEK envelope encryption, rotation
  actor/                   Actor type, context helpers, permission checks
  content/                 Entries, working copies, post types, revisions, taxonomies
  fields/                  Field groups, field types, validation
  blocks/                  Block definitions, validation, rendering
  richtext/                ProseMirror JSON schema validation and HTML renderer
  media/                   Uploads, storage drivers, variant pipeline client
  redirects/               Redirect rules, matching, import/export
  auth/                    Users, sessions, passkeys, TOTP, API tokens, sudo mode
  rbac/                    Fixed roles (owner, admin, editor) and their capability sets
  menus/                   Navigation menus
  settings/                Site and system settings
  search/                  Full-text search (Postgres FTS)
  render/                  Theme engine, page rendering, disk cache, dependency tracking
  seo/                     Meta, sitemap, robots, schema.org
  events/                  Domain event bus
  jobs/                    Background queue and scheduler
  counters/                Batched in-memory counters (hits, 404s, token use)
  audit/                   Append-only, hash-chained audit log
  secaudit/                Self-audit: dependency vulns, config and runtime checks
  webhooks/                Outbound webhooks
  netx/                    Hardened outbound HTTP client (SSRF protection)
  http/
    admin/                 Admin handlers + html/template views
    public/                Public site handlers (cache misses and dynamic routes)
    api/                   REST/JSON API handlers
  cli/                     Cobra commands
web/                       Admin assets (hand-written CSS, vanilla JS, Tiptap, SortableJS), bundled with esbuild, embedded
themes/                    Default theme, embedded (overridable from disk)
migrations/                SQL migrations, each tagged expand or contract
deploy/                    Caddyfile, systemd units, cloud-init, deploy and upgrade scripts
```

The v1.x WASM plugin host (`internal/plugins/`) is not created until that phase.

### 2.2 Layers
| Layer | Responsibility | Must not |
|---|---|---|
| Adapters (CLI, HTTP, API, jobs) | Parse input, build command, call service, format output | Contain business rules or permission checks |
| Services | Business logic, authorisation, transactions, emit events | Know about HTTP or terminal I/O |
| Stores (sqlc) | Typed parameterised queries | Contain business rules |
| Events | Decouple side effects (cache, webhooks, search) | Block the originating transaction |

### 2.3 Actor model
Every service call receives a `context.Context` carrying an `Actor`:
- `UserActor` (session or API token)
- `SystemActor` (CLI, jobs, scheduler) with an explicit capability set. CLI-originated actions record the OS user and hostname in the audit log.
- `AnonymousActor` (public)

Services call `actor.Can(ctx, cap, resource)` before reading or mutating. Unauthorised access returns a typed error. Handlers never check permissions themselves.

### 2.4 Commands and events
Complex operations are command structs (`PublishEntry{ID, At, ExpectedVersion}`) handled by services. After commit, services emit events (`EntryPublished`, `MediaUploaded`, `RedirectChanged`). Subscribers handle cache invalidation, search indexing, webhooks and static rebuilds. Event-driven work that must not be lost is enqueued as a River job in the same transaction as the change.

### 2.5 Request path

```
Browser ─▶ Caddy (TLS, HTTP/3, compression)
            ├─ /admin/*, /api/*, /preview/*, dynamic routes ─▶ cms serve (active colour)
            ├─ rendered file exists for {host}{path} ─▶ served from disk (.br / .gz / raw), Go not involved
            └─ otherwise ─▶ cms serve: redirect match → render + write file → or 404
Media origin (separate host) ─▶ Caddy ─▶ variant files on disk / S3 / CDN
```

- Rendered pages live at `/var/lib/cms/public/<host>/<path>/index.html` with `.br` and `.gz` siblings.
- A page that is redirected, unpublished, trashed or becomes uncacheable has its file deleted before the change is visible, so Caddy never serves stale or private content.
- Only configured hosts are served. Unknown `Host` headers get `421 Misdirected Request` (prevents cache poisoning).

### 2.6 Deployment model: core and site bundles

Each site is one instance of the same core binary plus a **site bundle**:

| Repo | Contains | Released as |
|---|---|---|
| `cms` (core, one repo) | The binary, admin UI, default theme, migrations | Signed core release (9.3) |
| `site-<name>` (one per site) | `site.yaml`, `theme/`, `content-types/`, `field-groups/`, `blocks/`, non-secret config | Signed site bundle (`.tar.zst`) |

- `site.yaml` pins the core channel (`stable` or `canary`) and the minimum core version the site needs
- A site repo's CI runs `cms theme validate`, `cms block validate` and `cms schema diff` against the pinned core, renders every template against fixtures, then builds and signs the bundle
- On the server, `cms upgrade` installs both: new core releases from the site's channel, and new site bundles from the site repo. Each goes through the same verify → snapshot → migrate → blue/green → watch flow (9.5)
- A schema change in a site bundle is applied by `cms schema apply` during its deploy, as `cms_migrator`
- Content (entries, media, users) never lives in a repo; it stays in that site's database and storage
- Your own website is the **canary**: core releases reach it before any client site (9.4)
- New site repos are created with `cms new` (5.5). Their CI is a short workflow that calls a reusable workflow in the core repo (`uses: <owner>/cms/.github/workflows/site.yml@v1`), so the site pipeline is maintained in one place

---

## 3. Technology Stack

| Concern | Choice | Notes |
|---|---|---|
| Language | Go, pinned via the `toolchain` directive in `go.mod` | Upgraded by Renovate |
| Router | stdlib `net/http` (1.22+ patterns) | No router dependency |
| Database | PostgreSQL 18 (pinned major); 16+ supported | UUIDv7 IDs generated in Go, so no hard dependency on 18 |
| Driver / pool | `pgx` v5 + `pgxpool` | Separate pools for web and jobs |
| Queries | `sqlc` | Typed, parameterised |
| Migrations | `goose` (embedded) + `squawk` lint in CI | Forward-only, expand/contract tagged |
| Templates | `html/template` everywhere (admin, themes, blocks) | Auto-escaping; one template system to learn |
| Frontend (admin) | Vanilla JS modules + hand-written CSS, bundled with esbuild | No `unsafe-eval`, no inline scripts, styles or handlers |
| Rich text | Tiptap (ProseMirror) inside rich-text fields only | Stores JSON; rendered server-side by `internal/richtext` |
| Block reordering | SortableJS | Blocks are a JSON tree edited by the block editor, not a Tiptap document |
| Sessions | Own implementation: hashed IDs in Postgres, staged (`mfa`, `enroll`, `full`) | |
| Queue | `river` (Postgres-backed) | No Redis required |
| CLI | `cobra` | |
| Config | env vars + one YAML file (`koanf` or stdlib) | No viper. Unknown keys are an error |
| Sanitising | `bluemonday` | Only for the explicit raw-HTML field type |
| Image processing | `vips` CLI (`libvips-tools`) run as a subprocess per job | No cgo, no resident service; memory used only while a job runs. imgproxy available as an optional driver for high volume |
| IDs | UUIDv7 generated in Go (`google/uuid` `NewV7`) | |
| Passwords | argon2id | |
| WebAuthn | `go-webauthn/webauthn` | |
| Plugins [v1.x] | `wazero` (WASM) + webhooks | |
| Reverse proxy | Caddy 2 | TLS, static file serving, blue/green upstream switching |
| Observability | `slog`, Prometheus metrics, OpenTelemetry behind a build tag | OTel adds binary size and memory, so it is opt-in |

---

## 4. Feature List

### 4.1 Content management

#### Custom post types (CPTs) [v1]
- Built-in types: `page`, `post`
- Define unlimited custom types (e.g. `product`, `case_study`, `event`, `team_member`)
- **Definitions are file-first:** YAML files in `content-types/`, versioned in git, are the source of truth (see 12.5)
  - Admin UI can edit definitions in development and writes them back to files
  - In production the admin UI is read-only for definitions
  - `cms schema diff` exits non-zero when the database and files disagree; CI and `cms doctor` run it
  - `cms schema apply` applies file changes; `cms schema export` writes database state to files
- Per-type options:
  - Label (singular/plural), icon, menu position
  - Hierarchical (parent/child) or flat
  - Supports: title, slug, excerpt, featured image, author, revisions, scheduling
  - Public/private visibility, whether it has its own archive and single URL
  - URL pattern (e.g. `/blog/{slug}`, `/products/{category}/{slug}`)
  - Default field groups and allowed blocks
  - Default sort order and admin list columns
  - Taxonomies attached
  - Template hierarchy overrides
  - Cacheability: `static` (default), `uncached` (always rendered by Go)

#### Entries [v1]
- Statuses: `draft`, `scheduled`, `published`, `archived`, `trashed` (no review workflow: every user who can edit can publish)
- **Working copy and published revision:**
  - Every entry has a working copy (what editors edit) and, once published, a pointer to the published revision (what visitors see)
  - Editing a published entry changes only the working copy. The live page is unchanged until the editor publishes
  - Admin shows "unpublished changes" when the working copy differs from the published revision
  - Publishing snapshots the working copy into a new revision and moves the pointer in one transaction
  - "Discard changes" resets the working copy to the published revision
- **Optimistic concurrency:** every entry has a `version`. Saves carry the expected version; a mismatch returns a conflict error. The editor shows a conflict screen with a field-level diff. The API uses `ETag` / `If-Match`
- Entry locking: soft lock with presence ("Alex is editing"), advisory only; optimistic locking is the real protection
- Autosave writes the working copy every few seconds when changed. It does not create revisions. Offline-tolerant: failed autosaves retry with backoff and warn the editor
- Slug management with uniqueness per type, locale and parent; automatic redirect creation when a published slug changes
- Hierarchical ordering and drag-reorder (for pages/menus)
- Scheduled publish and scheduled unpublish (one scheduler sweep, see 4.15)
- Soft delete with trash and restore
- Duplicate/clone entry
- Bulk actions (publish, unpublish, trash, change author, assign taxonomy)
- Author attribution (byline, selectable from users)
- Password-protected entries (optional; always `uncached`)
- Preview links (signed, expiring tokens) that render the working copy

#### Revisions [v1]
- A revision is created on explicit save, publish, restore and scheduled publish. Not on autosave
- Diff view in admin (field-level and block-level)
- Restore any revision into the working copy
- Configurable retention (count or age), pruned by a scheduled job. The published revision is never pruned
- Revision snapshots are stored as JSONB; Postgres TOAST compresses them (`lz4` column compression)

#### Taxonomies [v1]
- Built-in: `category` (hierarchical), `tag` (flat)
- Custom taxonomies per post type (e.g. `product_type`, `region`)
- Term metadata via field groups
- Term archives with their own URLs and templates
- Merge, rename and reassign terms (with redirects where URLs change)

#### Relationships [v1]
- Entry-to-entry relationship fields (one-to-one, one-to-many, many-to-many)
- Reverse lookups ("entries that reference this one")
- Referential integrity: warn/block on deleting referenced entries

#### Multilingual (schema [v1], UI [v1.x])
- v1 ships the schema: `locale` and `translation_group_id` on entries, locale-aware slug uniqueness, locale in URL patterns
- v1.x adds: per-locale slugs and URL prefixes or domains, fallback locale rules, translation status in admin, hreflang

### 4.2 Custom fields (ACF-style) [v1]

Field groups attach to post types, taxonomies, users, media, options pages or specific templates, with location rules. Field groups are defined in files, like content types.

**Field types**
| Category | Types |
|---|---|
| Basic | text, textarea, number, range, email, URL, slug |
| Content | rich text, markdown, code, oEmbed (allow-listed providers), raw HTML (sanitised, capability-gated) |
| Choice | select, multi-select, checkbox, radio, true/false, button group |
| Date/time | date, datetime, time, date range |
| Media | image, file, gallery, video |
| Relational | entry link, relationship, taxonomy term, user |
| Structured | group, repeater, flexible content (blocks), key-value |
| Layout | tab, accordion, message/help text |
| Special | colour picker, map/coordinates, link (url + label + target), icon picker |

The v0.1 "password" field type is dropped. Secrets do not belong in content.

**Field options (all types)**
- Label, name (machine key), instructions, placeholder, default value
- Required, min/max, regex pattern (RE2), uniqueness
- Conditional logic (show field X when field Y equals Z)
- Column width in the admin form
- Per-field read/edit capability
- Translatable flag
- Searchable flag (included in FTS index)
- Filterable flag: creates a generated column plus B-tree index for that field (not a broad GIN index)

**Storage**
- Values stored as JSONB in the working copy and revisions, validated against the schema on write
- Filterable fields get promoted generated columns on `entries` via a generated migration (`cms schema apply` emits it)
- Schema versioning: each field group has a version, and `cms schema migrate-data` handles renames and type changes as a resumable River job

**Options pages**
- Global settings pages defined by field groups (e.g. "Site Footer", "Social Links")
- Stored as singleton records, available in every template. Changing one invalidates every page that reads it (dependency tracking, 4.12)

### 4.3 Block builder [v1]

Structured, validated blocks for flexible page layouts (similar to ACF Flexible Content / Gutenberg, but schema-driven).

**Block definition** (file-based, versionable):
```yaml
name: hero
label: Hero
icon: layout-top
category: layout
fields:
  - { name: heading, type: text, required: true }
  - { name: subheading, type: textarea }
  - { name: image, type: image }
  - { name: cta, type: link }
  - name: style
    type: select
    options: [light, dark, image]
render: blocks/hero.gohtml     # html/template only
preview: blocks/hero.png
allowed_in: [page, landing_page]
cache: static                  # static | uncached
```

**Capabilities**
- Block library: each block = field schema + render template + optional editor preview
- Nested blocks (columns, tabs, accordions containing other blocks), with a depth limit (default 4)
- Reusable blocks / global blocks (edit once, update everywhere; dependency-tracked)
- Block patterns: saved arrangements of multiple blocks, insertable as a unit
- Per-post-type allowed block lists
- Editor: each block is a card in the JS block editor; reorder with SortableJS; duplicate, collapse, nested zones
- Block-level visibility rules: publish window (handled by scheduled re-render). "Logged-in only" makes the page `uncached`
- Block validation on save (server-side, against schema)
- Block versioning and migration when definitions change
- Block-level anchors/IDs and CSS class options
- Live preview pane in the admin (iframe rendering the working copy via the real theme, refreshed on change, debounced ~300 ms)
- Built-in starter blocks: rich text, heading, image, gallery, video embed, quote, button, columns, accordion, FAQ, CTA, hero, card grid, table, code, embed (allow-listed providers), spacer, entry list (query block). Form block in v1.x
- **Query block:** renders a list of entries by type/taxonomy/filter/sort/limit (limit capped at 100), with pagination. Pages with query blocks are re-rendered when any matching entry changes
- Stored as an ordered JSON tree: `[{id, type, version, data, children}]`

**Rendering**
- Each block type maps to one `html/template`
- Output escaped by default; raw HTML only through the explicit sanitised field type
- Rich text is rendered from validated ProseMirror JSON by `internal/richtext`, never by passing stored HTML through
- Embeds render as sandboxed iframes on the media origin or click-to-load facades. Provider HTML is never inlined
- Block render results cached in a bounded LRU keyed by content hash (byte cap, default 16 MB)

### 4.4 Media library [v1]

**Upload & storage**
- Drag-and-drop, multi-file, paste-from-clipboard and resumable uploads (tus protocol, or S3 multipart when using S3)
- Uploads stream to storage; files are never buffered whole in memory. SHA-256 computed while streaming
- Storage drivers: local disk, S3-compatible (DigitalOcean Spaces, S3, R2, MinIO)
- Content-addressed storage with SHA-256 deduplication
- Configurable max size and allowed types per role
- **Security:** MIME sniffing by content (not extension), extension allow-list, randomised stored filenames, files served from a separate media origin by default with `Content-Security-Policy: sandbox`, `nosniff` and `Content-Disposition` where appropriate, SVG sanitised or rasterised, optional antivirus hook (ClamAV, out of process)
- EXIF stripping (default on) to remove GPS data
- Decompression-bomb protection: maximum pixel count (default 50 MP) checked from headers before decoding

**Processing (always out of process)**
- Images are never decoded in `cms serve` or `cms worker`. The worker runs `vips` as a throwaway subprocess per image, inside a transient systemd scope (`systemd-run --scope -p MemoryMax=256M -p RuntimeMaxSec=60`) or rlimits where systemd is unavailable. Variants are stored content-addressed
- Inputs and outputs pass through temp files in a private directory; the subprocess gets no network access and no access to config or secrets
- Automatic responsive sizes (configurable named sizes: `thumb`, `medium`, `large`, plus custom)
- WebP/AVIF generation with originals retained
- On-demand transforms only for **named presets** via signed URLs (`/media/{id}/{preset}.{format}`). Arbitrary dimensions are rejected, so the URL space cannot be used to fill the disk
- Focal point selection for smart cropping
- Video/audio: store and serve. Poster frames and PDF thumbnails are optional worker extensions [v1.x]

**Organisation**
- Folders and collections
- Tags and custom fields on media (via field groups)
- Alt text, title, caption, credit, description (with accessibility warnings when alt is missing)
- Search by filename, metadata, type, date, uploader
- Usage tracking: where each asset is used, with safe-delete warnings
- Bulk upload, bulk edit metadata, bulk move, bulk delete
- Replace file while keeping the URL and references
- Find unused media; orphan cleanup job
- Import from URL via the hardened client (`internal/netx`)

**Delivery**
- Variants served by Caddy or CDN with `Cache-Control: public, max-age=31536000, immutable`
- Optional private/signed media for protected content (served by Go, short-lived signatures)
- Optional image CDN integration (e.g. Cloudflare Images)

### 4.5 Redirects and URL management [v1]

- Redirect types: 301, 302, 307, 308, 410 (gone), 451
- Match types: exact, prefix, wildcard, regex (with capture groups: `/old/(.*)` → `/new/$1`)
- Query string handling: ignore, preserve, or match
- Case-insensitive and trailing-slash normalisation options
- Source host scoping (for multi-domain setups)
- **Automatic redirects** created when a published slug or parent changes, or when an entry is trashed (configurable)
- Redirect chain and loop detection, with auto-flattening
- 404 log with hit counts, referrers and "create redirect" shortcut
- Hit counters and last-hit timestamps, so stale rules can be found
- Import/export CSV and JSON (`cms redirects import file.csv --dry-run`)
- Bulk operations and tagging/grouping of rules
- Test tool in admin and CLI: `cms redirects test /some/path`
- Fast matching: exact rules in an in-memory map, prefix rules in a radix tree, regex rules compiled once, all rebuilt on `RedirectChanged` events
- Creating a redirect deletes any rendered file at its source path, so Caddy falls through to Go
- **Counters are batched:** hits and 404s are counted in memory (`internal/counters`) and flushed every 60 s. The 404 tracker holds at most 10,000 distinct paths per flush window and samples beyond that
- Safeguards: block open redirects to external hosts unless explicitly allowed; cap regex rule count (default 500) and pattern length; RE2 guarantees linear-time matching
- Canonical URL enforcement (HTTPS, preferred host, www/non-www)

### 4.6 Navigation and menus [v1]
- Multiple named menus (header, footer, mobile, etc.)
- Items: entry link, term link, custom URL, anchor, label-only group
- Nested, drag-and-drop ordering
- Per-item attributes: target, rel, CSS class, icon, visibility rules
- Menu locations declared by themes
- Links resolve by ID, so slug changes update menus automatically. Menu changes invalidate every page that renders that menu
- Menu items loaded in one query per menu (no per-item queries)
- Mega-menu support through block-based item content [v1.x]

### 4.7 Users, roles and permissions

**Authentication [v1]**
- Email + password (argon2id), breached-password check (k-anonymity HIBP, optional)
- TOTP 2FA with recovery codes (mandatory); passkeys (WebAuthn) as an alternative second factor. Passwordless passkey sign-in [v1.x]
- Magic link login (optional). Links are single-use, expire in 15 minutes, and are bound to the requesting browser by a cookie nonce
- Session management: `__Host-` prefixed cookies, `Secure`, `HttpOnly`, `SameSite=Strict` for admin; rotation on login and privilege change; per-device session list with revoke; idle and absolute timeouts
- Session `expiry` touched at most once every 5 minutes (not on every request)
- **Sudo mode:** re-authentication (password or TOTP), valid 10 minutes, required for changing users and roles, API tokens, 2FA, exports, keys and security settings
- Login throttling per account and per IP, lockout and new-device alerts
- Password reset with single-use, expiring, browser-bound tokens
- 2FA mandatory for every account (TOTP with recovery codes; passkeys optional on top). There are no low-privilege accounts, so there are no exemptions
- SSO: OIDC (Google, Microsoft, generic) [v1.x]; SAML [later]

**Authorisation [v1]**
- Capability checks stay in services (`actor.Can`), but roles are **fixed in code**. There is no custom role editor
- Three roles:

| Capability area | Owner | Admin | Editor |
|---|---|---|---|
| Entries, media, menus, taxonomies, SEO fields | Yes | Yes | Yes |
| Publish, schedule, trash | Yes | Yes | Yes |
| Redirects, 404 log, site settings, options pages | Yes | Yes | No |
| Manage Admin and Editor users | Yes | Yes | No |
| Manage Owner users | Yes | No | No |
| API tokens, webhooks | Yes | Yes (own tokens) | No |
| Raw-HTML field, embeds outside the allow-list | Yes | No | No |
| Security page, audit log, backups and restore, keys | Yes | View security summary only | No |
| Cache, jobs, system health | Yes | View only | No |

- The Owner role is held by you on every site. A client can never remove or demote an Owner, and the last Owner cannot be deleted
- Content types, field groups, blocks and themes come from the site bundle, so no role can change them in production
- **API tokens:** format `cms_pat_<id>_<secret>` (prefix enables GitHub secret scanning); lookup by ID, constant-time comparison of SHA-256 of the secret; capabilities limited to the creator's role, mandatory expiry (default 90 days), last-used tracking (batched)
- Removed in v0.3: custom roles, per-post-type and per-taxonomy scoping, ownership rules, per-entry ACLs and impersonation. With a handful of trusted admins per site these add risk and code without adding value

**User profile [v1]**
- Display name, avatar, bio (for bylines), locale and preferences

### 4.8 Forms [v1; builder, export, webhooks, CAPTCHA and uploads v1.x]
- Form builder using the same field types as the block builder
- Submissions stored in the DB, with export, notifications and webhooks, and a retention policy
- Spam protection: honeypot, time-based, rate limits, Turnstile/hCaptcha
- File upload fields routed through the media security pipeline
- Email sending via SMTP or provider APIs (Postmark, SES, Resend); header injection prevented by construction
- Pages containing forms stay cacheable: the CSRF token and timing field are fetched by a small script

### 4.9 Comments [later]
Not planned. If a site needs comments, use an external service embedded as a click-to-load facade, so the page stays cacheable.

### 4.10 SEO [v1]
- Per-entry meta title, description, canonical, robots, Open Graph and Twitter fields
- Templates for default meta patterns (`{title} | {site}`)
- Auto-generated XML sitemaps (split by type, with `lastmod`), rendered to disk, and a `robots.txt` editor
- Schema.org JSON-LD generation (Article, Product, Organization, BreadcrumbList, FAQ)
- Breadcrumbs
- Redirect and 404 tooling (see 4.5)
- RSS/Atom feeds per type and taxonomy, rendered to disk
- SEO and accessibility checks in the editor (title length, missing alt, heading order, link text)
- Optional IndexNow ping on publish
- Hreflang [v1.x, with multilingual]
- Social image generation [v1.x]

### 4.11 Search [v1]
- Postgres full-text search (`tsvector`, weighted fields, language configs) over published content
- `search_vector` written by the search service (not a trigger), because only the service knows which fields are searchable
- Queries use `websearch_to_tsquery`. GIN index on `search_vector`; trigram (`pg_trgm`) index on title only, for typo tolerance
- Admin search across entries (working copies), media, users, settings
- Public search endpoint and template: rate-limited, result limit capped, served by Go (`uncached`)
- Index updates via events, with `cms search reindex`
- Optional adapter for Meilisearch/Typesense [later]

### 4.12 Themes and rendering [v1]

- Themes are directories of `html/template` files plus a manifest (`theme.yaml`), embedded or loaded from disk
- Themes are installed by deploy or CLI only. There is no theme upload in the admin UI
- Template hierarchy: `single-{type}`, `archive-{type}`, `taxonomy-{tax}`, `page-{slug}`, `404`, `search`, `home`, with a clear fallback order
- Partials, layouts and slots
- Restricted template function set (no filesystem, network, or exec access; no raw-HTML escape hatch except for the sanitised field type)
- Theme-declared: menu locations, image sizes, block overrides, settings fields, and the public CSP
- **Built-in base theme (parent):** embedded in the binary. Provides the document head (SEO meta, Open Graph, JSON-LD, canonical, hreflang), responsive image partial, skip links and landmark structure, pagination, breadcrumbs, cookie/consent partial, error pages and feed/sitemap links
- Site themes declare `extends: base` and override only what is unique to the site. Improvements to the base theme reach every site with a core release, without editing site repos
- Overrides are by template name; `cms theme validate` warns when an override shadows a base template that changed in the pinned core version
- Asset pipeline for theme CSS/JS (fingerprinted filenames, long cache headers)
- Default theme performance budget: < 50 KB CSS + JS, no JS required to read content, Lighthouse 100, WCAG 2.2 AA
- Preview mode and theme switching via CLI and admin
- `cms theme dev`: live reload of templates and blocks for theme authors

**Render-to-disk cache**
- On publish, and on a cache miss for a `static` route, the page is rendered and written atomically (temp file, `fsync`, rename) as `index.html`, `index.html.br` and `index.html.gz` under `/var/lib/cms/public/<host>/<path>/`
- Caddy serves these files directly (section 9.5). Go and Postgres are not touched for cached pages
- **Dependency tracking:** each render records what it read (entries, menus, options pages, settings, reusable blocks, query block filters) in `render_deps`. A change deletes the affected files, then re-renders them in the background via River (bounded concurrency)
- Theme or settings changes re-render the whole site in the background; old files keep serving until each page is replaced
- **Cacheability rules:** a page is `uncached` (always rendered by Go, never written to disk) if its type is `uncached`, it is password-protected, or it contains a logged-in-only block. Forms fetch their tokens by script so the page stays `static`
- Edge cache support: `Cache-Control`, `ETag`, `Surrogate-Key` headers, and purge hooks for Cloudflare/Fastly/Varnish, fired by the same dependency tracking
- **Static export:** `cms build --out ./dist` renders every `static` page to a folder (the same files as the disk cache). Not supported in static export: search, forms, comments, password-protected entries, logged-in blocks, private media. `cms build` lists any pages it skipped

### 4.13 Admin UI [v1]

- Server-rendered with `html/template`, progressive JS only where needed (editor, media, menus), no SPA
- Dashboard: recent activity, scheduled items, drafts with unpublished changes, system health, security summary, quick actions
- List screens: sortable, filterable, saved views, column picker, bulk actions, keyboard navigation
- Editor: block editor, field groups, sidebar with status/schedule/taxonomies/SEO, revision panel, live preview, conflict screen
- Global search / command palette (Cmd+K)
- Keyboard shortcuts
- Responsive layout, dark mode
- Accessibility target: WCAG 2.2 AA (keyboard navigation, focus management, ARIA, contrast), checked by axe-core in CI
- Notifications and flash messages, in-app activity feed
- Localised admin interface (i18n)
- **Strict CSP:** `script-src 'self'` with nonces where required; no `unsafe-eval`, no `unsafe-inline`. Tiptap runs with `injectCSS: false`; no inline handlers. CI fails on any CSP violation (9.2)
- Admin assets are bundled with esbuild; Tiptap is loaded only on screens with a rich-text field
- Optional separate admin hostname (own cookie scope and CSP), with an IP allow-list hook

### 4.14 API [v1 read-only, v1.x write]

- REST/JSON API over the same services, versioned under `/api/v1`
- Auth via session (same-site), API tokens; OAuth2 client credentials [v1.x]
- Read endpoints for all public content types, with filtering, sorting, field selection, pagination (cursor-based, page size capped at 100) and includes/expansion (depth capped at 2)
- Write endpoints [v1.x], with `If-Match` (optimistic concurrency) and `Idempotency-Key` support on POST. In v1, writes go through the admin UI and CLI
- OpenAPI spec generated and published at `/api/openapi.json`; checked in CI for breaking changes
- Deprecation policy: a version is supported for at least 12 months after its successor ships; `Deprecation` and `Sunset` headers
- Rate limiting per token/IP, CORS configuration
- Webhooks (see 4.16) for outbound events
- Headless mode: disable the public renderer and serve API only
- Typed client generation: `cms schema export --go|--ts` emits types for content definitions

### 4.15 Background jobs and scheduler [v1]
- Postgres-backed queue (River) with retries, backoff, unique jobs and dead-letter handling
- **All jobs are idempotent.** During a deploy a job may run on the old and new worker; running twice must be safe
- **Scheduling has one source of truth:** `scheduled_at` / `unpublish_at` columns. A sweeper job runs every minute and publishes or unpublishes due entries idempotently. No per-entry scheduled jobs
- Built-in jobs: scheduler sweep, image variant generation, revision pruning, orphan media cleanup, re-render queue, sitemap regeneration, search reindex, email, webhook delivery, counter flush, log retention, backups, security audit
- Cron-style scheduler for recurring tasks
- Concurrency limits per queue (default 2–4 on small hosts); render queue bounded so a site-wide re-render cannot starve other work
- Admin UI for job status; CLI: `cms jobs list/retry/cancel/purge`
- Separate `cms worker` process using the same binary (or `cms serve --with-worker` for small installs; image processing is always out of process regardless)

### 4.16 Extensibility

No in-process third-party code. Isolation levels:

1. **Compile-time modules [v1]** (Caddy-style): first-party or trusted extensions registered via a `Module` interface and compiled into a custom binary (`cms build-custom`)
2. **Webhooks and API clients [v1]:** subscribe to events (`entry.published`, `media.uploaded`, etc.) with HMAC-SHA256-signed payloads (timestamped, replay window 5 minutes), retries and delivery logs. Secrets stored encrypted (7.3). Delivery via the hardened client
3. **WASM plugins [v1.x] (`wazero`):** sandboxed, capability-declared (`read:entries`, `http:example.com`), memory-limited (page cap) and time-limited (context deadline with `WithCloseOnContextDone`), with a manifest and signed packages. Hooks run off the request path where possible: content validation, field transformation, custom block render, custom field types

Plugin manifest example [v1.x]:
```yaml
name: reading-time
version: 1.0.0
capabilities: [read:entry.body]
hooks: [entry.before_save]
limits: { memory_mb: 16, timeout_ms: 50 }
```

### 4.17 Import / export and migration [v1]
- Export full site or selected types to JSON/NDJSON, including media manifest
- Import with dry-run, conflict strategies (skip/overwrite/merge) and ID mapping
- **WordPress importer [v1.x, when the first WordPress migration is booked]:** WXR file → posts, pages, taxonomies, media, authors and redirects (complete); core Gutenberg blocks mapped to built-in blocks; shortcodes and unknown blocks kept as raw-HTML blocks, with a report of every unmapped item
- CSV import for entries and redirects
- Schema export/import (content types, field groups, blocks) for moving between environments (normally via git, see 4.1)
- Staging → production content sync via CLI

### 4.18 Audit, logging and compliance [v1]
- **Append-only audit log:** who/what/when/before-after diff for all mutations, logins, role changes, token changes and exports
  - The runtime DB role has `INSERT` and `SELECT` only on `audit_log` (no `UPDATE`/`DELETE`)
  - Each row stores the SHA-256 of the previous row (hash chain); `cms audit verify` checks the chain
  - Optional off-box shipping (syslog/OTLP/S3) so a compromised host cannot rewrite history
  - Partitioned by month; retention drops whole partitions, run by the migrator role
- **Personal data and the audit log:** audit rows reference users by ID and never copy user profile fields into diffs. Deleting a user replaces their name and email with a tombstone, so the hash chain stays intact
- Structured logs (`slog`, JSON), request IDs and correlation with job IDs
- Retention policies for every log-style table (`not_found_log`, `webhook_deliveries`, `form_submissions`, sessions)
- GDPR: delete or anonymise admin users; form submission export, retention and erasure arrive with forms [v1.x]. For client sites you act as data processor; the contract side is outside this spec
- Cookie/consent block (optional)

### 4.19 Backup and restore [v1]
- **Continuous:** WAL archiving to S3-compatible storage (pgBackRest or WAL-G) for point-in-time recovery. Target RPO ≤ 5 minutes
- **Logical:** `cms backup create` (DB dump + media manifest, optional media archive) to local or S3-compatible storage
- Backups encrypted at rest (age or KMS); backup credentials separate from app credentials
- `cms backup restore` with integrity checks
- Scheduled backups with retention rules
- Automatic pre-migration snapshot on every deploy (9.5)
- **Restore drill in CI**, weekly: restore the latest backup into a throwaway Postgres, run `cms doctor`, alert on failure
- `cms doctor` checks backup and WAL archive freshness
- `pg_dump`/`pg_restore` must match the server's major version; the container image bundles them, and `cms doctor` checks the host version for binary installs

### 4.20 Security audit (self-check) [v1]

The CMS can inspect its own build, configuration and data for known weaknesses, report them in the CLI and admin, and alert the owner. It reports only. It never patches itself, because a fix means a rebuild and redeploy (see `cms upgrade`, 9.6).

**Dependency vulnerability scanning**
- On start and on a schedule (default daily), read the embedded module list via `runtime/debug.ReadBuildInfo`
- Check it against the Go vulnerability database using `golang.org/x/vuln` (binary mode), with the OSV.dev API as an alternative source
- Each finding records: advisory ID (GO-/CVE-/GHSA-), severity, affected module and version, fixed version, whether the vulnerable symbol appears reachable, and first-seen date
- Also compares the Go toolchain version the binary was built with against known stdlib advisories
- Compares the running CMS version against the latest release and flags security releases
- Findings can be acknowledged or suppressed with a reason and optional expiry, and every action is audit-logged
- **Outbound lookup is on by default** (it only fetches public advisory data) with one setting to disable it (see 12.9). Goes through the hardened HTTP client
- **Offline mode:** accepts a downloaded vulnerability database snapshot (`cms security db import`) for air-gapped servers

**Configuration and runtime checks**
- Weak, default or missing secrets, KEK and session keys
- Debug or development mode enabled in production
- Missing HTTPS, HSTS, or insecure cookie flags; CSP absent or weakened
- Any account without 2FA (should be impossible; checked as a defence in depth), sites with no Owner account
- Stale or never-used API tokens, tokens without expiry, dormant privileged accounts, accounts using breached passwords (if the HIBP check is enabled)
- Runtime DB role with excess privileges (superuser, DDL rights, `UPDATE`/`DELETE` on `audit_log`, `CREATE` on public schema)
- Config, upload and secrets paths with permissive file modes
- Postgres reachable from non-local interfaces, where detectable
- `trusted_proxies` misconfigured (app sees only proxy IPs, or trusts everyone)
- Media served from the admin origin
- Backups or WAL archive missing or older than the configured threshold
- Audit hash chain broken
- Rate limiting disabled on login and password reset

**Content and extension checks**
- Uploaded files whose sniffed type differs from the declared type, or on a blocked list
- Redirect rules pointing to external hosts that are not allow-listed
- Themes: static scan for disallowed template functions and external script includes
- Plugins [v1.x]: manifest capabilities versus granted capabilities, unsigned packages, known-bad versions

**Surfaces**
- CLI: `cms security audit`, `status`, `history`, `ack`, `db import` (all with `--json`; non-zero exit when findings at or above `--fail-on` severity exist)
- Admin: Security page with a summary score, findings grouped by severity, remediation links and acknowledgement controls. Visible only to roles holding the `security.view` capability
- Scheduler: recurring `security.audit` job via the job queue
- Alerts: email, outbound webhook (`security.finding.created`) and an admin banner for critical findings, with de-duplication and a configurable quiet period
- `cms doctor` includes a fast subset (config checks plus cached dependency results)

**Limits and trust model**
- In-app results are early warning, not the authority. A compromised instance cannot be trusted to report on itself, so `govulncheck` in CI (source mode, with reachability analysis) stays the authoritative gate before deploy
- Binary-mode reachability is less precise than source mode, so expect some noise and show the affected package and symbol on each finding
- The audit job runs with a read-only `SystemActor` and cannot modify content or configuration

### 4.21 Ecommerce [later]

Not in v1 or v1.x. Recorded so the design stays compatible.

- **Shop-lite (planned approach):** products are a normal content type (price, variants, images). The cart lives in the visitor's browser. Checkout is a small `uncached` endpoint that re-reads prices from the database (never trusting the browser), creates a **Stripe Checkout** session and redirects to it. Stripe handles card data, VAT (Stripe Tax), receipts and refunds; card data never touches the server. A signed Stripe webhook records orders in an `orders` table and notifies the site owner. Target: small shops up to a few dozen products. Ships with a `shop` starter
- **Larger stores:** the CMS runs the site and content; the shop is Shopify (Buy Button or Storefront API). No commerce built in
- **Not planned:** full built-in commerce (stock, fulfilment, customer accounts, payment processing)

---

## 5. CLI Specification

Single binary, cobra-based. **Every command supports `--json`, `--quiet`, `--config`, `--env`, and non-zero exit codes on failure.** Destructive commands support `--dry-run` and require `--yes` in non-interactive mode. Security-sensitive commands run as `--as <user>` require that user's sudo-mode re-authentication.

### 5.1 Command tree
```
cms new <name> [--starter brochure|blog|blank] [--git] [--github]   # new site repo (laptop)
cms make type|block|field-group <name>           # generators inside a site repo
cms dev                                          # local dev: embedded Postgres, live reload
cms install                                      # set up a server for a site (was: cms init)
cms serve [--addr] [--with-worker]
cms worker
cms migrate up|status|create <name> [--expand|--contract]
cms migrate down                                 # dev builds / CMS_ENV=development only
cms doctor [--fix]                               # --fix: safe, idempotent repairs only (permissions, missing dirs, re-render)
cms version
cms upgrade [--channel stable] [--check]         # verified, blue/green, auto-rollback (9.6)
cms config show|validate
cms keys rotate|status

cms user create|list|get|update|delete|reset-password|disable|enable
cms user 2fa reset <id>
cms user role <id> owner|admin|editor
cms token create|list|revoke

cms type list|get                                # content types (definitions live in files)
cms schema apply|export|diff|migrate-data [--go|--ts]
cms field-group list|get
cms block list|get|validate

cms content list|get|create|update|delete
cms content publish|unpublish|schedule|trash|restore|discard <id>
cms content revisions <id>
cms content revert <id> <revision>
cms content import|export [--type] [--file]
cms content clone <id>
cms term list|create|update|delete|merge
cms menu list|get|import|export

cms media upload <files...>
cms media list|get|update|delete
cms media regenerate [--size] [--id]
cms media cleanup [--dry-run]
cms media import-url <url>

cms redirects list|add|update|delete
cms redirects import|export
cms redirects test <path>
cms redirects check                              # loops/chains
cms 404 list|top|clear

cms theme list|activate|validate|dev
cms cache clear|warm|status [--tag]
cms build --out ./dist                           # static export
cms search reindex
cms sitemap generate

cms jobs list|retry|cancel|purge
cms webhook list|create|delete|test|deliveries

cms backup create|list|restore|verify
cms db backup|restore                            # pg_dump wrapper (version-checked)
cms audit list|verify [--user] [--since]
cms security audit [--fail-on high] [--offline]
cms security status|history|ack <id>
cms security db import <file>
cms settings get|set|list
cms wp-import <file.xml> [--dry-run]
cms seed [--fixtures]                            # refuses to run when CMS_ENV=production
```

Removed from v0.1: `cms shell` (an unaudited production REPL), `cms type create|delete` (definitions are file-first). `cms plugin …` returns in v1.x.

### 5.2 Output conventions
- Human output: tables and concise messages
- `--json`: stable, versioned schema, never mixes logs into stdout (logs to stderr)
- Exit codes: `0` ok, `1` general error, `2` usage, `3` validation, `4` permission, `5` not found, `6` conflict
- Shell completions for bash/zsh/fish
- CLI commands run as `SystemActor` with a configurable capability set, or authenticate as a user/token with `--as`

### 5.3 `cms doctor`
Checks: config validity, DB connectivity and version, required extensions (`pg_trgm`, `unaccent`), migration state, `cms schema diff`, storage write access, render directory writable, file permissions, secrets and KEK presence and strength, DB role privileges, base URL/HTTPS config, trusted proxies, queue health, `vips` binary present and version-checked, backup and WAL freshness, `pg_dump` version match, missing alt text count, redirect loops, audit chain. Doubles as the post-deploy smoke test (`cms doctor --json` in the deploy script and CI). `--fix` performs only safe, idempotent repairs and never changes content, security settings or schema.

### 5.4 `cms install` and `cms dev`
- `cms install` (server): takes a site bundle (or the default theme), writes config, generates the KEK and session keys into systemd credentials (or prints them for a secret manager), creates the `cms_migrator` and `cms_app` roles (given a Postgres superuser URL once, never stored), runs migrations, and creates your Owner account with a passkey, and optionally the client's first Admin (invite link). Target: fresh server to working site in under 5 minutes
- `cms dev` (laptop, inside a site repo): starts an embedded Postgres (no Docker needed), runs migrations, loads the repo's `fixtures/`, serves with live reload of templates, blocks, content types and theme assets. In the core repo it also live-reloads Go and admin assets. `docker-compose.yml` remains for MinIO and Mailpit

### 5.5 `cms new` and `cms make`

`cms new <name>` creates a site repo in a new folder. The starters are embedded in the binary, so a new project always matches the installed `cms` version.

- Interactive by default (starter, git, GitHub); flags for scripting: `--starter`, `--git`, `--github` (creates a private repo with `gh`)
- Writes `site.yaml` with the site name, the pinned core version (the installed one) and the release channel (`stable`)

| Starter | Contents |
|---|---|
| `brochure` | Home, About, Services (content type), Case studies (content type), Team, Contact page; hero, CTA, testimonial, FAQ, card grid and logo strip blocks |
| `blog` | Posts, categories, tags, author archive, RSS, related posts, reading time |
| `blank` | `page` type, one layout, the base theme only |
| `shop` | Listed as "planned" until shop-lite exists (4.21) |

Generated layout:
```
<name>/
├── site.yaml                  name, domain, pinned core version, channel
├── theme/
│   ├── theme.yaml             extends: base
│   ├── layouts/default.gohtml
│   ├── templates/             home, single-page, archive-post, 404 (only what the starter overrides)
│   └── assets/site.css
├── content-types/
├── field-groups/
├── blocks/<block>/            <block>.yaml + <block>.gohtml + preview.png
├── fixtures/                  sample content so `cms dev` is never empty
├── .github/workflows/site.yml calls the shared workflow in the core repo
└── README.md                  run, deploy, add a block
```

`cms make` generators (run inside a site repo, `--dry-run` supported):
- `cms make type <name>`: content type YAML plus `single-<name>` and `archive-<name>` templates
- `cms make block <name>`: block folder with YAML schema, template and preview stub
- `cms make field-group <name>`: field group YAML with a location rule stub

Starters are tested in core CI: each one is generated, validated with `cms theme validate` and `cms schema diff`, rendered against its fixtures, and checked against the default theme's performance and accessibility budgets.

---

## 6. Data Model (core tables)

All primary keys are UUIDv7 (`uuid`, generated in Go with `uuid.NewV7()`) unless noted. All foreign keys are enforced and indexed. `*_enc` columns hold envelope-encrypted values (`key_id`, nonce, ciphertext).

```
users(id, email UNIQUE, password_hash, role, display_name, status, locale, created_at, ...)
webauthn_credentials(id, user_id, name, credential, last_used_at) -- passkeys; TOTP seeds are encrypted columns on users
sessions(token PK, data, expiry)                                  -- index on expiry
api_tokens(id, user_id, name, secret_sha256, capabilities, expires_at NOT NULL, last_used_at)
-- roles are fixed in code; users.role is one of owner | admin | editor

content_types(id, name UNIQUE, config jsonb, version, source_hash)
field_groups(id, name UNIQUE, config jsonb, location_rules jsonb, version, source_hash)
block_definitions(id, name UNIQUE, schema jsonb, version, source_hash)

entries(id, type, status, version int NOT NULL,
        slug, parent_id, author_id, locale, translation_group_id,          -- live identity (routing)
        draft_title, draft_slug, draft_fields jsonb, draft_blocks jsonb,   -- working copy
        published_revision_id, published_version,
        search_vector tsvector,                                            -- from published revision
        published_at, scheduled_at, unpublish_at, created_at, updated_at, deleted_at)
  UNIQUE INDEX (type, locale, parent_id, slug) NULLS NOT DISTINCT WHERE deleted_at IS NULL
  INDEX (type, status, published_at DESC)
  INDEX (scheduled_at) WHERE scheduled_at IS NOT NULL
  INDEX (unpublish_at) WHERE unpublish_at IS NOT NULL
  GIN (search_vector)
  + generated columns and B-tree indexes for filterable fields (emitted by cms schema apply)
entry_revisions(id, entry_id, number, kind, author_id,
                title, slug, fields jsonb, blocks jsonb, created_at)
  UNIQUE (entry_id, number); jsonb columns use lz4 compression
entry_relations(source_id, target_id, field, position, PRIMARY KEY(source_id, field, position))
  INDEX (target_id)                                                        -- reverse lookups
taxonomies(id, name UNIQUE, config jsonb)
terms(id, taxonomy_id, parent_id, slug, name, fields jsonb, UNIQUE(taxonomy_id, parent_id, slug))
entry_terms(entry_id, term_id, PRIMARY KEY(entry_id, term_id)), INDEX (term_id)
reusable_blocks(id, name, blocks jsonb, version)
block_patterns(id, name, blocks jsonb)

media(id, sha256, filename, mime, size, width, height, storage_key, folder_id,
      alt, title, caption, focal_x, focal_y, fields jsonb, uploader_id, created_at, deleted_at)
  INDEX (sha256)
media_variants(id, media_id, preset, format, storage_key, size, UNIQUE(media_id, preset, format))
media_folders(id, parent_id, name)
media_usage(media_id, entry_id, field_path, PRIMARY KEY(media_id, entry_id, field_path))

redirects(id, source, match_type, target, status_code, preserve_query,
          host, is_auto, hits, last_hit_at, created_by, created_at)   -- hits flushed in batches
  UNIQUE (host, source, match_type)
not_found_log(id, host, path, referrer, hits, first_seen, last_seen, UNIQUE(host, path))

render_deps(page_key, dep_type, dep_id, PRIMARY KEY(page_key, dep_type, dep_id))
  INDEX (dep_type, dep_id)                                            -- "which pages read X?"

menus(id, name UNIQUE, location)
menu_items(id, menu_id, parent_id, position, type, ref_id, url, label, attrs jsonb)
  INDEX (menu_id, parent_id, position)

settings(key PK, value jsonb, updated_at)
options_pages(id, name UNIQUE, fields jsonb, version)
secrets(id, name UNIQUE, value_enc, created_at, rotated_at)           -- SMTP, S3, OIDC credentials

webhooks(id, url, secret_enc, events, active)
webhook_deliveries(id, webhook_id, event, status, attempts, response, created_at)  -- retention job
audit_log(id bigint identity, prev_hash, row_hash, actor_type, actor_id, action,
          resource_type, resource_id, diff jsonb, ip, created_at)     -- partitioned by month

-- v1.x
forms(id, name, schema jsonb, config jsonb)
form_submissions(id, form_id, data jsonb, created_at)                 -- retention job
plugins(id, name, version, manifest jsonb, enabled)
```

**Notes**
- Public rendering reads only the published revision; the editor reads only the working copy
- `version` increments on every working-copy write; "unpublished changes" = `version <> published_version`
- Promoted generated columns for filtered fields instead of broad GIN indexes on JSONB (GIN slows every write)
- Soft delete via `deleted_at` on entries and media; uniqueness constraints are partial on `deleted_at IS NULL`
- UUIDv7 IDs sort by creation time and are not enumerable, but do reveal creation time. Public URLs use slugs, not IDs

---

## 7. Security Specification

### 7.1 Threat model highlights
Account takeover, privilege escalation/IDOR, stored XSS via rich text, blocks or embeds, malicious uploads (including decompression bombs), SSRF (media import, webhooks, oEmbed), path traversal, injection, CSRF, cache poisoning via unkeyed headers, denial of service via expensive endpoints (search, transforms, query blocks, regex redirects, 404 floods), client IP spoofing, supply chain compromise, malicious themes, theft of backups, email header injection, data exfiltration via the API. [v1.x: malicious plugins.]

### 7.2 Controls
| Area | Control |
|---|---|
| Injection | sqlc only, no string-built SQL; lint rule banning `fmt.Sprintf` in query code |
| XSS | Auto-escaping templates; rich text validated as ProseMirror JSON against a node/mark allow-list and rendered by our own escaping renderer; `bluemonday` only for the raw-HTML field; embeds in sandboxed iframes; strict CSP with nonces; no inline event handlers |
| CSRF | Go `http.CrossOriginProtection` (Sec-Fetch-Site / Origin) as a global layer, plus per-session tokens on all mutating routes; `SameSite=Strict` admin cookies |
| AuthZ | Central `actor.Can` in services; per-endpoint authorisation tests; deny by default |
| Sessions | `__Host-` cookies; Secure, HttpOnly, SameSite; rotation; idle and absolute timeouts; sudo mode for sensitive actions |
| Passwords | argon2id, tuned parameters, breach check, rate-limited |
| Uploads | Content sniffing, allow-lists, pixel limits, out-of-process processing, separate media origin by default, SVG sanitisation, size limits, EXIF stripping |
| SSRF | Outbound HTTP via `internal/netx`: block private/link-local/metadata IP ranges (checked after DNS resolution, at connect time, to defeat rebinding), redirect limits, timeouts, response size caps |
| Path traversal | Storage keys are generated IDs; render paths are derived from validated slugs and checked to stay inside the render root |
| Client IP | `trusted_proxies` (default loopback only); `X-Forwarded-For` ignored from anyone else |
| Host header | Only configured hosts served; others get 421 |
| Headers | CSP, HSTS (preload-ready), `X-Content-Type-Options`, `Referrer-Policy`, `Permissions-Policy`, `frame-ancestors`, `Cross-Origin-Opener-Policy` |
| Secrets | Env / systemd credentials / secret manager only; stored credentials envelope-encrypted (7.3) |
| Database | Two roles: `cms_migrator` (owns schema, used only by `cms migrate`) and `cms_app` (DML only; `INSERT`/`SELECT` only on `audit_log`; no DDL); per-role `statement_timeout` |
| Rate limiting | Login, password reset, magic link, API, search, transforms, 404 logging (all bounded in-memory, single node) |
| Expensive endpoints | Caps on query block limit, API page size and include depth, search result count, regex redirect count, transform presets |
| Themes | `html/template` only; restricted func map; installed by deploy/CLI only; per-theme public CSP |
| Supply chain | `govulncheck`, Renovate, actions pinned by SHA, `go mod verify`, SBOM, releases signed with a private cosign key, signature verified at deploy |
| Processes | Run as unprivileged user, systemd hardening (`NoNewPrivileges`, `ProtectSystem=strict`, `PrivateTmp`, `ProtectHome`, `RestrictAddressFamilies`, `MemoryMax`), separate DB roles |
| Backups | Encrypted at rest; separate credentials; restore tested weekly |
| Audit | Append-only, hash-chained, optionally shipped off-box |
| Plugins [v1.x] | WASM sandbox, declared capabilities, memory page cap, context deadline, signed packages |

### 7.3 Key management
- **KEK** (key-encryption key, 256-bit) loaded at start from systemd `LoadCredential=`, a file with mode 0400, or a KMS. Never stored in the database or config file
- **DEKs** (data-encryption keys) generated per secret, encrypted with the KEK, stored alongside the ciphertext with a `key_id`
- AEAD: AES-256-GCM (or XChaCha20-Poly1305), with the row ID as associated data so ciphertexts cannot be swapped between rows
- Encrypted: TOTP seeds, passkey private data where applicable, webhook secrets, SMTP/S3/OIDC credentials, session signing key
- `cms keys rotate` creates a new KEK version and re-wraps all DEKs in a background job; old KEK retained until re-wrap completes; `cms keys status` reports progress
- Losing the KEK means losing encrypted data. `cms install` prints backup instructions and `cms doctor` checks that a KEK backup has been acknowledged

### 7.4 Security testing in CI
- `gosec`, `staticcheck`, `govulncheck` (source mode): the authoritative vulnerability gate, complementing the in-app self-audit in 4.20
- Fuzz tests for parsers (blocks JSON, ProseMirror JSON, rich-text renderer, redirect rules, upload sniffing, URL patterns, CSV import): 30 s per target on PRs, long runs nightly
- Authorisation matrix tests: for every service method, test each role plus anonymous
- CSP test: headless browser loads every admin screen and fails on any CSP violation
- Integration tests against a real Postgres (testcontainers), running as `cms_app` to prove least privilege
- SSRF test suite (private ranges, rebinding, redirects to internal hosts)
- DAST pass (ZAP baseline) against staging, nightly
- Dependency licence check (lightweight: flags AGPL/GPL dependencies in case any site is ever handed over to a client)
- External penetration test before v1.0; quarterly review of auth, uploads and the renderer after that

---

## 8. Performance and Caching

### 8.1 Hot path
- Public `static` pages are served by Caddy from pre-compressed files on disk (4.12). Go and Postgres are not involved
- Cache misses and `uncached` routes are rendered by Go; a miss on a `static` route writes the file for next time
- In-memory redirect and routing tables, rebuilt via events
- Variants and theme assets served by Caddy/CDN with immutable cache headers
- Targets (1 vCPU / 1 GB server): cached pages served by Caddy < 5 ms p95; cache-miss renders < 50 ms p95; admin pages < 300 ms p95; sustained 1k req/s on cached pages

### 8.2 Memory budget
| Process | Target | How |
|---|---|---|
| `cms serve` idle RSS | ≤ 40 MB | No eager caches; templates parsed once |
| `cms serve` under load | ≤ 128 MB | `GOMEMLIMIT=100MiB`, bounded LRUs with byte caps, request body limits, streaming uploads |
| `cms worker` | ≤ 64 MB | Queue concurrency 2–4 on small hosts |
| `vips` subprocess | ≤ 256 MB per job, hard-capped; 0 MB when idle | Transient systemd scope with `MemoryMax`, pixel limit, one job at a time on 1 GB hosts |
| Postgres (1 GB host) | ≤ 400 MB | `shared_buffers=128MB`, `work_mem=4MB`, `max_connections=30` |
| Core binary | ≤ 40 MB | `-trimpath -ldflags="-s -w"`, OTel behind a build tag |

Budgets are enforced in CI (9.2): a regression above 10% fails the build.

### 8.3 Efficiency rules
- **Bounded caches only:** anything keyed by user input (404 paths, transform URLs, search queries, block renders) uses an LRU with a size cap
- **Batched writes:** redirect hits, 404s, API token `last_used_at` and session touches are counted in memory and flushed every 60 s (session touch every 5 minutes)
- **No N+1:** menus, relationships, terms and query blocks load by ID set in one query (`WHERE id = ANY($1)`). Render tests assert a maximum query count per page
- **Pools sized for the host:** web pool max 8–10, job pool max 4 on small hosts, so jobs cannot starve requests. `statement_timeout` 5 s for web, 60 s for jobs
- **Prepared statements** via pgx statement cache; query timeouts on every call
- **Streaming everywhere:** uploads, exports, backups and imports stream; nothing reads whole files into memory
- **Precompression at write time:** brotli and gzip once per render, not per request
- **Admin assets:** esbuild bundles with code splitting; Tiptap only where needed
- **Load testing:** k6 scripts and Go benchmarks in the repo (matcher, renderer, richtext), run in CI

---

## 9. CI/CD, Deployment and Operations

### 9.1 Principles
- Every commit on `main` is releasable. CI proves the build is safe, the new schema works with the previous binary, and the release is signed
- Deploys never restart in place. A new version starts next to the old one and takes traffic only after it proves healthy
- Rollback is a binary switch (about one second), never a down migration

### 9.2 Continuous integration (every pull request)
1. **Reproducible toolchain:** Go pinned by the `toolchain` directive; all GitHub Actions pinned by commit SHA
2. **Generated code is current:** `sqlc generate`, `go mod tidy` and the asset build, then `git diff --exit-code`
3. **Static checks:** `go vet`, `staticcheck`, `golangci-lint` (including `gosec`), `govulncheck` (source mode), licence check, `go mod verify`
4. **Migration safety:**
   - `squawk` lints every new migration for locking operations (non-concurrent index creation, table rewrites, `NOT NULL` without default, etc.)
   - Every migration is tagged `-- +cms expand` or `-- +cms contract`
   - A `contract` migration may only remove objects the previous release no longer references; it ships at least one release after the code stopped using them
5. **N-1 compatibility test:** apply this branch's migrations, then run the previous release's integration suite against the new schema. Passing proves binary-only rollback is safe
6. **Tests:** unit, authorisation matrix, integration (testcontainers Postgres, as `cms_app`), golden `--json` CLI tests, short fuzz runs, Playwright + axe-core on admin screens, CSP violation check
7. **Budgets:** k6 smoke run against the built binary (p95 and RSS from `/metrics`), binary size check; >10% regression fails
8. **API compatibility:** OpenAPI diff against the previous release; breaking changes in `/api/v1` fail
9. **Schema check:** `cms schema diff` against the files in the repo

### 9.3 Release (on merge to `main`)
- **release-please** (Conventional Commits) maintains a release PR with the changelog and version bump; merging it tags the release
- **GoReleaser:** `CGO_ENABLED=0 -trimpath`, `SOURCE_DATE_EPOCH` for reproducible builds, linux/amd64 and linux/arm64; version embedded via `-ldflags`
- Artefacts: static binary, checksums, distroless container image (`gcr.io/distroless/static`, non-root), deploy bundle (`deploy/`)
- **SBOM** (syft) attached to each release
- **Signing:** cosign with a private key pair (key stored as a GitHub Actions secret, public key baked into the deploy bundle and servers), with transparency-log upload disabled. Keyless signing is not used because it would publish the private repo's identity to the public Rekor log, and GitHub artifact attestations for private repos need Enterprise Cloud
- **Distribution:** private GitHub Releases and a private GHCR image. Servers pull with a read-only fine-grained token scoped to this one repo
- Upgrade notes generated from migration tags; any `contract` migration is called out

### 9.4 Continuous deployment
Core releases move through three rings:

| Ring | Channel | Servers | Promoted when |
|---|---|---|---|
| 1 | `edge` | Staging (runs a fixture site) | Every release, automatically |
| 2 | `canary` | Your own website | 24 h on staging with `cms doctor`, ZAP baseline and Playwright smoke green, no alerts |
| 3 | `stable` | Client sites | 72 h on your website with no alerts |

- A scheduled CI job does the promotions. Servers pick up their channel on the next timer run (9.6)
- Manual promotion (security fixes, skipping the soak): a `workflow_dispatch` job, or `gh release edit` to move the release to a channel
- Site bundles skip the rings: they are validated in the site repo's CI and deploy straight to that one site, with the same automatic rollback
- One deploy at a time per server: a lock file on the host. Promotion jobs use a GitHub `concurrency` group

### 9.5 Deploy flow (single server, zero downtime)

Two instances of `cms serve` (`cms@blue` on 127.0.0.1:8081, `cms@green` on 127.0.0.1:8082) via a systemd template unit. Caddy proxies to whichever is active.

1. Download the release bundle from the private release (read-only token). **Verify the cosign signature against the pinned public key, and the checksum**; abort on failure
2. Unpack to `/opt/cms/releases/<version>/`
3. **Pre-migration snapshot** (`pg_dump -Fc`, or confirm the WAL archive is current within 5 minutes)
4. `cms migrate up` as `cms_migrator` (expand-only, so the running version keeps working)
5. Point the idle colour's symlink at the new release and start it
6. Wait for `/readyz`, then run `cms doctor --json` against it; abort and stop it on failure
7. Switch Caddy's upstream to the new colour (rewrite the imported upstream snippet and reload Caddy via its admin API; reloads are graceful)
8. **Watch for 5 minutes:** error rate and p95 from `/metrics`. On regression, switch Caddy back to the old colour (still running) and stop the new one
9. Stop the old colour with graceful shutdown (`http.Server.Shutdown`, 30 s drain)
10. Worker: start the new `cms worker`, then stop the old one with River graceful stop (30 s). Jobs are idempotent, so overlap is safe

The rendered-page directory is shared by both colours, so the cache survives deploys. A new theme version re-renders in the background while old files keep serving.

**Caddyfile sketch**
```
example.com {
	root * /var/lib/cms/public/example.com

	@dynamic path /admin/* /api/* /preview/* /search* /_cms/*
	handle @dynamic {
		import /etc/caddy/cms-upstream.caddy
	}

	@rendered file {path}/index.html
	handle @rendered {
		rewrite * {path}/index.html
		file_server {
			precompressed br gzip
		}
	}

	handle {
		import /etc/caddy/cms-upstream.caddy
	}
}
# /etc/caddy/cms-upstream.caddy (rewritten by the deploy script):
# reverse_proxy 127.0.0.1:8081 { health_uri /readyz }
```

### 9.6 Staying up to date
- **Renovate:** Go modules, GitHub Actions, base images and the Go toolchain; grouped weekly; patch and minor updates auto-merge when CI is green; security updates immediately
- **Nightly jobs** on `main` and on the latest release tag: `govulncheck`, Trivy image scan, long fuzz runs, ZAP baseline. A new advisory affecting a released version opens an issue and a patch-release PR automatically
- **Pull-based deploys:** each server runs `cms upgrade` on a systemd timer (every 10 minutes). It checks the private release feed and runs exactly the deploy flow in 9.5 (verify signature, snapshot, migrate, blue/green, doctor, watch, auto-rollback). CI never holds SSH keys or credentials to production; servers hold only a read-only token. Production servers follow the `stable` channel, which a release joins after its staging soak (9.4)
- **CI minutes:** private repos have a limited GitHub Actions allowance. PR checks stay fast (target < 10 minutes); fuzzing, ZAP and k6 long runs are nightly only, and nightly jobs skip when `main` has not changed
- Release notes and a documented upgrade path per version; upgrades must work from any of the previous three minor versions

### 9.7 Runtime
- `cms@.service` (web, blue/green instances) and `cms-worker.service` (jobs), unprivileged user, hardened unit files with `MemoryMax`
- Image processing: `vips` subprocesses spawned by the worker in transient systemd scopes (`MemoryMax=256M`); nothing resident
- Caddy for TLS, static file serving and upstream switching
- Config via env vars or `/etc/cms/config.yaml`; secrets via systemd credentials, outside the release dir
- Postgres via `DATABASE_URL` (runtime role) and `DATABASE_MIGRATOR_URL` (migrator role); bound to `127.0.0.1` or managed
- Provisioning: one cloud-init script for new servers (installs Postgres, Caddy, `libvips-tools`, the blue/green units and the upgrade timer). No public installers
- Local development: `cms dev` (embedded Postgres); `docker-compose.yml` for MinIO and Mailpit

### 9.8 Observability
- Health endpoints: `/healthz` (liveness), `/readyz` (DB + queue + render dir writable)
- Prometheus metrics: request latency, render cache hit ratio (from Caddy and Go), re-render queue depth, job queue depth, DB pool stats, Go memory and GC stats
- Structured logs to journald; optional OTLP traces (build tag)
- Error reporting hook (Sentry-compatible)
- Alerts: deploy watch failures, backup/WAL staleness, restore drill failure, critical security findings. All sites alert to the owner by email and webhook
- Fleet view [v1.x]: each instance sends a signed daily heartbeat (core version, site bundle version, health, open findings, last backup) to an endpoint on your own site, which lists every client site on one page

### 9.9 Upgrades and maintenance
- Migrations are forward-only and expand/contract style, enforced in CI (9.2)
- Schema changes to content types go through files → `cms schema diff` → `apply`
- Multi-site [later]: one binary, many configs and databases

---

## 10. Testing Strategy

| Level | Approach |
|---|---|
| Unit | Services with fake stores; field/block validators; richtext renderer; redirect matcher; permission logic; counters |
| Integration | Real Postgres via testcontainers, as `cms_app`; migrations up; sqlc queries; working copy / publish flows; optimistic locking |
| Compatibility | Previous release's integration suite against the new schema (N-1) |
| HTTP | `httptest` end-to-end for admin and public handlers, including CSRF, cross-origin protection and CSP headers |
| Rendering | Golden HTML for the default theme; maximum query count per page; dependency tracking invalidates the right files |
| CLI | Golden-file tests of `--json` output |
| Security | Authorisation matrix, fuzzing, upload malware and decompression-bomb samples, SSRF suite, CSP violation check, ZAP baseline |
| Performance | k6 load tests with latency and memory budgets; benchmarks on matcher, renderer and richtext |
| Accessibility | axe-core in CI against admin screens and the default theme |
| Operations | Weekly restore drill; deploy script tested on staging including forced rollback |

Coverage target: 80%+ on services, with 100% of service methods covered by the authorisation matrix.

---

## 11. Roadmap

One developer (the owner). Each phase ends at a gate it must pass before the next phase starts.

| Phase | Weeks | Scope | Exit gate |
|---|---|---|---|
| 0. Foundations and pipeline | 1–3 | Repo, config, keys, migrations with both DB roles, actor and fixed roles, audit log, cobra root, `doctor`, `install`, `dev`; full CI (9.2), release pipeline (9.3), blue/green deploy on staging (9.5), Renovate, nightly jobs, restore drill | A signed `v0.0.1` deploys blue/green to staging and rolls back on a forced failure |
| 1. Core content | 4–7 | Auth (password, passkeys, TOTP, sudo mode), three roles, content types (file-first), site bundle format, base theme, `cms new` with the `blank` starter, `cms make`, entries with working copy and optimistic locking, revisions, taxonomies, basic admin UI, CLI, default theme | Editor cannot lose work: autosave, conflict and publish flows pass E2E tests |
| 2. Fields, blocks, rendering | 8–11 | Field groups and types, options pages, block definitions and editor, reusable blocks, query block, live preview, richtext renderer, render-to-disk with dependency tracking | Memory and latency budgets (8.1, 8.2) met in CI |
| 3. Media, redirects, SEO | 12–14 | Media library, storage drivers, `vips` subprocess pipeline, usage tracking, redirects with auto-creation and batched 404 log, menus, SEO module, sitemaps, feeds | Upload fuzzing, decompression-bomb and SSRF suites green |
| 4. Platform | 15–16 | Search, read-only REST API, webhooks, backups with PITR, security audit module, `cms upgrade` with site bundles and release rings | Restore drill passes weekly; a site bundle change deploys and rolls back cleanly |
| 5. Launch on your own site | 17–18 | Move your own website onto it, `brochure` and `blog` starters, accessibility pass, external security review of auth, uploads and the renderer, runbook | Your site runs on it for 2 weeks with no incidents → **v1.0**. Client sites only after this |
| v1.x | 19+ | WordPress importer (when needed), forms, write API, fleet view, OIDC SSO, multilingual UI, mega-menus, social images. Ecommerce (shop-lite) later. WASM plugins only if a real need appears | Per feature |

---

## 12. Decisions

All open questions are decided.

| # | Question | Decision | Status |
|---|---|---|---|
| 1 | Single-site or tenant-aware? | One instance per site (own server, database, users). No `site_id` columns. Avoid global singletons so multi-site can be added later | Decided |
| 2 | Pure Go or libvips images? | Core stays pure Go. The worker runs the `vips` CLI as a resource-limited subprocess per job: no cgo, no Docker, no resident memory. imgproxy is an optional driver | Decided |
| 3 | UUIDv7 or bigint? | UUIDv7 generated in Go. Target Postgres 18, support 16+. `audit_log` uses bigint identity for the hash chain | Decided |
| 4 | Tiptap or custom editor? | Tiptap inside rich-text fields only; blocks use a vanilla JS editor with SortableJS | Decided |
| 5 | File-first or UI-editable types? | File-first in git; admin UI edits in dev and is read-only in production; CI runs `cms schema diff` | Decided |
| 6 | Licence and distribution? | Private, single-owner project. No licence, no public distribution. Private repo, private releases, key-based signing, pull-based deploys | Decided |
| 7 | WordPress import fidelity? | Posts, pages, terms, media, authors, redirects complete; core Gutenberg blocks mapped; shortcodes and unknown blocks kept as raw-HTML blocks with a report | Decided |
| 8 | Embed default theme? | A base theme is embedded; site themes extend it and override only what they need. Starters are embedded too | Decided |
| 9 | Vulnerability lookup default? | On by default (public data only), one setting to disable; offline import kept. The release check reads the private release feed, not a public endpoint | Decided |
| 10 | Who uses the admin? | You (Owner on every site) and client Admins, with optional client Editors. Three fixed roles, 2FA for everyone | Decided |
| 11 | How are client sites customised? | Per-site repos producing signed site bundles (theme, content types, fields, blocks); one core binary for all sites | Decided |
| 12 | How do core updates reach client sites? | Release rings: staging → your own site → client sites, promoted automatically after clean soaks | Decided |
| 13 | How are new sites started? | `cms new` with embedded starters (`brochure`, `blog`, `blank`); shared CI workflow; `cms make` generators | Decided |
| 14 | Ecommerce? | Later. Shop-lite with Stripe Checkout for small shops; Shopify for larger stores; no full built-in commerce | Decided |

---

## 13. Appendix: Reference Projects

- **PocketBase:** single-binary Go backend, admin UI, hooks
- **Caddy:** compile-time module system; also the reverse proxy and static server here
- **Hugo:** content and theme model, static export
- **Directus / Payload / Strapi:** schema-driven headless CMS design
- **ACF / Gutenberg:** field and block UX conventions
- **Craft CMS:** entry types, fields, sections and the draft/revision model, a close conceptual match

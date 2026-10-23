# cms

A private, fast, secure CMS in a single Go binary. One instance per site: your own website, plus one server per client if you ever need it. The full design is in [cms-tech-spec.md](cms-tech-spec.md), which is the source of truth.

- Public pages are rendered to pre-compressed files (Brotli and gzip) and served straight from disk by Caddy.
- The admin is server-rendered with a small vanilla JS editor (Tiptap rich text, block builder, media library, menus, redirects, SEO).
- Security defaults: mandatory 2FA (TOTP, plus optional passkeys), strict CSP, separate DB roles, envelope encryption, and a hash-chained audit log.
- Releases are signed and deployed blue/green with automatic rollback.

## Make a blog in two minutes

You only need the `cms` binary. Docker and a local Postgres are not needed, because `cms dev` runs an embedded Postgres.

```sh
cms new my-blog --starter blog     # brochure, blog or blank
cd my-blog
cms dev                            # http://localhost:8080, admin at /admin
```

Sign in as `dev@localhost`. The password is in `.cms/dev/credentials`, and 2FA is skipped until you enrol it. `cms dev` loads the sample content from `fixtures/` and reloads when you edit the theme, blocks or content types.

What you edit in a site repo:

| Path | What it is |
|---|---|
| `site.yaml` | Name, URL, permalinks, image presets |
| `content-types/*.yaml` | Post types and their fields (`cms make type case-study`) |
| `taxonomies/*.yaml` | Categories, tags |
| `blocks/<name>/` | Custom blocks: YAML schema plus template (`cms make block pricing`) |
| `theme/` | Templates, partials, CSS. Anything not overridden comes from the built-in base theme |
| `fixtures/` | Sample content for `cms dev` and CI |

Run `cms check` before you push. It validates the schema, templates and fixtures, then renders every page. The site's CI runs the same command.

## Day to day

1. **Your laptop:** edit the theme and content types in the site repo, check them with `cms dev`, then push.
2. **Site repo CI** (`.github/workflows/site.yml`, created by `cms new`): runs `cms check`, then builds and signs a site bundle.
3. **The server** pulls new core releases and site bundles on its channel (`edge`, `canary` or `stable`), then deploys them blue/green. It rolls back if `/readyz` or the error rate goes bad during the watch period.
4. **Content** is edited in the admin on the live site, not in git.

## Working on the core (this repo)

You need Go 1.26 and Node 22 (Node is only used for the admin assets and the E2E tests).

```sh
make dev          # build, then run the blog starter from .dev-site/
make test         # Go tests against embedded Postgres (race detector on)
make e2e          # Playwright: every admin and public page, CSP and axe checks, passkey flow
make generate     # sqlc + admin asset bundle (web/dist is committed)
make lint         # vet, golangci-lint, govulncheck
```

| Path | What it is |
|---|---|
| `cmd/cms` | Entry point |
| `internal/` | Services (content, auth, media, render, redirects…), HTTP adapters (`http/admin`, `http/api`, `http/public`), CLI |
| `migrations/` | goose SQL, tagged `-- +cms expand` / `contract`. Never edit a migration that has shipped; add a new one |
| `internal/db/queries` | sqlc queries; generated code in `internal/db/dbq` |
| `themes/base` | Base theme every site extends |
| `starters/` | `blog`, `brochure`, `blank` (embedded, so rebuild after editing) |
| `web/` | Admin CSS and JS source; `npm run build` writes `web/dist` |
| `deploy/` | systemd units, Caddyfile, blue/green `deploy.sh`, cloud-init |

## First-time GitHub setup

1. The core lives in the **private** repo `Chrisgarlick/go-cms`.
2. Generate a signing key with `cosign generate-key-pair`. Add `COSIGN_PRIVATE_KEY` and `COSIGN_PASSWORD` as repository secrets, and keep `cosign.pub` for your servers.
3. Add a fine-grained, read-only token for the private repo as `CMS_RELEASES_TOKEN` (site repos use it to download the core). Optionally set the `STAGING_URL` and `CANARY_URL` variables for the post-release smoke checks.
4. Merge to `main`. release-please opens a release PR, and merging that PR tags a release. GoReleaser then builds, signs and publishes the binaries with an SBOM. Releases start on `edge`, and `promote.yml` moves them to `canary`, then `stable`.

Release builds record the repo name, so `cms new` writes site workflows that call `Chrisgarlick/go-cms/.github/workflows/site.yml@v1`. A local `make build` takes it from `git remote origin`.

## Deploying a site

Fill in the variables at the top of `deploy/cloud-init.yaml`, paste in `cosign.pub`, and use it as the user data for a fresh Ubuntu 24.04 server. It installs Postgres, vips and Caddy natively (no Docker), then runs `cms install`, which:

- creates the database roles
- writes the config and key file
- migrates and applies the site bundle
- emails the owner an invite

After that, timers handle upgrades (`cms upgrade`) and nightly backups (`cms backup`). Run `cms doctor` on the server at any time for a health check, and `cms security audit` for the self-audit.

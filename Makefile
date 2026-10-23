# Developer entry points. CI runs the same targets (see .github/workflows/ci.yml).
GO       ?= go
REPO     ?= $(shell git remote get-url origin 2>/dev/null | sed -E 's,(git@github.com:|https://github.com/),,; s,\.git$$,,')
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X cms/internal/version.Version=$(VERSION) -X cms/internal/version.Commit=$(shell git rev-parse HEAD 2>/dev/null) -X cms/internal/version.Date=$(shell date -u +%Y-%m-%dT%H:%M:%SZ) $(if $(REPO),-X cms/internal/version.Repo=$(REPO))

.PHONY: build dev test test-short lint generate assets check-generated fuzz e2e starters budget clean

build: ## Static, stripped binary
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/cms ./cmd/cms

dev: build ## Run the blog starter locally
	@test -d .dev-site || ./bin/cms new .dev-site --starter blog --git=false
	cd .dev-site && ../bin/cms dev

test: ## Unit + integration tests (embedded Postgres)
	$(GO) test -race -count=1 ./...

test-short: ## Unit tests only
	$(GO) test -short ./...

lint:
	$(GO) vet ./...
	golangci-lint run ./...
	govulncheck ./...

generate: ## sqlc queries and admin assets
	$(GO) tool sqlc generate
	cd web && npm ci --no-audit --no-fund && npm run build

assets:
	cd web && npm run build

check-generated: generate ## Fail if generated code is stale
	$(GO) mod tidy
	git diff --exit-code -- internal/db/dbq web/dist go.mod go.sum

fuzz: ## Short fuzz runs (nightly runs longer)
	$(GO) test ./internal/richtext -run '^$$' -fuzz FuzzRender -fuzztime $${FUZZTIME:-30s}

starters: build ## Every starter must generate and validate
	@set -e; for s in blog brochure blank; do \
	  rm -rf /tmp/cms-starter-$$s; ./bin/cms new /tmp/cms-starter-$$s --starter $$s --git=false -q; \
	  ./bin/cms theme validate /tmp/cms-starter-$$s; done

e2e: build ## Browser tests (Playwright) against a dev site
	cd e2e && npm ci --no-audit --no-fund && npx playwright test

budget: build ## Memory and latency budgets (needs k6)
	./scripts/budget.sh

clean:
	rm -rf bin dist .dev-site

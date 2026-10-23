#!/usr/bin/env bash
# N-1 compatibility (spec 9.2): run the previous release's integration tests
# against this branch's migrations. If they pass, rolling back to the previous
# binary on the new schema is safe, so deploys never need a down migration.
set -euo pipefail
prev=$(git describe --tags --abbrev=0 HEAD^ 2>/dev/null || true)
if [[ -z $prev ]]; then
  echo "No previous release tag: skipping N-1 check."
  exit 0
fi
dir=$(mktemp -d)
trap 'git worktree remove --force "$dir" >/dev/null 2>&1 || true' EXIT
git worktree add --detach "$dir" "$prev" >/dev/null
# Contract migrations are allowed to break N-1 only if they were announced a
# release earlier; they are tagged and listed in the release notes.
cp migrations/*.sql "$dir/migrations/"
echo "Running $prev integration tests against $(git rev-parse --short HEAD)'s schema…"
(cd "$dir" && go test -count=1 ./internal/content/... ./internal/render/... ./internal/auth/... ./internal/http/... ./internal/audit/...)
echo "OK: $prev works on the new schema; binary-only rollback is safe."

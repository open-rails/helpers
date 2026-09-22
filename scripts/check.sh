#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
export GOWORK=off
: "${RIVER_TEST_DATABASE_URL:?Set RIVER_TEST_DATABASE_URL to an owned PostgreSQL test database}"

mapfile -t modules < <(git ls-files --cached --others --exclude-standard -- go.mod '**/go.mod')
[[ "${#modules[@]}" == 1 && "${modules[0]}" == go.mod ]] || { echo "helpers requires exactly one root go.mod" >&2; exit 1; }
[[ -z "$(git ls-files --cached --others --exclude-standard -- go.work '**/go.work')" ]] || { echo "helpers must resolve without a workspace" >&2; exit 1; }
[[ -z "$(git ls-files -z '*.go' | xargs -0 gofmt -l)" ]] || { echo "run gofmt on Go source" >&2; exit 1; }
go mod tidy -diff
api_deps="$(go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./api)"
while IFS= read -r dep; do
  [[ -z "$dep" || "$dep" == github.com/open-rails/helpers/api ]] || { echo "API core imports non-standard package: $dep" >&2; exit 1; }
done <<< "$api_deps"
if go list -m -f '{{.Path}}' all | grep -Eq '^github.com/open-rails/(authkit|openrails|migratekit|apikit|riverkit)($|/)'; then
  echo "helpers must not depend on applications or retired standalone helper modules" >&2
  exit 1
fi
go vet ./...
go test -race -count=1 ./...
(cd api/compat/spa && node probe.mjs)
go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...

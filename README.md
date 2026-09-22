# helpers

Shared Go helpers in one module: `github.com/open-rails/helpers`. Requires Go 1.26.6 or newer. MigrateKit remains a separate module.

| Import | Purpose | Runtime dependencies |
| --- | --- | --- |
| `github.com/open-rails/helpers/api` | HTTP errors, responses, safe metadata and pagination | Standard library |
| `github.com/open-rails/helpers/api/gin` | Gin writers, query binding and locale helpers | API helpers and Gin |
| `github.com/open-rails/helpers/river` | Compose library jobs into one host-owned River client | River and pgx |

Import only the packages an application needs. The API core does not import Gin or River. Go builds the imported packages and their dependencies; all packages share this module's version and Go/toolchain requirements.

## API helpers

The core provides typed errors, stable type/code fields, optional request IDs, bounded/sanitized metadata, JSON writers and generic list/message/deletion responses. The Gin adapter delegates to those writers and adds pagination binding and locale middleware. It does not add a router or authentication system. Existing response bytes are retained, including omitted codes/discriminators and preserved request IDs.

## River composition

`river.New` combines library contributions, validates registration and binds their producers to the same ordinary River client and actual host pool. The host starts/stops that client and retains ownership of the pool. These helpers do not add a scheduler, migrate tables or own billing logic.

When importing both the helpers and upstream River, use a descriptive alias such as `riverhelpers` for `github.com/open-rails/helpers/river`.

## Migration

Replace the previous module imports with the corresponding packages in this module:

| Previous import | Shared import | Package name |
| --- | --- | --- |
| `github.com/open-rails/apikit` | `github.com/open-rails/helpers/api` | `api` |
| `github.com/open-rails/apikit/adapters/gin` | `github.com/open-rails/helpers/api/gin` | `ginapi` |
| `github.com/open-rails/riverkit` | `github.com/open-rails/helpers/river` | `river` |

No forwarding packages are provided at the old import paths. Libraries and hosts that exchange River contribution types must use the shared package paths; update those libraries before updating their host composers. MigrateKit imports do not change.

The API helpers preserve APIKit's response contract. Replacing older `doujins-org/ginapi` writers can change envelope semantics and requires application-level compatibility checks. See the [error envelope compatibility notes](api/compat/DECISION-object-discriminator.md) and [frozen HTTP/parser fixtures](api/compat/README.md) for the recorded differences.

## Validation

Use one entry point locally and in CI:

```sh
RIVER_TEST_DATABASE_URL='postgres://.../helpers_test?sslmode=disable' bash scripts/check.sh
```

It checks formatting, the single-module/package boundaries, vet/race tests, actual PostgreSQL River composition, the frozen frontend-parser fixtures and reachable vulnerabilities. For a quick API-only check, run `go test ./api/...`.

`api/compat` contains frozen wire/parser evidence, not reverse dependencies on applications. Parser origins and test adaptations are documented in [SPA parser provenance](api/compat/spa/PROVENANCE.md).

## License

All packages are covered by the root [MIT license](LICENSE), which retains the copyrights of Shinobi Online LLC and Paul Fidika.

# helpers

Shared Go helpers in one module: `github.com/open-rails/helpers`. Requires Go 1.26.6 or newer. MigrateKit remains a separate module.

| Import | Purpose | Runtime dependencies |
| --- | --- | --- |
| `github.com/open-rails/helpers/api` | HTTP errors, responses, safe metadata and pagination | Standard library |
| `github.com/open-rails/helpers/auth` | Request identity and optional scoped permission capability | Standard library |
| `github.com/open-rails/helpers/api/gin` | Gin writers, query binding and locale helpers | API helpers and Gin |
| `github.com/open-rails/helpers/river` | Initialize River tables and compose one host-owned client | River and pgx |
| `github.com/open-rails/helpers/deps` | Dependency supervision, Redis client, fallback switch, probe/status/metrics handlers | go-redis |

Import only the packages an application needs. The API core does not import Gin or River. Go builds the imported packages and their dependencies; all packages share this module's version and Go/toolchain requirements.

## API helpers

The core provides typed errors, stable type/code fields, optional request IDs, bounded/sanitized metadata, JSON writers and generic list/message/deletion responses. The Gin adapter delegates to those writers and adds pagination binding and locale middleware. It does not add a router or authentication system. Existing response bytes are retained, including omitted codes/discriminators and preserved request IDs.

## Authentication values

Consumers define their own `AuthenticateRequest(context.Context, *http.Request) (auth.Principal, error)` interface. A provider returns the verified identity for that request; an optional `auth.PermissionChecker` on the principal checks a host-resolved immutable scope and permission without verifying sender proof again. Identity-only providers implement only `Identity() auth.Identity`. Missing permission capability denies privileged access. Account mapping, routes, issuer configuration, verification and authorization policy stay with the provider or host.

## River composition

`river.ApplyMigrations(ctx, pool, schema)` creates the selected schema and applies River's migrations under a database/schema advisory lock. An empty schema means `public`. Schema creation happens inside the lock, and the lock uses a dedicated connection so a host pool with `MaxConns=1` cannot stall itself. The host retains its pool; call this explicit initializer before starting workers or accepting requests that enqueue jobs.

`river.New` combines library contributions, validates registration and binds their producers to the same ordinary River client and actual host pool. Pass the same schema in `riverqueue.Config.Schema`; empty also means `public`. Construction does not migrate or start workers. The host starts/stops that client and retains ownership of the pool. These helpers add neither a scheduler nor billing logic.

When importing both the helpers and upstream River, use a descriptive alias such as `riverhelpers` for `github.com/open-rails/helpers/river`.

## Dependency supervision

`deps.New()` supervises external dependencies. Each one is probed forever: every 10s (±10% jitter) while up, with full-jitter exponential backoff (500ms to 30s) while down, and it recovers after two consecutive successes. Optional dependencies go down on one failed probe, required ones after three (`DownAfter`, `ProbeTimeout` and `ProbeInterval`, the up-period for probes that cost something, tune one dependency). `Dependency.Report(err)` marks it down on the first data-path connectivity error; a probe already in flight cannot undo that. `OnUp`/`OnDown` hooks run in order on their own goroutine with the supervisor's context; while they lag, pending transitions coalesce to one down plus the latest state, so a hung hook never stalls probing. Adding a dependency under an existing name replaces it; `OnClose` releases a probe's resources when it is replaced or the supervisor stops. `Supervisor.Retry` waits for a required dependency at startup instead of exiting. `AddPostgres` (built on `PostgresProbe`) checks Postgres over one dedicated connection, closed on replace and stop, so a saturated application pool does not read as an outage; `PostgresUnavailable` classifies connection-level errors.

`deps.NewSwitch(dep, primary, newFallback)` serves the primary while the dependency is up and a local fallback otherwise. `deps.Call`/`deps.Do` retry a call once on the fallback when the primary is unreachable. Fallback state is replaced, never merged, on recovery. Use it only for state whose per-process scope is acceptable (caches, rate-limit windows); state other replicas must see belongs in Postgres.

`deps.NewRedis(RedisConfig)` returns a `redis.UniversalClient` without dialing: `master_name` with `sentinel_addrs` gives a Sentinel failover client (the Sentinel password defaults to `password`), otherwise one of `addrs` gives a plain client and several a cluster client. `AddRedis` registers it as optional; its probe writes a short-lived key, so a primary that refuses writes (`NOREPLICAS`, `READONLY`) is down too.

HTTP: `Gate()` is the application listener's handler and can bind before the application is built. It serves `/livez` (always 200, never checks dependencies) and `/readyz` (200 once `Open(app)` is called, 503 after `Drain()`), and returns 503 for everything else until `Open`. `OpsHandler()` adds `/statusz` (JSON per dependency) and `/metrics` (`app_ready`, `app_dependency_up{dependency,class}`, `app_dependency_transitions_total`, and `Counter`s) for an internal port.

## Migration

Replace the previous module imports with the corresponding packages in this module:

| Previous import | Shared import | Package name |
| --- | --- | --- |
| `github.com/open-rails/apikit` | `github.com/open-rails/helpers/api` | `api` |
| `github.com/open-rails/apikit/adapters/gin` | `github.com/open-rails/helpers/api/gin` | `ginapi` |
| `github.com/open-rails/riverkit` | `github.com/open-rails/helpers/river` | `river` |

No forwarding packages are provided at the old import paths. Libraries and hosts that exchange River contribution types must use the shared package paths; update those libraries before updating their host composers. MigrateKit imports do not change.

The API helpers preserve APIKit's response contract. Replacing older `doujins-org/ginapi` writers can change envelope semantics and requires application-level compatibility checks. See the [error envelope compatibility notes](api/compat/DECISION-object-discriminator.md) and [frozen HTTP/parser fixtures](api/compat/README.md) for the recorded differences.

## Injected-code scan

A reusable workflow scans a repository's tracked tree for the repo-injection worm (invisible padding, decode-and-eval, blockchain dead-drop C2, tampered build configs). Pin it by commit SHA:

```yaml
jobs:
  injection-scan:
    uses: open-rails/helpers/.github/workflows/injection-scan.yml@<helpers commit SHA>
```

Locally: `scripts/scan-injected-code.sh --root <repo>`. Rules, thresholds and exclusions are in [docs/injection-scan.md](docs/injection-scan.md).

## Validation

Use one entry point locally and in CI:

```sh
RIVER_TEST_DATABASE_URL='postgres://.../helpers_test?sslmode=disable' DEPS_TEST_REDIS_ADDR=127.0.0.1:6379 DEPS_TEST_SENTINEL_ADDR=127.0.0.1:26379 bash scripts/check.sh
```

It checks formatting, the single-module/package boundaries, vet/race tests, actual PostgreSQL River initialization/composition, Redis outage/recovery through a TCP cut, the frozen frontend-parser fixtures and reachable vulnerabilities. The PostgreSQL test login must be able to create temporary test databases; tests remove only their own databases/schemas. For a quick API-only check, run `go test ./api/...`.

`api/compat` contains frozen wire/parser evidence, not reverse dependencies on applications. Parser origins and test adaptations are documented in [SPA parser provenance](api/compat/spa/PROVENANCE.md).

## License

All packages are covered by the root [MIT license](LICENSE), which retains the copyrights of Shinobi Online LLC and Paul Fidika.

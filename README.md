# helpers

Shared Go helpers in one module: `github.com/open-rails/helpers`. Requires Go 1.26.6 or newer. MigrateKit remains a separate module.

| Import | Purpose | Runtime dependencies |
| --- | --- | --- |
| `github.com/open-rails/helpers/api` | HTTP errors, responses, safe metadata and pagination | Standard library |
| `github.com/open-rails/helpers/auth` | The host's auth as a library takes it: `Authenticator`, the verified request's identity (subject, invoker, credential) and optional permission and recent sign-in checks, the standard refusal (`Refuse`), and its conformance check (`authtest`) | Standard library |
| `github.com/open-rails/helpers/userinfo` | The host directory's user lookup (`Lookup`) and its conformance check (`userinfotest`) | Standard library |
| `github.com/open-rails/helpers/smtp` | Send email through any SMTP server (`smtptest`: an in-process server for end-to-end tests) | Standard library |
| `github.com/open-rails/helpers/api/gin` | Gin writers, query binding and locale helpers | API helpers and Gin |
| `github.com/open-rails/helpers/river` | Initialize River tables and compose one host-owned client | River and pgx |
| `github.com/open-rails/helpers/deps` | Dependency supervision, Redis client, fallback switch, probe/status/metrics handlers | go-redis |

Import only the packages an application needs. The API core does not import Gin or River. Go builds the imported packages and their dependencies; all packages share this module's version and Go/toolchain requirements.

## API helpers

The core provides typed errors, stable type/code fields, optional request IDs, bounded/sanitized metadata, JSON writers and generic list/message/deletion responses. The Gin adapter delegates to those writers and adds pagination binding and locale middleware. It does not add a router or authentication system. Existing response bytes are retained, including omitted codes/discriminators and preserved request IDs.

## Authentication

`auth.Authenticator` is a host's auth as a library (OpenRails) takes it to guard its own routes. The host only says who a request is; the library builds its gates, so the host writes no middleware.

`Authenticate(r)` verifies the request's credential live (a revoked sign-in, banned or deleted account is refused) and returns its `Verified`: `Identity()` is the native account whose authority it uses (`Subject` within `Issuer`, a `user` or an `application`), who actually acts (`Invoker`: the subject itself, or someone acting on its behalf) and how it was proven (`Credential`: a session, device key, API key, signed token or access token; never the subject). A credential's bounds are the provider's opaque `Credential.State`, which encoding drops, so an `Identity` built or decoded from data grants nothing. A consumer calls `Authenticate` once per request and keeps the `Verified` for that request alone.

Two optional capabilities of a `Verified` carry authority, and their absence denies:

- `Can(ctx, scope, permission)`: exactly that permission in exactly that `Scope` (`{Authority, ID}`, such as the group that administers a merchant), checked live.
- `CheckRecentSignIn(ctx)`: nil when a person signed in recently enough to move money or grant access; `ErrStepUpRequired` asks them to sign in again (an `*auth.Challenge` gives `MaxAge` and the provider's `Metadata`); `ErrForbidden` for a credential with no sign-in of its own.

A `Verified` may also implement `Bound` (`BoundScope()`): the one scope its credential acts in, such as a group's API key or a trusted issuer's token for its group, and the zero `Scope` for a person's own sign-in. A library serving one scope's resources admits there only a credential bound to that scope.

An `Authenticator` may also implement `PermissionCatalog` (`KnownPermission`), so a library refuses a misspelled permission when it mounts, and `Headers` (`AllowedHeaders()`, `ExposedHeaders()`): the request headers its credentials are sent in (`Authorization`, `DPoP`) and the response headers its challenges set (`WWW-Authenticate`, `DPoP-Nonce`). A library merges them into its CORS lists and never names a provider's headers itself.

Errors classify with `errors.Is`: `ErrUnauthenticated` (with `ErrExpired`, `ErrRevoked` or `ErrSenderProofRequired`), `ErrForbidden`, `ErrStepUpRequired`, `ErrUnavailable`. `auth.Refuse(r, err)` is the answer every consumer gives: 401 with an RFC 6750 challenge (`DPoP` scheme for a DPoP-bound request), 401 `insufficient_user_authentication` with `max_age` for a step-up (RFC 9470), 403 for `ErrForbidden`, and 503 for `ErrUnavailable` or anything unclassified. A `Challenge`'s `Header` (a DPoP nonce) is set on it. Consumers write their own body and never show provider error text.

A host checks its implementation in its own CI with `authtest.Check(t, authenticator, authtest.Cases{Scope, Permissions, Staff, User, ...})`: staff holding the permissions, a user holding none, optionally single-permission holders, a stale sign-in, an application, a credential `Bound` to the scope and a `Foreign` one bound to another, refused credentials and a `Revoke` hook. It fails on anything admitted that must be refused, any grant outside the exact scope and permission, an application reading as a person or as signed in, a bound scope misreported, and an error that does not classify as the contract says. With `Headers`, it also fails on a header Staff's credential is sent in that is not allowed, or a header a refusal's `Challenge` carries that is not exposed.

## User info

`userinfo.Lookup` is the host's directory read in process, so a library (OpenRails) reaches people without importing the host (AuthKit) or copying its data. `Get(ctx, ids)` returns each held id's `User` (`ID`, the subject the host's auth reports; `Email`, `Name`, `Username`); an id the directory does not hold is absent, not an error. `Search(ctx, query, limit)` returns up to `limit` users whose email, username or name contains `query` as literal text, ignoring case. Values are current at read time; the caller keeps no copy. A host checks its implementation in its own CI with `userinfotest.Check(t, lookup, userinfotest.Fixtures{...})`, giving people its directory holds, optionally ids it does not hold and a `Change` hook that proves reads are current.

## SMTP

`smtp.New(smtp.Config{Host, Port, Username, Password, From})` returns a `Sender`, so the email provider is configuration: SendGrid is `smtp.sendgrid.net:587` with username `apikey` and an API key with `mail.send` as the password; ZeptoMail, SES or a self-hosted server are another host. Port 465 is TLS from the first byte; any other port (0 is 587) upgrades with STARTTLS when offered, and must before credentials go to a host other than loopback. Authentication is PLAIN, else LOGIN. `From` is one RFC 5322 mailbox (`"Shop <noreply@shop.example>"`); a `Message` may carry its own. `Send(ctx, Message{To, Subject, Text, HTML})` opens one connection per message (safe for concurrent use), bounded by `ctx` and `Timeout` (30s), and writes a multipart/alternative message with `Date`, `Message-ID` and encoded headers; a refusal wraps the server's `*textproto.Error`. `CheckHealth` connects, negotiates TLS and authenticates without sending. No error carries the password. Applications name the settings `EMAIL_SMTP_HOST`, `EMAIL_SMTP_PORT`, `EMAIL_SMTP_USERNAME`, `EMAIL_SMTP_PASSWORD` and `EMAIL_SMTP_FROM`.

`smtptest.Start(t, smtptest.Options{...})` runs an SMTP server on 127.0.0.1 that captures and decodes what it receives (`Wait`, `Messages`), optionally requiring credentials, offering STARTTLS or speaking implicit TLS with a certificate `ClientTLS` trusts, refusing chosen recipients, and simulating an outage (`Outage`).

## River composition

`river.ApplyMigrations(ctx, pool, schema)` creates the selected schema and applies River's migrations under a database/schema advisory lock. An empty schema means `public`. Schema creation happens inside the lock, and the lock uses a dedicated connection so a host pool with `MaxConns=1` cannot stall itself. The host retains its pool; call this explicit initializer before starting workers or accepting requests that enqueue jobs.

`river.New` combines library contributions, validates registration and binds their producers to the same ordinary River client and actual host pool. Pass the same schema in `riverqueue.Config.Schema`; empty also means `public`. Construction does not migrate or start workers. The host starts/stops that client and retains ownership of the pool. These helpers add neither a scheduler nor billing logic.

When importing both the helpers and upstream River, use a descriptive alias such as `riverhelpers` for `github.com/open-rails/helpers/river`.

## Dependency supervision

`deps.New()` supervises external dependencies. Each one is probed forever: every 10s (±10% jitter) while up, with full-jitter exponential backoff (500ms to 30s) while down, and it recovers after two consecutive successes. Optional dependencies go down on one failed probe, required ones after three (`DownAfter`, `ProbeTimeout` and `ProbeInterval`, the up-period, and `DownInterval`, the least period while failing, for probes that cost something, tune one dependency). `Dependency.Report(err)` marks it down on the first data-path connectivity error; a probe already in flight cannot undo that. `OnUp`/`OnDown` hooks run in order on their own goroutine with the supervisor's context; while they lag, pending transitions coalesce to one down plus the latest state, so a hung hook never stalls probing. Adding a dependency under an existing name replaces it; `OnClose` releases a probe's resources when it is replaced or the supervisor stops. `Supervisor.Retry` waits for a required dependency at startup instead of exiting. `AddPostgres` (built on `PostgresProbe`) checks Postgres over one dedicated connection, closed on replace and stop, so a saturated application pool does not read as an outage; `PostgresUnavailable` classifies connection-level errors.

`deps.NewSwitch(dep, primary, newFallback)` serves the primary while the dependency is up and a local fallback otherwise. `deps.Call`/`deps.Do` retry a call once on the fallback when the primary is unreachable. Fallback state is replaced, never merged, on recovery. Use it only for state whose per-process scope is acceptable (caches, rate-limit windows); state other replicas must see belongs in Postgres.

`deps.NewRedis(RedisConfig)` returns a `redis.UniversalClient` without dialing: `master_name` with `sentinel_addrs` gives a Sentinel failover client (the Sentinel password defaults to `password`), otherwise one of `addrs` gives a plain client and several a cluster client. `AddRedis` registers it as optional; its probe writes a short-lived key, so a primary that refuses writes (`NOREPLICAS`, `READONLY`) is down too.

HTTP: `Gate()` is the application listener's handler and can bind before the application is built. It serves `/livez` (always 200, never checks dependencies) and `/readyz` (200 once `Open(app)` is called, 503 after `Drain()`), and returns 503 for everything else until `Open`. `OpsHandler()` adds `/statusz` (JSON per dependency) and `/metrics` (`app_ready`, `app_dependency_up{dependency,class}`, `app_dependency_transitions_total`, `Counter`s, and `GaugeFunc` gauges read at scrape time; names are validated and unique, label values escaped) for an internal port.

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

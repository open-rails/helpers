# Error envelope compatibility

These results describe the frozen writer and parser snapshots in this directory,
not current application releases. They guide compatibility checks when changing
an application's error writers.

The GinAPI snapshots wrap the error body in a top-level discriminator; the
AuthKit and OpenRails API snapshots do not:

```json
{"object":"error","error":{"type":"...","code":"...","message":"..."}}   ginapi
{"error":{"type":"...","code":"...","message":"..."}}                    authkit, openrails
```

The helpers `api` package uses the second envelope by default. `api.WithObject`
adds the discriminator explicitly.

## What the fixtures prove

`golden/` holds the captured HTTP status, headers and body for each writer snapshot;
`spa/golden/parsers.json` holds what the consumers' own parsers, run
unmodified, do with each one. 70 fixtures.

**1. On a real error status, the discriminator does not matter.** Both SPAs
take the same branch for both shapes at 4xx/5xx: they test
`typeof errorData.error === 'object'`, never `object`. Doujins never reads
`object` at all.

**2. It matters in exactly one place: an error body delivered with a 2xx
status.** `hentai0/frontend/src/services/api/api-service.ts` guards the success
path with `data.object === 'error'`, and that guard is the only reader:

| envelope | `api-service.ts` on a 200 |
|---|---|
| ginapi (has `object`) | throws — 14/14 fixtures |
| openrails `billingauth` v0.142.2 (has `object`) | throws — 2/2 |
| authkit (no `object`) | **resolves** — 11/11 |
| openrails `pkg/api` (no `object`) | **resolves** — 19/19 |
| apikit canonical (no `object`) | **resolves** — 17/17 |
| apikit + `WithObject` | throws — 5/5 |

"Resolves" means the error envelope is returned to the caller as the success
payload `T`. A handler that answers 200 with an error body would hand the UI
`{error:{…}}` where it expects its model. Removing `object` removes this parser
backstop; applications should return the appropriate error status.

**3. `hentai0/hooks/upload/useUploadVideo.ts` degrades rather than fails.** It
gates `errorData.object === 'error'` before reading the server's message. Without
the discriminator it falls back to the generic string:

| envelope | what the uploader is shown |
|---|---|
| with `object` | `"the artwork was refused"` |
| without | `"Upload failed - please try again"` |

Information loss on every upload error, not a wrong success.

**4. The OpenRails snapshots also differ in error type, code and request ID.**
The `billingauth` v0.142.2 fixture contains:

```json
{"error":{"message":"authentication required","type":"authentication_required"},"object":"error"}
```

The separately captured `golden/openrails-unreleased/` fixture contains:

```json
{"error":{"type":"authentication_error","code":"authentication_required","message":"authentication required","request_id":"req_01HZZ"}}
```

The fixture directory names identify historical captures, not supported package
versions or current deployment state.

## Migration guidance

The canonical envelope stays nested. Hosts whose consumers require the top-level
`object` field can add it with `api.WithObject`. Before replacing older GinAPI
writers, check both ordinary 4xx/5xx responses and the success-status error and
upload-message paths shown above.

Consumers can support both envelope shapes by inspecting the nested `error`
object and reading its message without requiring the top-level discriminator.
Qualify such changes against the application's current parsers; the frozen
snapshots here do not establish compatibility with later consumer versions.

## Reproduce

From the repository root:

```bash
GOWORK=off go test ./api/... -count=1
node api/compat/spa/probe.mjs
```

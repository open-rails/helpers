# Open decision: the `object` discriminator

**Status: OPEN — owner decision required. Not resolved in this lane.**

GinAPI wraps the error body in a top-level discriminator; AuthKit and OpenRails
do not:

```json
{"object":"error","error":{"type":"...","code":"...","message":"..."}}   ginapi
{"error":{"type":"...","code":"...","message":"..."}}                    authkit, openrails
```

apikit's canonical envelope is the second. `apikit.WithObject` renders the
first, and no writer applies it. This file is the evidence for choosing.

## What the fixtures prove

`golden/` holds real bytes off a real socket for every deployed writer;
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
`{error:{…}}` where it expects its model. Nothing in the fleet is known to do
that today — the guard is a backstop — but removing `object` removes the
backstop.

**3. `hentai0/hooks/upload/useUploadVideo.ts` degrades rather than fails.** It
gates `errorData.object === 'error'` before reading the server's message. Without
the discriminator it falls back to the generic string:

| envelope | what the uploader is shown |
|---|---|
| with `object` | `"the artwork was refused"` |
| without | `"Upload failed - please try again"` |

Information loss on every upload error, not a wrong success.

**4. OpenRails is already removing its own `object`.** v0.142.2 —
the version Doujins and Hentai0 pin — emits the discriminator from
`pkg/billingauth.WriteJSONError`, with the *code* in the `type` field and no
`request_id`:

```
{"error":{"message":"authentication required","type":"authentication_required"},"object":"error"}
```

OpenRails master (`e4d109e06`, PR #471, unreleased) replaced it with the nested
envelope — `golden/openrails-unreleased/`:

```
{"error":{"type":"authentication_error","code":"authentication_required","message":"authentication required","request_id":"req_01HZZ"}}
```

So "OpenRails emits `object`" is true of the deployed pin and false of its
direction of travel.

## The options

| option | what changes | blast radius |
|---|---|---|
| **A. Canonical stays nested (today's default).** Hosts that need the discriminator call `WithObject`. | nothing in apikit | hentai0's 200-with-error-body guard stops firing for any adopted service, and its upload errors lose the server's message, unless hentai0's parser is changed in the same PR |
| **B. Canonical carries `object`.** | `Envelope` gains the field; every AuthKit and OpenRails response gains it | additive for every parser measured here — no fixture changes outcome except the two above, which improve. Contradicts OpenRails' own #471 and adds a field Stripe's taxonomy does not have |
| **C. Nested canonical + fix the two hentai0 readers** (guard on `typeof data?.error === 'object' && data.error.type`, and read the message unconditionally). | apikit unchanged; two files in hentai0 | one coordinated hentai0 PR; both readers get *stronger* — they then work for all six envelope families instead of two |

**This lane recommends C** and did not apply it: it needs a hentai0 PR, and the
brief for this lane forbids one. B is the only option that requires no consumer
change at all, at the cost of a field OpenRails is actively deleting.

Whichever is chosen, it is a **major-version migration** for the field: record
it in the README's wire section, ship it in one version, and change every
consumer in the same coordinated PR.

## Reproduce

```bash
cd compat && GOWORK=off go test ./... -count=1   # 70 writer fixtures
cd spa && node probe.mjs                          # the parsers, unmodified
```

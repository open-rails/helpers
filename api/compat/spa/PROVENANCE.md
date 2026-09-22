# SPA parser provenance

Every file under `vendor/` is a byte-for-byte copy of a consumer's real
parser. Nothing here is rewritten: `probe.mjs` runs these files, over a real
socket, against the golden bodies in `../golden`.

These are frozen historical parser snapshots, not claims about current consumer checkouts. The executable probe retains their recorded behavior; adoption tests against current consumers belong in those repositories.

| vendored | repo | commit | source path | sha256 |
|---|---|---|---|---|
| `vendor/doujins/lib/http/client.ts` | doujins | `6437215b3ca5` | `frontend/src/lib/http/client.ts` | `03d9fa7b2d712c1b…` |
| `vendor/hentai0/services/api/api-service.ts` | hentai0 | `d21f1367bcc9` | `frontend/src/services/api/api-service.ts` | `dce148851e3da3ac…` |
| `vendor/hentai0/hooks/upload/useUploadVideo.ts` | hentai0 | `d21f1367bcc9` | `frontend/src/hooks/upload/useUploadVideo.ts` | `6cf0672e93ca3574…` |
| `vendor/hentai0/types/api/errors.ts` | hentai0 | `d21f1367bcc9` | `frontend/src/types/api/errors.ts` | `3b84f00480f38c10…` |

## What is stubbed, and why it is still faithful

`stubs/` stands in for leaf modules the error path never reaches — the auth
store, the auth SDK, route guards, language extraction — plus two mechanical
gaps between a browser bundler and node:

- `axios.ts`: `isAxiosError` mirrors axios' own check (`isAxiosError === true`)
  and `post` throws the error the probe planted, which is how
  `useUploadVideo`'s real `getUploadErrorMessage` is reached — it is
  module-private, so the probe drives `uploadQueue`, the path the UI takes.
- `hentai0-types-api.ts`: re-exports the REAL `types/api/errors.ts`
  (`getUserErrorMessage` and all) and adds the `APIError` binding, which
  `api-service.ts` imports as a value although it is an interface. A bundler
  erases it; node's type-stripper cannot.
- `react.ts`: `useState`, whose value no probed function reads.

`resolver.mjs` applies exactly one source transform to a vendored file —
`import.meta.env`, which Vite substitutes at build time — and counts every
rewrite so the transform cannot silently grow into a rewrite of parser logic.

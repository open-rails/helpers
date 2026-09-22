# Frozen HTTP and parser evidence

Ported from APIKit commit `9753ffd7933e9a3374caefac98a9b6a0098de763`.
The `apikit` fixture directory names retain their provenance. Canonical and
optional `WithObject` writer tests exercise this module over local HTTP sockets.
Capture preserves status, normalized headers and exact body bytes; it sorts
header names and excludes Date and Content-Length.

The other HTTP fixtures record historical AuthKit, OpenRails and GinAPI writers.
They remain frozen evidence, not assertions about today's downstream versions.
This package does not import those libraries or download their historical sources.

Run `go test ./api/...` from the repository root, then
`node api/compat/spa/probe.mjs` to exercise the preserved real parser snapshots
against all 70 fixtures. See `spa/PROVENANCE.md` for parser origins and the bounded
Node stubs/transform. Do not regenerate historical receipts during a package move.

# Retained events

Each product owns an independent scoped log and its authentication, payloads and
HTTP routing. A scoped `eventlog.Reader` is the consumer seam for both embedded
and HTTP clients. No global ordering, delivery workers, registrations or ACKs.

`Query` uses an opaque `After` cursor, exact `Types` (unknown values are valid),
and `Limit` (default 100, maximum 500). Filters are sorted/deduplicated; at most
32 input values, each 1–128 bytes after trimming. Keep the same filter when
resuming a cursor. `ErrInvalidCursor` means malformed/wrong-scope/wrong-filter;
`ErrExpiredCursor` means retained history cannot continue and current state must
be resynchronized. Product HTTP adapters map these to 400 and 410 respectively.
`OccurredAt` is domain time; `RecordedAt` is storage time.

`Consumer.Poll` loads the owner's checkpoint, reads a page and calls the owner's
commit function. The commit function applies idempotent effects and saves
`NextCursor` atomically, comparing the previous checkpoint or serializing writers.
Checkpoint keys include source, scope, subscription and canonical filter. Empty
pages must also commit: their cursor can advance across irrelevant events. If
effects are external, use event IDs for idempotency before saving the checkpoint.
The owner controls polling, retries and resynchronization; errors never reset a
cursor automatically.

`Log` centralizes paging and retention for existing storage through `Snapshot`.
Adapters must provide one repeatable-read snapshot and preserve their legacy
cursor codec. `SkipFloorBoundary` exists only for legacy logs that never stored
a pruned boundary ID; new logs must retain and validate it. Imported history must
preserve IDs, positions, epochs, boundaries and recording timestamps. If that is
impossible, explicitly return 410; never silently reset a cursor.

## PostgreSQL

`postgres.Migrate` installs versioned tables/functions in a host-created schema.
`postgres.New` binds a namespace/source and scope. Create the initial empty stream
with `EnsureScope` during scope provisioning (append also creates it).
`AppendTx` queues a fact inside the mutation transaction. SQL capture triggers
can insert the same columns into `eventlog_pending`, omitting `transaction_id`
and `ordinal`. Never force its deferred constraint immediate or change those
internal columns. The deferred trigger locks all touched scopes in sorted order,
then assigns positions and monotonic recording timestamps. Rolled-back mutations
leave no facts. Duplicate IDs abort the transaction; records are immutable.

The pending identity is only intra-transaction ordering, never a public cursor.
There is no ordering between independent scopes. Existing SQL journal capture,
restore suppression, scope checks and domain snapshot functions remain owned by
the product; adopting the shared queue must preserve them.

Retention defaults to 90 days and is enforced by reads even without pruning.
Call `Prune` periodically with a bounded batch (1–10,000 rows per scoped call).
Hosts own pool lifetime, schema permissions and scheduling. Test PostgreSQL with:

```sh
EVENTLOG_TEST_DATABASE_URL=postgres://... GOWORK=off go test ./eventlog/...
```

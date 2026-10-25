# Shared inbox

`helpers/notify` describes general notices and actionable items without importing an application, router, identity provider or database. `helpers/notify/postgres` stores them on the host's PostgreSQL pool. AuthKit, OpenRails, ContentKit and their hosts can use the same types; adopting this package does not require any of them to invent notifications.

General notices, such as a comment reply or a receipt, have an empty `Status`. Tracked actionable items, such as an operator finding, have `open`, `resolved` or `dismissed` status. **Read and archive state are independent:** reading a failed-payment notice does not fix the payment. `Transition` only records workflow state; the application authorizes and executes the real action. `TransitionTx` can commit that action and its resolution atomically. Revision checks reject stale actions.

`ActionRequested` preserves classification separately from workflow. A historical "payment failed" notice can record that action was requested, with an empty `Status`, without falsely claiming that the payment still fails today. Imports of such history retain the flag and can display "Action requested"; only an authoritative workflow should display "Open" or "Needs attention". Publishing a tracked `StatusOpen` item sets the flag automatically. Reading never changes either value.

The host controls which categories produce notices. Routine charge reminders/receipts can be off by default without suppressing failed-payment notices. Preferences and rendering are domain policy, so there is no hard-coded event taxonomy, email template or automatic sender in this package.

```go
// The host creates the schema and runs migrations before accepting requests.
err := postgres.Migrate(ctx, pool, "app")
inbox, err := postgres.New(pool, postgres.Config{
    Schema: "app", Scope: "project:example",
})
item, err := inbox.Publish(ctx, notify.Input{
    Recipient: notify.Recipient{Audience: notify.AudienceUser, ID: userID},
    Origin: notify.Origin{System: "contentkit", Scope: projectID, ID: replyID},
    Kind: "comment.reply", Data: replyReferences, CreatedAt: createdAt,
})
```

Use `PublishTx` inside a domain mutation's `pgx.Tx` when both share the database. A rolled-back comment creates no inbox item. This legacy insert-once interface retains the first payload and local state on retry. Deduplication includes the local scope, audience, recipient and source system/scope/ID, permitting deliberate fan-out while isolating independent merchants and deployments. The host must explicitly map a verified source recipient to a local recipient. Matching email addresses grants no authority.

## Revisioned producers

`notify.Sink.Apply(ctx, []notify.Mutation)` accepts the current state of up to 100 exact notification identities atomically. `postgres.TxSink.ApplyTx` joins that acceptance to the producer's `pgx.Tx`. The PostgreSQL store implements both. An adapter for a remote inbox must durably enqueue delivery in the supplied transaction; a successful enqueue does not claim the remote inbox already contains the message. No event-log polling or automatic sender is built into this library.

```go
notice := notify.Input{
    Recipient: notify.Recipient{Audience: notify.AudienceUser, ID: userID},
    Origin: notify.Origin{System: "contentkit", Scope: appID, ID: "comment.reply:" + replyID},
    Kind: "comment.reply", Data: replyReferences, CreatedAt: createdAt,
    Visibility: notify.VisibilityApp, AppID: appID,
}
err := inbox.ApplyTx(ctx, tx, []notify.Mutation{{Input: notice, Revision: 1}})

// When the reply is deleted, send a newer state for the same exact identity.
err = inbox.ApplyTx(ctx, deleteTx, []notify.Mutation{{
    Input: notify.Input{Recipient: notice.Recipient, Origin: notice.Origin,
        Visibility: notice.Visibility, AppID: notice.AppID},
    Revision: 2, Withdrawn: true,
}})
```

The producer assigns positive revisions from its durable issue state. Delivery retries reuse the revision; allocating a new revision for each retry can incorrectly reopen a resolved issue. Lower revisions are successful no-ops. Equal revisions with the same normalized mutation are successful no-ops; equal revisions with different payload or withdrawal state return `ErrConflict`. JSON object-key ordering and whitespace do not affect retry identity, and numeric values are never converted through floating point.

`Item.ProducerRevision` is separate from `Item.Revision`, which still tracks local receipt and workflow changes. A higher-revision update preserves read/archive state. A higher-revision publication after withdrawal explicitly reopens the item unread and unarchived. Publishing cannot change an existing identity's visibility or app ID. Hosts authorize producer namespaces and recipient accounts before calling a sink.

Withdrawal immediately hides the receipt from active and archived lists, details, and reader mutations, regardless of whether it was read. A withdrawal arriving first stores a hidden marker; a delayed older publication cannot recreate the message. Legacy `Publish` retries also cannot resurrect it. Markers have no automatic expiration because deleting them without a proven retry/restore horizon would permit resurrection. Whole-account/app erasure removes the rows and retains its existing permanent recipient fence.

Batch withdrawal is a batch of exact mutations. Producers determine the affected IDs from their own domain data; there are no wildcard prefixes, ranges, dependency graphs, or destination-inbox lookups. `ApplyTx` uses a savepoint, so a failed batch leaves no partial mutations even if the caller handles its error and commits other work.

Migration 6 adds producer state to the existing inbox table; old rows have producer revision zero. `MergeFrom` preserves producer revisions, withdrawn markers, reader state, and recipient erasure. `ClassifyLegacyOrigins` only classifies unversioned legacy rows: versioned unclassified rows require exact source-state adoption, because relabeling them would invalidate their producer replay digest. Run explicit migrations before serving; stop old inbox readers/writers during the switch to revisioned publication because older binaries do not understand withdrawn markers. General platform event logs and their retention/cursors are independent of notification delivery.

`List` returns active receipts by default; `ListOptions.Dismissed` selects archived receipts. `SetRead` and `SetDismissed` return the canonical item and revision. Neither operation acknowledges the producer or changes workflow state. `Get` includes archived items. `EraseRecipient` takes the same transactional fence as publishing, deletes the inbox and rejects later publications for that recipient. It is permanent account erasure, not dismissal or unsubscribe. Hosts own retention policy and must never purge open work merely because it was read.

Hosts that combine several verified identities use `ListRecipients`, `GetRecipients`, `SetReadRecipients` and `SetDismissedRecipients`. Pass the freshly authorized recipients on every request. SQL filters this set before pagination and mutation; unlinking an identity removes its items from access immediately on the next authorization check. Cursors bind the normalized recipient set, scope and filters. Restart pagination after access changes. Inbox cursors are not durable source-delivery watermarks.

`Origin.Subject` optionally preserves the producer's recipient when the host maps it to a different local identity. It is provenance, not permission, and does not change deduplication identity. A producer retry preserves the original payload, source subject, read/archive state and workflow transitions.

General platform event logs expose facts for optional host integrations. A host may deliberately turn a selected fact into a notice, but the shared inbox does not require an event consumer. Producers can instead publish explicit notifications through a configured sink. Transport delivery, recipient read/archive state and domain resolution are separate operations.

Existing domain records can also project into the common model without duplicating authoritative workflow storage. Operator findings retain their evidence, severity, reopen rules and resolution executor. `Revision: 0` denotes an unversioned external projection: call the source's authorized domain API to act on it. A historical event's `ActionRequested` flag does not imply that its problem remains unresolved.

The package creates no routes, workers or automatic delivery channels. AuthKit, OpenRails, ContentKit and application-specific producers can publish stable notification IDs through the same sink when configured. Applications own authentication, preferences, rendering and their existing job-runner lifecycle.

Run the real persistence checks against an owned PostgreSQL database:

```sh
NOTIFY_TEST_DATABASE_URL='postgres://…' go test -race ./notify/...
```

The tests also accept `RIVER_TEST_DATABASE_URL`, used by the repository's ordinary check script. Each test creates and removes only its own schema. No application or external service dependencies are added.

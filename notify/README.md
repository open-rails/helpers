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

Use `PublishTx` inside a domain mutation's `pgx.Tx` when both share the database. A rolled-back comment creates no inbox item. A remote importer instead publishes after fetching the source; retries retain the first payload and local state. Deduplication includes the local scope, audience, recipient and source system/scope/ID, permitting deliberate fan-out while isolating independent merchants and deployments. The host must explicitly map a verified source recipient to a local recipient. Matching email addresses grants no authority.

`List` returns active receipts by default; `ListOptions.Dismissed` selects archived receipts. `SetRead` and `SetDismissed` return the canonical item and revision. Neither operation acknowledges the producer or changes workflow state. `Get` includes archived items. `EraseRecipient` takes the same transactional fence as publishing, deletes the inbox and rejects later publications for that recipient. It is permanent account erasure, not dismissal or unsubscribe. Hosts own retention policy and must never purge open work merely because it was read.

Hosts that combine several verified identities use `ListRecipients`, `GetRecipients`, `SetReadRecipients` and `SetDismissedRecipients`. Pass the freshly authorized recipients on every request. SQL filters this set before pagination and mutation; unlinking an identity removes its items from access immediately on the next authorization check. Cursors bind the normalized recipient set, scope and filters. Restart pagination after access changes. Inbox cursors are not durable source-delivery watermarks.

`Origin.Subject` optionally preserves the producer's recipient when the host maps it to a different local identity. It is provenance, not permission, and does not change deduplication identity. A producer retry preserves the original payload, source subject, read/archive state and workflow transitions.

OpenRails produces immutable billing notification events; ContentKit and OpenRails SaaS each persist their own inbox using this package. Each application registers its own source consumer, publishes fetched events idempotently, and acknowledges only successful publications or deliberate policy skips. A crash between local commit and source acknowledgement safely replays the same item. Optional routine billing receipts can be skipped by default without suppressing failures. Source delivery acknowledgements, recipient read/archive state and domain resolution are separate operations.

Existing domain records can also project into the common model without duplicating authoritative workflow storage. Operator findings retain their evidence, severity, reopen rules and resolution executor. `Revision: 0` denotes an unversioned external projection: call the source's authorized domain API to act on it. A historical event's `ActionRequested` flag does not imply that its problem remains unresolved.

The package creates no routes, workers or automatic delivery channels. AuthKit's existing event hook and application-specific producers can publish stable event IDs when a host chooses a concrete recipient mapping. Applications own authentication, preferences, rendering and their existing job-runner lifecycle.

Run the real persistence checks against an owned PostgreSQL database:

```sh
NOTIFY_TEST_DATABASE_URL='postgres://…' go test -race ./notify/...
```

The tests also accept `RIVER_TEST_DATABASE_URL`, used by the repository's ordinary check script. Each test creates and removes only its own schema. No application or external service dependencies are added.

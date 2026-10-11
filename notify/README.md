# Shared inbox

`helpers/notify` describes general notices and actionable items without importing an application, router, identity provider or database. `helpers/notify/postgres` stores them on the host's PostgreSQL pool. AuthKit, OpenRails, ContentKit and their hosts can use the same types; adopting this package does not require any of them to invent notifications.

General notices, such as a comment reply or a receipt, have an empty `Status`. Tracked actionable items, such as an operator finding, have `open`, `resolved` or `dismissed` status. **Read state is independent:** reading a failed-payment notice does not fix the payment. `Transition` only records workflow state; the application authorizes and executes the real action. `TransitionTx` can commit that action and its resolution atomically. Revision checks reject stale actions.

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

Use `PublishTx` inside a domain mutation's `pgx.Tx` when both share the database. A rolled-back comment creates no inbox item. A remote importer instead publishes after fetching the source; retries retain the first payload and local state. Deduplication includes the local scope, audience, recipient and complete origin, permitting deliberate fan-out while isolating independent merchants and deployments. The host must explicitly map a verified source recipient to a local recipient. Matching email addresses grants no authority.

`List` returns bounded keyset pages. Cursors bind scope, recipient and filters, and are not durable delivery watermarks: restart retained-inbox scans to catch late commits. `SetRead` does not acknowledge the producer, send mail or change workflow state. `EraseRecipient` takes the same transactional fence as publishing, deletes the inbox and rejects later publications for that recipient. It is permanent account erasure, not dismissal or unsubscribe. Hosts own retention policy and must never purge open work merely because it was read.

Existing domain records can implement the same model without duplicating storage. OpenRails customer notices retain their IDs and exact read timestamps; operator findings retain their evidence, severity, reopen rules and resolution executor. Project both as `notify.Item`, with distinct audiences. `Revision: 0` denotes an unversioned external projection: call the source's authorized domain API to act on it. Importing a retained source inbox creates local history; it does not promise an archive of rows the source deleted before polling, or synchronize source resolution/deletion. An application needing a live operational work list should read that authoritative source.

The package creates no workers or delivery queue and sends nothing by itself. Existing durable event hooks can publish using their stable event IDs; HTTP polling can import retained pages. Applications may add delivery channels using their existing job runner. A job acknowledgement, an inbox read and a domain resolution remain three different operations.

Run the real persistence checks against an owned PostgreSQL database:

```sh
NOTIFY_TEST_DATABASE_URL='postgres://…' go test -race ./notify/...
```

The tests also accept `RIVER_TEST_DATABASE_URL`, used by the repository's ordinary check script. Each test creates and removes only its own schema. No application or external service dependencies are added.

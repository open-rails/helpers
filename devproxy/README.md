# Development process proxy

`cmd/devproxy` owns a stable HTTP port and independently manages application
generations. Build first, then submit the completed executable. The serving
generation stays active while a candidate starts, and also when a build or
candidate fails. This is a local development supervisor, with a mode-0600 Unix
control socket.

```sh
go build -o /private/tools/devproxy ./cmd/devproxy
/private/tools/devproxy serve --listen 127.0.0.1:2052 \
  --control /private/run/app.sock

/private/tools/devproxy deploy --control /private/run/app.sock \
  --companion '["run-media-worker"]' -- /private/builds/app-123 run-server
/private/tools/devproxy status --control /private/run/app.sock
```

`deploy` sends its current directory and environment. The supervisor snapshots
the executable into a private generation directory, selects an ephemeral API
port, overrides `PORT` and `OPS_PORT=0`, and starts it without a shell. Each
repeatable `--companion` is a JSON argument array for that same executable with
its own port. Every companion must remain alive; the API must return 200 from
`/readyz`. The dev-only readiness handler must echo
`X-Devproxy-Ready-Token` when the request value equals its
`DEVPROXY_READY_TOKEN` environment value. A 200 response without the matching
token cannot promote a different listener that happened to take the port.
Set application issuer/public URLs explicitly to their stable public
addresses rather than deriving them from the ephemeral backend port.

After readiness, new requests go to the candidate and existing requests stay
with their old generation. The proxy preserves the original Host, handles
HTTP upgrades, and does not retry requests after an upstream disconnect.
`--drain-timeout` bounds old requests, including long-lived upgraded connections
(default 30s); after that they close. Then every old process group receives
SIGTERM, followed by SIGKILL after `--stop-timeout` (default 45s). Applications
should interrupt background jobs as shutdown begins, keep their database
connections open until the queue acknowledges them, and drain HTTP before
closing dependencies. The budget allows a 20s HTTP drain and 20s of cleanup;
it is still a hard limit for a process that ignores cancellation. The readiness
deadline is `--ready-timeout` (default 60s).

The deploy command exits after promotion and does not own the server process.
An external watcher may freely stop its old deploy command while compiling a
new executable. Stop the supervisor itself to stop all managed generations.
A companion that dies after promotion marks status unhealthy and `/readyz`
returns 503; it never promotes another in-flight generation implicitly.

This protects planned rebuilds and failed candidate startup. It does not keep
an old generation indefinitely to recover from a later application crash, or
guarantee child cleanup if the supervisor itself is forcibly killed with
SIGKILL. Use a normal interrupt or SIGTERM for its graceful shutdown.

Run the subprocess and concurrency checks with:

```sh
go test -race ./devproxy ./cmd/devproxy
```

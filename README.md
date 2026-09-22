# helpers

Shared Go helpers for HTTP APIs and composing River jobs.

One repository, one Go module and one release version. Applications import the independent packages they need:

- `github.com/open-rails/helpers/apikit`: HTTP errors, responses and pagination.
- `github.com/open-rails/helpers/apikit/gin`: Gin writers, binding and locale helpers.
- `github.com/open-rails/helpers/riverkit`: compose library jobs into one host-owned River client.

APIKit and RiverKit are being consolidated here with their existing behavior and regression evidence. MigrateKit remains a separate library and is outside this repository. No helper package imports its sibling unless required by its purpose; the Gin adapter depends on APIKit, while the API core does not depend on Gin or River.

The migration is in progress; consumer adoption and the first shared release follow the qualified package import.

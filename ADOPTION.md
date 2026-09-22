# Adoption

One module: `github.com/open-rails/helpers`. Go1.26.6 or newer is required; the patch floor and dependency pins include the security fixes qualified during consolidation.

| Previous import | Shared import | Package name |
| --- | --- | --- |
| `github.com/open-rails/apikit` | `github.com/open-rails/helpers/api` | `api` |
| `github.com/open-rails/apikit/adapters/gin` | `github.com/open-rails/helpers/api/gin` | `ginapi` |
| `github.com/open-rails/riverkit` | `github.com/open-rails/helpers/river` | `river` |

Where both upstream River and the helpers are imported, use an explicit descriptive alias to distinguish them. No old-module forwarding package is provided. MigrateKit remains unchanged.

AuthKit and OpenRails expose the composer contribution type, so migrate/release those libraries with the same helpers version before migrating host composers. Current integration lanes include Doujins956, Hentai0695, OpenRails-SaaS87, Cozy-art330 and the OpenRails demo. Preserve their active worktrees and adopt the coordinated public versions. Primary checkouts may predate these lanes.

This move preserves APIKit's current response contract. It does not silently migrate older `doujins-org/ginapi` writer semantics in an application. The frozen HTTP/parser receipts document those differences; any such writer adoption requires its own downstream qualification. The old repositories and tags remain available until consumer adoption is complete.

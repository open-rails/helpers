# Consolidation qualification

Owner: /root. Basehelpers6fa3079. Includes API donor09aaa0f and River donorb821ead; root owns module/CI/docs. Only APIKit and RiverKit are consolidated; MigrateKit stays separate.

API runtime namespace-only comparison to9753ffd passed; original70 HTTP fixtures and15 parser assets are unchanged. API/Gin tests, normalized socket-byte cases and real parser probe pass. River source normalizes to publishedb971676 apart from package/upstream alias/test-env renames; actual PostgreSQL composition/ownership tests pass. Independent common-file review found no dependency-cycle, isolation or gate omission.

One go.mod, no workspace or reverse AuthKit/OpenRails/oldhelper dependency. API package has standard-library-only imports. Root scripts/check.sh passes with owned PostgreSQL and all four Go package suites under race/vet; Node probe matches70 fixtures. Security scan initially identified old Go1.26.0 and quic-go0.54 inherited pins. Go1.26.6, quic-go0.59.1 and x/crypto0.57 remove the affected code/imported-package reports. Final scan reports0 affected code and0 affected imported packages; one unimported legacy module-package advisory remains informational. No HTTP payload change is made for these updates.

The single CI job runs this same entrypoint, with a required PostgreSQL DSN and pinned vulnerability checker. Publication and consumer migration follow exact-head CI. No MigrateKit edit, deployment, live provider call or deletion of old source repositories.

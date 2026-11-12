# Backend remediation tracking

Specification: user-provided `LawPilot_BACKEND_FIXES_README_v2_CALENDAR.md`. It reviewed `main` at `3bacf15`. This tracker separates implemented code from future contract proposals and integration acceptance gates.

## Current changes: durable execution, native calendar and gRPC foundations

The existing backend now has durable mock AI acceptance, polling/cancel/requeue APIs, a leased/fenced outbox dispatcher, real Temporal workflow/activities, scoped DB roles, restart reconciliation, persistent local Temporal infrastructure and process-kill tests. See [DURABLE_EXECUTIONS.md](DURABLE_EXECUTIONS.md) for wire changes, migration 005, limits, and commands.

This is not completion of the full roadmap. Real provider write/reconciliation tests, production identity, authoritative approval, confidential document/evidence processing and pilot gates remain open. The existing mock AI and Clio non-implementation boundaries are preserved.

| Specification item | Current status / next acceptance work |
|---|---|
| P0-01 durable execution | Local mock path implemented and tested with real PostgreSQL/Temporal; external-write recovery remains dependent on provider integration; awaiting_review has no approval ingress yet |
| P0-02 trusted workload identity | AI/calendar paths use mTLS with workload caller allowlists and delegated user JWT verification for calendar; production issuance/rotation and Temporal identity remain pending |
| P0-03 authoritative approval | Pending: authenticated immutable-artifact decisions and trusted effects; keep sample approval workflow separate |
| P0-04 evidence authorization | Pending: authoritative document/version/passage resolution and claim support checks |
| P0-05 contract governance | Generated Go/TS/Python gRPC clients, Buf wire/JSON checks and generation drift CI added; broader provider/event contract governance remains pending |
| P0-06 migrations/recovery | Forward SQL 005 and local role separation added; version runner, checksums, production role provisioning, restore/upgrade tests pending |
| P1-01 provider registry | Pending: DB ownership resolution and two adapter conformance fixtures |
| P1-02 Clio adapter | Pending: real OAuth, sandbox, vault-backed credentials, webhooks and rate/reconciliation handling |
| P1-03 context engine | Pending: typed recipes, scoped source snapshots, reproducibility and invalidation |
| P1-04 document ingestion | Pending: storage/scanning/parsing, versioned passages and embedding/retrieval pipeline |
| P1-05 real AI capabilities | Pending: model gateway, approved providers/templates, legal evaluations and output gates |
| P1-06 calendar domain | Native Go domain, civil time/bounded recurrence, immutable proposals, ACL/revision/conflict tests and Temporal reminders added; external sync, invitations and legal deadline verification remain pending |
| P1-07 general events | Execution command delivery only is implemented; event publication, consumer inbox and revision handling remain pending |
| P1-08 calendar AI | Pending: authorized fresh briefings and approval-backed scheduling proposals |
| P1-09 firm knowledge/templates | Pending: immutable approved versions, CRUD and publish permissions |
| P2-01 authorization | Extend tested tenant/matter/capability boundary to new action-specific domains |
| P2-02 provider/native | Pending: ownership/cutover/rollback and native provider scope |
| P2-03 privacy/security | Pending: adversarial release gates and policy/legal review |
| P2-04 operations | Durable status/leases/retry state inspectable; Go/Temporal/Rust tracing, metrics, alerts and production exporters pending |
| P2-05 CI/deployment | Unit/SQL/process-recovery CI added; scanning, compatibility, load/chaos coverage and production deployment remain pending |
| P2-06 documentation | README, API contracts, layer status and operations updated for this slice; keep each future claim tied to tests and deployment evidence |

The owner explicitly chose a fresh PostgreSQL 18 database without retaining PostgreSQL 16 data. That instruction takes precedence over the specification's proposed PG16 data-preservation work. Future upgrades and recovery of actual PostgreSQL 18 data still need the P0-06 migration/restore process.

Migrations `005` and `006` now cover durable execution and input representation. Calendar migrations must use the next available number, not the illustrative `005_calendar_domain.sql` name in the review snapshot.

Next reviewable slices follow the specification: P0-02/P0-03 service identity and authoritative approvals, P0-05/P0-06 contract/migration governance, then trusted provider routing and the general events/calendar foundations. External account/model choices and pilot acceptance remain product-owner decisions; credentials must be configured through environment settings, never committed or requested in chat.

Calendar/gRPC implementation details, local environment and explicit limits: [CALENDAR_GRPC.md](CALENDAR_GRPC.md). Migration 007 adds this native domain. Calendar proposal approval is scoped to native scheduling; it does not complete P0-03 authoritative AI/legal artifact approval.

Calendar overlap policy is explicit: new shared firm calendars allow overlaps; exclusive single-resource calendars enforce calendar-wide conflicts. Migration 008 preserves existing exclusive behavior, with a manager-only policy transition API. Attendee/resource conflicts across calendars and external reservation semantics remain pending.

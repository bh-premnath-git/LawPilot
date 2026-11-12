# Backend layer order and current boundaries

Work proceeds in this order. The changes below harden the existing starter; they do not implement real legal AI, Clio synchronization or a production approval service.

| Order | Layer | Technology | Changes in this pass | Remaining integration |
|---|---|---|---|---|
| 1 | API Gateway | NestJS / TypeScript | Permission checks and database actions share a tenant transaction; UUID tenant context is normalized; JSON body limit is 32 KiB; responses carry server-generated request IDs and no-store headers; AI endpoint returns documented HTTP 200 | Durable asynchronous execution, readiness and rate limits |
| 2 | AI Orchestration | TypeScript | Registry version, uniqueness and limits validated at startup; result identity/version and mandatory review enforced; external actions rejected; completed matter results need evidence references; byte-safe streamed request handling and HTTP deadlines | Verified passage resolution, model/provider budgets and workload identity |
| 3 | AI Capabilities | Python | Typed UUID identity fields, nonempty actor/trace IDs, validated registry, streaming body limit, malformed-signature rejection and consistent UTF-8 input sizing | Real model calls and per-capability evidence/template services; current outputs remain mocks/abstentions |
| 4 | Business Workflows | Go + Temporal | Approval waits expire (24-hour default, 30-day maximum); signals bind tenant, draft, version and SHA-256; stale/wrong-scope signals ignored; state query added | Trusted signal ingress, authoritative approver verification, activities/retry policy, workflow persistence integration and external-outcome reconciliation |
| 5 | Provider Integration | Go | Typed canonical matter response, UUID validation, trailing JSON rejection, input bound and server deadlines; mock/Clio capability distinctions tested | Real adapters, tenant-owned connection lookup, OAuth/vault/webhooks and provider conformance against a sandbox |
| 6 | Document Processing | Rust | Detect input exceeding 1,000,000 bytes without accepting truncation; reject unknown fields; deterministic SHA-256 and UTF-8 byte-size tests | Sandboxed worker deployment, scanning, real document parsing and provenance |
| 7 | Identity | Keycloak 26.8.2 | Native organizations enabled in realm import; existing PKCE, audience and server-side authorization boundary retained | Trusted organization-to-tenant provisioning and membership reconciliation; organization/role claims never automatically grant database access |
| 8 | Persistence | PostgreSQL + pgvector | PostgreSQL 18 / pgvector 0.8.7 enabled; tenant directory covered by RLS; bounded pool acquisition and SQL statements; AI state changes atomically append metadata-only audit/outbox events; upgrade migrations and rollback-only isolation test | Outbox dispatcher, service-specific roles, retention and backup/restore validation |
| 9 | Observability | OpenTelemetry | Optional OTLP export and W3C context propagation across API → Supervisor → Python; static span names/status only; shutdown flushing | Go provider/Temporal/Rust instrumentation, dependency readiness, metrics and production telemetry storage |

## Upgrade an existing local database

The Docker stack now uses PostgreSQL 18 with pgvector 0.8.7 and a fresh `pgdata18` volume. PostgreSQL 16 data is deliberately not migrated. Seed tenant data/grants again after first start. Fresh databases use `db/001_init.sql`. Existing PostgreSQL volumes do not rerun initialization scripts. Apply the following as the database administrator, from the repository root:

```bash
docker compose --env-file .env exec -T postgres psql -U postgres -d lawpilot -v ON_ERROR_STOP=1 < db/002_tenant_directory_rls.sql
docker compose --env-file .env exec -T postgres psql -U postgres -d lawpilot -v ON_ERROR_STOP=1 < db/003_execution_outbox.sql
docker compose --env-file .env exec -T postgres psql -U postgres -d lawpilot -v ON_ERROR_STOP=1 < db/004_pgvector.sql
```

The API needs the outbox INSERT permission before the new execution path is started. It cannot publish or mark outbox events delivered; that belongs to a future dispatcher. Audit/outbox payloads contain execution IDs, state and decision codes, not legal content or tokens.

For an existing Keycloak realm, enable **Organizations** in its admin settings or apply an explicit realm configuration migration. Editing the import JSON does not update an existing realm. Organization membership does not replace LawPilot SQL membership/capability/matter grants.

Approval workflow inputs now require `DraftSHA256` (64 lowercase hex characters) and `RequiredPermission=draft.approve`. Decisions must include matching `TenantID`, `DraftID`, `DraftVersion` and `DraftSHA256`, plus `ApproverID`. These are scaffold contract changes: do not deploy over running workflows without a workflow versioning/migration strategy. A returned `pending-authoritative-authorization` is not an approved action or permission to write to a provider.

The API execution path is still synchronous. A process crash can leave an accepted execution pending; an outbox record alone does not provide a running retry/recovery service.

## Validation

```bash
npm ci
npm run build
npm test
python3 scripts/verify_structure.py
# In a Python virtual environment with the worker's requirements installed:
(cd services/ai-capabilities && python -m pytest -q)
(cd services/workflow-service && go test ./...)
(cd services/provider-router && go test ./...)
(cd services/document-worker && cargo test)
docker compose --env-file .env exec -T postgres psql -U postgres -d lawpilot -v ON_ERROR_STOP=1 < db/tests/tenant_isolation.sql
docker compose --env-file .env exec -T postgres psql -U postgres -d lawpilot -v ON_ERROR_STOP=1 < db/tests/pgvector.sql
```

The SQL test uses synthetic records and rolls them back. Temporal unit tests use its in-process test environment, without requiring a running Temporal cluster.

Live validation also exercised real Keycloak tokens, API permission denials and granted reads, AI delegation, duplicate/conflicting idempotency requests, membership revocation, paired audit/outbox writes and a common exported W3C trace across the three AI request services.

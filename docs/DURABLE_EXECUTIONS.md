# Durable mock AI execution (P0-01)

This implements the first remediation slice against the existing backend, without enabling real AI, confidential-document retrieval, attorney approvals or provider writes. The remaining work is tracked in [REMEDIATION_STATUS.md](REMEDIATION_STATUS.md).

## Request path

```mermaid
flowchart LR
 Client --> API[NestJS API]
 API --> DB[(Execution + audit + outbox transaction)]
 DB --> Dispatcher[Leased dispatcher]
 Dispatcher --> Temporal[Temporal workflow]
 Temporal --> Worker[Go activities]
 Worker --> Supervisor[TypeScript Supervisor]
 Supervisor --> Python[Safe mock capabilities]
 Worker --> DB
 Client --> Status[Authorized status endpoint]
 Status --> DB
```

`POST /v1/tenants/{tenantId}/executions` validates the authenticated principal's current membership, capability and matter grant. It atomically stores the execution, request input, audit event and metadata-only dispatch intent. It returns **202** with `executionId`, `status`, `statusUrl`, and a `Location` header. It does not wait for Temporal or AI availability.

The old `POST /v1/tenants/{tenantId}/ai/execute` path is now a deprecated alias with the same **202** behavior. This is a deliberate wire-behavior change: callers must poll `statusUrl` instead of expecting a synchronous result. An idempotency key reused with equivalent JSON object ordering returns the existing execution; changed content returns `409`.

- `GET /v1/tenants/{tenantId}/executions/{id}`: owning principal only, with current membership/capability/matter grants rechecked. Other principals receive `404`. Revoked access receives `403`; raw stored request input is never returned.
- `POST .../executions/{id}/cancel`: atomically records cancellation and a Temporal cancellation intent. It is idempotent; already completed terminal results remain unchanged. Workers cannot overwrite a cancelled state with an AI result.
- `POST .../executions/{id}/dispatch/retry`: requeues a dead-lettered start intent for an authorized owner, preserving its event/execution/workflow IDs. Terminal executions cannot be restarted this way.
- `GET /v1/readyz`: checks database connectivity and the durable schema. Temporal being offline does not prevent safe command acceptance.

## State and delivery semantics

Execution states are `accepted → queued → running → awaiting_review | completed | rejected | cancelled | failed`. The approval-related states are reserved for the next approval slice: a completed AI output requiring attorney review remains `awaiting_review` and cannot trigger a provider action. There is no approval/publish endpoint in this change.

The dispatcher claims eligible **execution commands only**, using `FOR UPDATE SKIP LOCKED`, a 30-second lease and a random fencing token. It processes one command per poll (one second) per dispatcher; additional dispatcher instances can claim independently. A single Temporal RPC has a 10-second deadline. Expired claims are reclaimable; stale tokens cannot acknowledge/requeue a newer claim.

The stable workflow ID is `lawpilot:{tenantId}:ai:{executionId}`. Temporal rejects duplicate workflow IDs even after completion. If the start acknowledgement is lost, retrying the same ID recognizes the existing workflow instead of starting a second one. Delivery retries use exponential backoff capped at five minutes; after ten recorded failed deliveries, status exposes `dead_letter` and a sanitized error code. Owners can explicitly requeue it after the outage is resolved.

Go activities use bounded retries/timeouts and reauthorize against authoritative database records both before calling the Supervisor and before storing a response. Already committed activity results are not recomputed. A cancelled execution cannot be overwritten. If retries exhaust or a permanent policy/validation error occurs, a separate retryable failure activity persists the outcome. A periodic reconciler also marks stale queued/running executions failed when their Temporal workflow has failed, expired, been cancelled or been terminated.

Read-only AI requests **can be repeated** if a worker crashes between the request and result commit. Tests prove one committed completion, not exactly-once inference. No provider writes are available here: uncertain external effects still need operation-level reconciliation in a later integration slice.

Non-command lifecycle events remain pending in the global outbox. They are not falsely marked delivered: the general event publisher/inbox is P1-07. The current dispatcher starts/cancels workflows; it is not a general event bus.

## Data and trust boundaries

Inputs/results remain in tenant-isolated PostgreSQL records. Only IDs and sanitized status/failure metadata enter outbox payloads and Temporal activity/workflow histories. The dispatcher DB role has no direct table access; narrowly scoped security-definer functions expose leased command metadata and stale execution references. The workflow role has separate, RLS-enforced permissions for loading authorized requests and persisting execution/audit/outbox changes.

The API no longer needs the shared internal HMAC key because it does not call internal AI endpoints. The Go worker still signs development transport requests to the Supervisor. Shared-HMAC replacement, workload authentication, authoritative approval and evidence verification are still P0-02 through P0-04. These local roles, static development passwords and the dev Temporal server are not production provisioning.

## Start and upgrade

Fresh Compose databases run `001_init.sql` followed by `005_durable_executions.sql`. Existing PostgreSQL 18 installations must apply migration **005 once**, before starting the API/worker/dispatcher:

```bash
# Stop the old synchronous API before migration so it cannot accept new legacy rows.
docker compose --env-file .env stop platform-api
docker compose --env-file .env exec -T postgres psql -U postgres -d lawpilot -v ON_ERROR_STOP=1 < db/005_durable_executions.sql
docker compose --env-file .env up --build -d
curl -fsS http://localhost:3000/v1/readyz
```

The migration preserves past execution results. Legacy accepted synchronous rows have no persisted input; they are explicitly marked `failed / LEGACY_INPUT_UNAVAILABLE` instead of inventing/replaying their original request. Migration version tracking, checksums, a general runner and backup/restore automation remain P0-06. Do not rerun 005 against an already migrated schema.

Compose includes Temporal CLI **1.10.0** (embedded server 1.32.2), an execution worker and an outbox dispatcher. Temporal history persists in the separate `temporaldata` volume using SQLite. This is a local dev server, not a production HA cluster. The gRPC port is loopback-bound at `localhost:7233`; the local UI is `http://localhost:8233`. Graceful shutdown preserves history; do not delete volumes whose state you need.

Host-run processes require the explicit values in `.env.example`:

```text
WORKFLOW_DATABASE_URL=postgresql://lawpilot_workflow:local-dev-workflow-only@localhost:5432/lawpilot
DISPATCHER_DATABASE_URL=postgresql://lawpilot_dispatcher:local-dev-dispatcher-only@localhost:5432/lawpilot
TEMPORAL_ADDRESS=localhost:7233
TEMPORAL_NAMESPACE=default
TEMPORAL_TASK_QUEUE=lawpilot-executions-v1
```

The earlier approval sample can keep a separate worker with `TEMPORAL_TASK_QUEUE=lawpilot-approval-v1`; existing approval histories are not migrated or signalled by this change.

The worker additionally needs the shared development `INTERNAL_HMAC_KEY`, `SUPERVISOR_URL` and a capability registry. The dispatcher uses its own DB URL plus the workflow-role URL for the narrowly scoped stale-state reconciler. Keycloak user creation and synthetic SQL membership/grant seeding remain as documented in the main README.

## Recovery verification

Unit suites run through `npm test` and `(cd services/workflow-service && go test ./...)`. Real-database suites opt in through `TEST_DATABASE_URL`, `TEST_APP_DATABASE_URL`, `TEST_WORKFLOW_DATABASE_URL` and `TEST_DISPATCHER_DATABASE_URL`; CI configures synthetic credentials and runs them after migrations.

The process recovery suite requires a migrated development database, the built API, two Go binaries, and Temporal CLI 1.10.0:

```bash
npm run build
mkdir -p work
(cd services/workflow-service && go build -o ../../work/execution-worker ./cmd/worker && go build -o ../../work/outbox-dispatcher ./cmd/dispatcher)
# Set the four TEST_*_DATABASE_URL variables to the local admin/app/worker/dispatcher roles.
# TEMPORAL_BIN can point to a downloaded temporal binary, otherwise temporal is found on PATH.
node scripts/test_durable_recovery.cjs
```

The suite generates synthetic JWTs against its own local JWKS server (not a live Keycloak integration test), starts an isolated real Temporal server with persistent SQLite, and exercises:

- API kill/restart after acceptance with the same idempotency key.
- Accepted commands dispatching while no worker is running.
- Dispatcher kill/restart after a workflow start.
- Worker kill mid-activity and one committed completion after a read-only retry.
- Temporal kill/restart with its existing durable database.
- Cancellation before dispatch, owner-only status, revoked grants before execution, and forged result rejection.
- Absence of private input sentinels in outbox payloads and Temporal history.

DB tests separately cover lease expiration/fencing, lost-start-ack deduplication, dead-letter visibility, retry permissions, dispatcher input isolation, revoked access during a running agent call, and no re-execution of an already committed result. Fixtures are removed after the tests. Logs and the synthetic Temporal database are retained under `work/recovery-*` for diagnosis.

OpenTelemetry currently traces API requests and Supervisor → Python calls separately. Propagation across dispatcher/Temporal/Go is not yet implemented; workflow IDs and persisted delivery/activity state provide operational correlation in this slice. Broader tracing remains P2-04.

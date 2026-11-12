# Native calendar and authenticated service contracts

This slice implements a PostgreSQL-owned native calendar. Clio/calendar OAuth, webhooks, external synchronization, invitations, email/SMS delivery, and verified legal deadline calculation remain unimplemented. Availability and sync responses explicitly report `native_only`; external availability is unknown. AI capability implementations remain safe mocks.

## Transport and authority

| Caller | Receiver | Contract / authority |
|---|---|---|
| Browser | NestJS API | REST, Keycloak access token; strict request validation |
| NestJS API | Go calendar | `lawpilot.calendar.v1.CalendarService`, mTLS `api-gateway` plus original user bearer token; calendar independently verifies issuer, audience, signature, membership and grants |
| Go execution worker | TS Root Supervisor | `lawpilot.rpc.v1.SupervisorService`, mTLS `execution-worker`; input loaded from accepted durable execution |
| TS Root Supervisor | Python capability | `lawpilot.rpc.v1.CapabilityService`, mTLS `root-supervisor`; registry and result validation |
| Go reminder worker | Go calendar | Reference-only `LoadReminderWork` / `DeliverReminder`, mTLS `execution-worker`; current stored actor/entry revision reauthorized before delivery |
| Dispatcher / workers | Temporal | Temporal SDK, existing local development server; production Temporal identity/TLS configuration remains pending |

Other components are not silently converted: the development provider-router remains an HMAC HTTP mock, Rust processing is isolated stdio, and database access uses scoped SQL roles. Internal gRPC ports are not published by Compose. AI execution HTTP is disabled by default; the old process-recovery fixture explicitly enables its development-only compatibility switch. No unencrypted fallback is used for gRPC.

Workload identity is verified from the CA-validated certificate CN and an explicit caller allowlist. This is local workload authentication, not a managed production identity/rotation system. The certificate CA is powerful: only provision authorized workload identities. AI requests contain trusted execution context; calendar public contracts contain no client-selectable actor field.

## Local setup and migration

```bash
cp .env.example .env
bash scripts/generate_dev_tls.sh
# Put the outputs of `id -u` and `id -g` in LOCAL_UID/LOCAL_GID in .env.
# Set INTERNAL_HMAC_KEY to a random 32+ character secret for the dev provider mock.
docker compose --env-file .env up --build -d
```

Development certificates expire after seven days. Stop the stack, explicitly remove `work/tls`, regenerate, then recreate service containers to rotate. Keys are 0600; each container receives only its own leaf key/certificate and the public CA. The CA private key stays on the host. Never commit generated credentials or reuse this CA in production.

Fresh databases apply `007_calendar_domain.sql` after 005/006, then `008_calendar_overlap_policy.sql`. For an existing PG18 database, stop the API, calendar service, execution worker and dispatcher during this schema change and apply each missing migration once as migration administrator:

```bash
docker compose exec -T postgres psql -U postgres -d lawpilot -v ON_ERROR_STOP=1 < db/007_calendar_domain.sql
docker compose exec -T postgres psql -U postgres -d lawpilot -v ON_ERROR_STOP=1 < db/008_calendar_overlap_policy.sql
```

Migration 008 and the matching service release must be deployed together: old writers do not supply the new policy field for exclusive calendars. The calendar service checks for the new column at startup. No additional environment variable is required for booking policy.

The new role is `lawpilot_calendar` with a local-only placeholder password documented in `.env.example`; production provisioning must replace it. Tenant RLS applies to every calendar table. Calendar service can update only decision columns on stored proposals, not their immutable command bytes/hash. This migration does not introduce a migration version/checksum runner.

The seed script now grants synthetic staff `calendar.read`, `calendar.write`, `calendar.approve`, `calendar.manage`, and `calendar.availability`. Calendar creation also grants its creator explicit calendar permissions. Additional staff need both capability and calendar grants; matter-linked events require `matter.read` grants. Clients require `calendar.client_read` plus calendar read, participation, shared visibility, and any linked matter grant. Do not grant staff capabilities to a client.

Host-run processes must set their own environment (NestJS does not read `.env` automatically):

| Process | Addresses | Certificate environment |
|---|---|---|
| API | `CALENDAR_GRPC_ADDRESS=localhost:50051` | `GRPC_CLIENT_*` = `work/tls/api-gateway.{pem,key}` |
| Calendar | `CALENDAR_GRPC_ADDRESS=:50051`, `CALENDAR_DATABASE_URL`, `KEYCLOAK_*`, `APP_ENV=development` | `GRPC_SERVER_*` = calendar-service leaf |
| Go worker | `SUPERVISOR_GRPC_ADDRESS=localhost:50052`, `CALENDAR_GRPC_ADDRESS=localhost:50051`, existing workflow DB/Temporal settings | `GRPC_CLIENT_*` = execution-worker leaf |
| Supervisor | `SUPERVISOR_GRPC_BIND=0.0.0.0:50052`, `AI_GRPC_ADDRESS=localhost:50053` | both client/server = root-supervisor leaf |
| Python | `AI_GRPC_BIND=0.0.0.0:50053`, `python -m app.grpc_server` from service directory; set `RPC_PYTHON_PATH` to absolute generated Python directory if needed | server = ai-capabilities leaf |
| Dispatcher | existing DB/Temporal settings, `ENABLE_CALENDAR_REMINDERS=true` | No calendar leaf needed; schedules workflows through Temporal |

Every gRPC process requires `GRPC_CA_FILE`. The full variable names are `GRPC_CLIENT_CERT_FILE`, `GRPC_CLIENT_KEY_FILE`, `GRPC_SERVER_CERT_FILE`, `GRPC_SERVER_KEY_FILE`. Use absolute file paths when starting outside the repository root. Compose supplies these per process automatically. Server name validation pins calendar-service/root-supervisor/ai-capabilities respectively.

## Overlap policy and resource semantics

| Policy | Intended use | Conflict scope |
|---|---|---|
| `shared` (new default) | Shared firm/multi-attorney calendar | Overlaps allowed, including overlapping attendees; no implicit reservation inferred |
| `exclusive` (explicit opt-in) | One calendar representing a single attorney or meeting room | Active busy entries must not overlap within that calendar |

Calendar visibility and booking exclusivity are independent. Participant IDs represent attendance and visibility, not a canonical resource reservation. Two attorneys can have simultaneous appointments on a shared firm calendar. Even exclusive calendars do not enforce attendee/resource conflicts across separate calendars. Use one exclusive calendar per resource when that local scope is sufficient; do not treat it as firm-wide resource scheduling.

Availability returns calendar occupancy, its overlapPolicy, and an explicit warning that attendee/cross-calendar resource availability is not checked. A busy interval on a shared firm calendar does not imply every attorney is unavailable. Before external synchronization, a future resource domain must define stable resource IDs, attendee busy/declined/optional states, cross-calendar conflict rules and provider reservation semantics; this slice does not invent those rules.

Migration 008 preserves existing calendars as `exclusive`, so existing booking intent is not silently relaxed. Managers can explicitly convert existing shared firm calendars using the policy endpoint. The entry-level enforcement flag is bound to the calendar policy by a deferred composite foreign key; a writer cannot bypass exclusivity by downgrading one entry. Policy transitions lock the calendar and update all entries atomically; enabling exclusive mode over conflicting data rolls back completely. Cancelled entries and unverified deadline candidates do not reserve busy time.

## Calendar REST surface

All routes start `/v1/tenants/:tenantId` and require a Keycloak access token. IDs are UUIDs; mutation bodies require an `idempotencyKey` (8–128 characters).

- `GET/POST /calendars`; create accepts name, defaultTimeZone, approvalRequired (defaults true), overlapPolicy (`shared` by default, or `exclusive`).
- `PATCH /calendars/:id/overlap-policy`: overlapPolicy, expectedRevision, idempotencyKey; requires calendar manage permission. Changes are audited and atomic. Existing overlaps prevent switching to exclusive.
- `PUT /calendars/:id/grants/:principalId`: permissions (`read`, `write`, `approve`, `manage`), expectedRevision. An empty permissions array revokes access.
- `GET /calendar-entries?calendarId=...&from=...&to=...`: range at most 93 days; keyset pageSize ≤100 and pageToken. ACL filtering precedes pagination.
- `GET /calendar-entries/:id?calendarId=...`: typed entry, ETag revision.
- `POST /calendar-entries`: calendarId, entry. Direct mutation requires a calendar with approvalRequired false.
- `PATCH /calendar-entries/:id`: calendarId, entry, expectedRevision, scope; optional expectedSeriesRevision. If-Match, when supplied, must match expectedRevision.
- `POST /calendar-entries/:id/cancel`: same revision/scope fields without entry.
- `POST /calendar-entry-proposals`: calendarId and command `{type: "create", entry: ...}` or update/cancel command with entryId and revision/scope fields.
- `GET /calendar-entry-proposals/:id?calendarId=...`: immutable preview, SHA-256 and affected IDs/count.
- `POST /calendar-entry-proposals/:id/approve` or `/reject`: calendarId, expectedSha256 and idempotencyKey. Approval rechecks current proposer/reviewer permissions, versions and conflicts.
- `POST /calendar-availability/query`: calendarId, from, to. Requires dedicated availability capability; returns busy intervals without private titles/IDs/attendees. It is a purpose-specific free/busy permission.
- `GET /calendar-sync/status?calendarId=...`; `GET /calendar-entries/:id/reminders?calendarId=...`.

Entry example:

```json
{
  "title": "Synthetic consultation",
  "description": "Staff notes",
  "kind": "consultation",
  "visibility": "private",
  "schedule": {
    "type": "timed",
    "start": "2027-01-15T14:00:00Z",
    "end": "2027-01-15T15:00:00Z",
    "timeZone": "America/New_York"
  },
  "participantPrincipalIds": [],
  "reminders": [{"minutesBefore": 60}]
}
```

An all-day schedule uses `type: "all_day"`, civil `startDate`, `endDateExclusive` and `timeZone`. Supported years are 2000–2200. Timed inputs use explicit RFC3339 offsets with at most millisecond precision; IANA zones are required. Optional recurrence is `{frequency: "daily"|"weekly", interval: 1, count: 10}`, bounded to 366 occurrences. Recurrences preserve wall time; ambiguous/nonexistent recurrence local times are rejected. Update/cancel scope is `occurrence`, `this_and_following`, or `series`; series changes require expectedSeriesRevision. Recurrence rule replacement is not implemented. Cancelled occurrences are not resurrected.

Private entries are creator-only. Internal entries require staff calendar/matter access. Client views contain only shared appointments/consultations in which that client participates, with staff notes, other participants, and creator identity removed. Overlap blocking applies only to calendars explicitly marked `exclusive`, with transaction serialization and a database exclusion constraint; adjacent events are allowed. Shared calendars allow concurrent appointments. `deadline_candidate` is unverified and does not reserve busy time; `verified_deadline` is rejected. No service calculates or asserts legal deadline authority.

Reminders belong to the requesting actor and entry revision. Temporal timers produce an idempotent in-app `due` state/event after reauthorization; they do not send messages. Changes/cancellations invalidate old scheduled/due reminders. Offsets are 0–40320 minutes, at most three per entry. At-start reminders allow up to one minute of timer/delivery jitter; deliveries at least one minute after event start are suppressed as stale. No user notification inbox/UI is claimed.

## Contract generation and verification

Sources: `contracts/proto`; committed clients: `contracts/gen/go`, `contracts/gen/python`, `packages/rpc/src/generated`. Go shared module is locally replaced by each consumer. CI checks Buf minimal lint, wire/JSON compatibility against main and deterministic generated-file drift. Additive fields use new tags; never reuse removed tags (reserve them).

```bash
npm ci
python3 -m pip install grpcio-tools==1.76.0
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.6
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
RPC_GO_BIN="$(go env GOPATH)/bin" npm run generate:rpc
npm run build && npm test
```

`go test ./...` in calendar-service runs civil-time unit tests; setting `TEST_DATABASE_URL` (admin) and `TEST_CALENDAR_DATABASE_URL` enables real database ACL/concurrency/proposal/reminder tests. `scripts/test_calendar_grpc.cjs` launches actual API, calendar, Supervisor, Python, Go worker/dispatcher and Temporal processes with synthetic JWKS/certificates. It requires all five `TEST_*DATABASE_URL` variables listed in CI, built binaries under work, Python requirements, and Temporal CLI (`TEMPORAL_BIN`). Logs are synthetic and retained under ignored `work/calendar-grpc-*`; no keys/tokens should be uploaded as CI artifacts.

Production workload issuance/rotation, Temporal security, external integrations, generalized event inboxes, authoritative AI/legal approval, full Go/Temporal tracing, load limits and deployment operations remain separate acceptance work.

# Configured file, provider and legal AI services

**Updated scope:** [Capability extensions](CAPABILITY_EXTENSIONS.md) supersede earlier limitations below for webhooks, reconciliation, appointments, OCR and context workspaces. Apply migrations 016–018.

These features are implemented behind disabled-by-default configuration. No real account, document store or model was accessed during development. Tests use synthetic fixtures. Existing Keycloak 26.8.2, PostgreSQL 18/pgvector 0.8.7, native calendar ACLs and shared/exclusive booking policy remain the foundation.

## Start locally

1. Copy `.env.example` to `.env`. Set `LOCAL_UID`/`LOCAL_GID` to your host user and generate development certificates with `bash scripts/generate_dev_tls.sh`. Existing certificates need regeneration in an explicitly new directory to include `integration-service` and `document-parser`; move the new directory to `work/tls` after stopping services. Each container mounts only its own key and the public CA.
2. For a fresh database, `docker compose up --build -d` applies SQL 001–015 and records checksums. Existing volumes do not rerun initialization. For an existing database through 010, verify that schema first, then run `MIGRATION_DATABASE_URL=... node scripts/migrate.cjs --baseline-through=010` using a dedicated migration owner. Baseline is an explicit assertion that existing SQL has already been applied. Thereafter run without the baseline flag; changed applied files fail checksum validation. Never supply migration credentials to runtime services.
3. Leave `FILE_STORAGE_ENABLED`, `CLIO_ENABLED`, `GENERIC_PROVIDER_ENABLED`, `PROVIDER_WRITES_ENABLED` and `LEGAL_AI_ENABLED` false until their configuration is complete. Enabling incomplete configuration fails startup; disabled features return an explicit unavailable/unconfigured response.
4. Add the SQL capabilities described below to actual Keycloak subjects and grant `matter.read` on each relevant matter. Token roles alone do not grant access. `scripts/seed_local_tenant.sql` is a synthetic starting point. Use separate subjects for author and reviewer.

Compose is local development infrastructure, with placeholder SQL passwords and dev Keycloak/Temporal. Production requires external secrets, TLS PostgreSQL/Temporal, CA issuance, authenticated ingress and outbound policies; changing APP_ENV alone is not deployment provisioning.

## Files: private quarantine → scan → isolated parse → index

Set `FILE_STORAGE_ENABLED=true`, `S3_BUCKET`, `S3_REGION`, canonical `S3_KMS_KEY_ID` ARN and `CLAMAV_ADDRESS`. Prefer IAM credentials; optional `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY` and session token support local setups. Optional S3-compatible endpoint must support AWS checksum, SSE-KMS and conditional writes. Bucket startup validation requires all four public-access blocks and default encryption with the same KMS key. Never allow public or tenant-direct bucket listing. Use separate IAM prefixes/policies for quarantine and released originals; only this service may promote an object.

`docker compose --profile documents up --build -d` adds the document parser on an internal network with no Internet route, read-only root, dropped capabilities, process/memory/CPU limits and bounded scratch space. Supply an updated ClamAV deployment separately. In production use a TLS scanner endpoint (`CLAMAV_CA_FILE`, `CLAMAV_SERVER_NAME`, optional client pair). A Compose override must mount those certificates and set the corresponding container paths. Outbound scans fail closed on timeout, malformed response or unavailable signatures/scanner.

API routes (all under `/v1/tenants/:tenantId`):

| Route | Input / behavior |
|---|---|
| `POST /matters/:matterId/files/uploads` | filename, mediaType, sizeBytes, lowercase sha256, idempotencyKey → private 15-minute PUT URL and exact required headers |
| `POST /matters/:matterId/files/:fileId/finalize` | Returns 202; atomically queues a stable Temporal job |
| `GET /matters/:matterId/files/:fileId` | Owner-only processing state and safe error code |
| `GET /matters/:matterId/files/:fileId/download` | Current authorized document only; 60-second attachment URL |
| `GET /integration-jobs/:jobId` | Owner job status, requires integration.read |

Upload URLs are bearer credentials. Send precisely the signed headers/checksum, never log URLs, and do not persist them in analytics. Access is checked when issuing a URL; revocation cannot invalidate an already issued URL before its short expiry. Processing independently checks size/hash, malware result and parser provenance. Released object writes use `If-None-Match: *`; retries may reuse only byte-identical encrypted originals. This protects application writes, not privileged bucket administrators: configure S3 versioning/Object Lock and legal retention policy separately where required. Configure lifecycle expiration on quarantine and incomplete multipart uploads; runtime cleanup leases expire unused upload sessions. Failed processing may leave a private clean orphan; reconcile those against `file_ingestions` under the firm's retention policy, never delete current originals indiscriminately.

Limits: 10 MiB raw file, 200 PDF pages, 262,144 UTF-8 extracted bytes. PDFs require a text layer; no OCR, image/table/layout/annotation interpretation is claimed. DOCX includes body, headers, footers, footnotes and endnotes with part labels; no rendered page numbers are invented. Macro/embedded/ActiveX archives, external relationships, XML entities, tracked changes and unresolved fields are rejected. DOCX images/text boxes/visual layout may carry additional material; attorney inspection of the original remains required. PDF citations carry actual page numbers; every version binds the original SHA and immutable extracted passages. Indexing requires a clean scan proof and the current actor/matter grant.

Capabilities: `document.ingest`, `document.download`, `integration.read`, plus `matter.read` grants. Native text ingestion and deletion use their existing permissions.

## Clio Manage and additional providers

Set `CLIO_ENABLED=true`, registered `CLIO_CLIENT_ID`, secret, exact `CLIO_REDIRECT_URI`, `CLIO_REGION` (us/eu/ca/au) and the account's IANA `CLIO_TIME_ZONE`. URLs are pinned to official regional Clio Manage `/api/v4`, `/oauth/authorize` and `/oauth/token` endpoints. Optional `CLIO_API_VERSION` sends X-API-VERSION. This is the Clio Manage adapter, not the newer Clio Platform API.

Supply `INTEGRATION_TOKEN_KEYS` JSON or preferably a mounted `INTEGRATION_TOKEN_KEYS_FILE`: `{"active":"k1","keys":{"k1":"<base64 of 32 random bytes>"}}`. AES-256-GCM binds tokens to tenant, connection and provider. Retain old keys while rotating the active key, reauthorize/re-encrypt existing connections before removing old keys. Neither API responses nor events expose access/refresh tokens.

The generic REST adapter uses the same canonical matter/calendar/entry records. Configure its base/authorize/token URLs, OAuth client/secret/redirect and account IANA zone using `GENERIC_PROVIDER_*`. It is a documented adapter protocol, not automatic compatibility with every legal vendor. Resources are `/matters`, `/calendars`, `/calendar-entries`, wrapped in `{data:[...],meta:{paging:{next:"absolute same-resource URL"}}}`; IDs are string/integer, etag required. Matter fields: title/status; calendars: name; entries: calendar_id, optional matter_id, title/description, RFC3339 start_at/end_at, time_zone, all_day. Writes update `/matters/:id` with `{data:{title}}` and If-Match.

Connection routes: POST `/provider-connections` (providerId and idempotencyKey), POST `/:connectionId/oauth/start`, POST `/:connectionId/oauth/complete` (code/state), POST `/:connectionId/disconnect`, PUT `/:connectionId/grants/:principalId` (permissions, expectedRevision, idempotencyKey), POST `/:connectionId/sync` (idempotencyKey). See typed DTOs in the integration controller for exact bodies. The callback must be authenticated as the initiating subject; OAuth state is hashed, actor-bound, single-use and expires after ten minutes. An uncertain code exchange requires a new authorization flow. Local disconnect destroys stored credentials; revoke the application's grant in Clio too to invalidate upstream refresh tokens.

Sync imports matters and shared provider-owned calendars/entries through durable, bounded cursor pages. IDs stay stable, ETags avoid repeated changes, and native mutation endpoints refuse provider-owned calendars. Imported matters do not automatically grant staff access. Grant matter access administratively and calendar access through existing grant endpoints. Overlap on a shared firm calendar is allowed. Poll by requesting a new sync; no automatic periodic schedule is configured. Inspect job failures and calendar sync coverage before relying on freshness. Remote deletions, full recurrence reconciliation and webhooks are not implemented; a successful poll does not prove a complete calendar mirror. Validate real Clio calendar owner/type mappings and region/account permissions before enabling operational use.

Provider writes default off. When enabled, only existing matter-title changes are supported: POST `/provider-updates` (connectionId, matterId, title, idempotencyKey), GET `/provider-updates/:operationId` for full preview, POST `/:operationId/decision` (artifactSha256, approve/reject, idempotencyKey). A different current authorized reviewer approves the immutable artifact. Execution rechecks both principals, connection grants, native revision and ETag; sends one conditional PATCH; uncertain responses permit GET reconciliation only, never blind write retries. Unresolved outcomes remain `uncertain` for manual comparison. No calendar booking, create, delete or deadline writes are advertised.

Capabilities: `provider.manage/read/sync/write/approve`, explicit connection grants, and `matter.read` for updates; `integration.manage` for status and `integration.read` for owner job polling.

Official references: [authorization](https://docs.developers.clio.com/api-docs/clio-manage/authorization/), [regions](https://docs.developers.clio.com/handbook/getting-started/regions/), [ETags](https://docs.developers.clio.com/api-docs/clio-manage/etags/). Synthetic conformance tests are included; live provider validation requires future credentials.

## Configured legal generation and authoritative review

Set `LEGAL_AI_ENABLED=true`, approved HTTPS `LEGAL_AI_BASE_URL` ending e.g. `/v1`, exact `LEGAL_AI_MODEL`, operator-approved `LEGAL_AI_REVISION` and API key (or file). The adapter calls `/chat/completions` with `max_tokens`, one non-streaming choice and no tools. Select json_schema or json_object mode for a compatible provider. Model response identity must exactly match the configured model; choose an endpoint/model that preserves that identifier. Default output budget is 2048 tokens; prompt limit 32 KiB. Raise budgets deliberately alongside `CONTEXT_DOCUMENT_MAX_BYTES`; complete summaries fail instead of silently truncating oversized documents. Disabled mode keeps existing deterministic quote/abstention behavior.

Worker → context-engine derives actor/matter/input from the persisted execution, prepares an authorized versioned snapshot and binds an expiring grant to its input digest. Python receives typed trusted context separately from task input. Source and template text remain untrusted data. The model selects S1…Sn references; the server resolves IDs/page/SHA and checks exact supporting quotes. Invented quotes, tool calls, wrong profiles, extra fields and incomplete outputs are rejected. Quote support does not establish legal correctness or guarantee that every prose assertion follows from a quotation.

Requests use existing POST `/executions`:
- matter.qa.v1: `{question}`
- document.summarize.v1: `{documentId}`
- legal.draft.v1: `{instructions,templateId,documentId?}` with an approved template

POST `/legal/templates` accepts name/version/body/idempotencyKey; GET `/legal/templates/:templateId` previews it; POST `/:templateId/decision` approves/revokes its expectedSha256. `template.manage` creates, `template.approve` approves; a distinct person must approve. Bodies/hash are immutable, versions explicit.

Every real generated legal result enters awaiting_review, including Q&A and summaries. GET `/executions/:executionId/review` shows the full artifact, claims/limitations and canonical SHA. POST `/:executionId/review` accepts expectedSha256 and approve/reject. A distinct active `draft.approve` subject needs context.read and matter.read; the originating subject cannot self-approve. A durable Temporal timer expires unreviewed results after 24 hours. Approval checks current evidence/template permissions again. Changed/deleted sources or revoked grants suppress confidential/stale results even after prior approval. Nothing automatically sends legal advice or changes provider records.

## Operations and verification

Go runtime roles reject superuser/BYPASSRLS/table-owner membership; production DB URLs require sslmode=verify-full. Production Temporal requires TLS CA/server name and optional mTLS/API key. Go DB and provider/model secrets support file configuration as listed in `.env.example`; mount files only into the workload that needs them.

Workload identity requires both trusted certificate chains and exact CN/SPIFFE URI in `WORKLOAD_TRUST_DOMAIN`. Go rereads credentials on new handshakes, Node watches files every five seconds, Python reloads server bundles on new handshakes. Roll trust bundles with old+new CA, then atomic cert/key replacement, then remove old CA after connection draining. Existing connections need draining; this is not instantaneous revocation. Go RPC servers limit concurrency/deadlines and connection age. Restart long-running Temporal clients after rotating their TLS files.

OPS_ADDRESS exposes internal /livez, /readyz, /metrics (bounded RPC counters and certificate expiry; no matter/document labels). Never expose it publicly. SQL `system_queue_metrics()` returns aggregate outbox/consumer/integration/AI counts. Alert on dead letters, uncertain writes, old pending jobs, scanner failure and approaching certificate expiry. Domain event command types are isolated from ordinary publication/replay.

Back up PostgreSQL with a server-compatible pg_dump client and encryption; test restoration to an isolated database with a separate administrative identity, then verify RLS roles/extensions and application reads. Back up Temporal persistence, S3 originals/versions, KMS keys, OAuth keyring and Keycloak independently. Database backup alone cannot restore documents or workflow history. Use least-privilege egress: parser only receives integration-service RPC; AI only reaches its approved model/telemetry; integration reaches approved S3/KMS/scanner/provider/context; other workloads cannot reach provider credentials. Compose's parser network enforces local parser isolation; production CNI/firewall allowlists must be deployed by your operator.

Tests cover synthetic adapter OAuth/ETag/pagination and overlapping shared calendars, tenant-bound encryption, scanner protocol/failure, Rust malformed archives, processed page provenance, immutable legal grants, distinct approvals and stale-source suppression. No claim of live-provider conformance, legal correctness, OCR, complete bidirectional calendar synchronization or a production deployment is made.

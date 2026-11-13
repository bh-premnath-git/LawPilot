# Capability extensions and remaining validation

Migrations **016–018** extend the configured integrations from PR #11. Apply with the dedicated migration identity: `MIGRATION_DATABASE_URL=... node scripts/migrate.cjs`. Fresh Compose databases run all migrations automatically. Existing volumes need the migration command before restarting services. The previous [configuration guide](CONFIGURED_INTEGRATIONS.md) describes storage, credentials and the base review flow; this page supersedes its earlier limitations where stated below.

## Implemented scope

| Capability | New behavior | Boundary |
|---|---|---|
| Clio changes | Signed Manage webhooks queue durable, deduplicated sync hints | Notifications trigger authenticated re-fetches; they never directly mutate records |
| Missing records | Completed imports probe unseen remote IDs; 404/410 create reversible unavailable tombstones | 403, 429, server/network errors never prove deletion; nothing is physically deleted |
| Recurring calendars | Clio `expanded=true` imports individual occurrences in a stable per-job window, 30 days back through one year ahead; recurrence metadata survives mapping | This is a bounded snapshot, not infinite recurrence coverage. Provider exceptions depend on expanded responses and live account validation |
| Appointment writes | Distinct approval for single timed appointment creation and editing, with local revision/ETag checks | No recurring/series, all-day, court-rule, deadline, document or notification writes |
| OCR | Optional isolated Tesseract extraction of scanned PDF pages, with original SHA/page provenance | Approximate text recognition, not visual-layout or image interpretation; attorney inspection remains necessary |
| Firm knowledge | Explicit curated knowledge workspaces using immutable existing document versions/citations | Both knowledge.read and the workspace's matter.read/context.read grants remain required; unrelated client matters are never implicitly shared |
| Calendar context | Current authorized matter-linked events, revision-bound snapshot hash and coverage warnings | Private events remain creator-only; this is not complete firm availability or legal deadline verification |
| Operations | Queue metrics/alerts, cert-manager certificates, network-policy templates and opt-in read-only live conformance checks | Operators must configure actual issuers, ingress/egress, credentials and validate the target cluster/provider sandbox |

## Webhook registration

Set `PROVIDER_WEBHOOKS_ENABLED=true` and `PROVIDER_WEBHOOK_PUBLIC_URL=https://your-api.example/v1/provider-webhooks`. Keep the endpoint HTTPS, forward X-Hook-Secret/X-Hook-Signature headers, preserve request bytes, and never log callback URLs, payloads or signing secrets. The API captures raw webhook bytes before JSON interpretation. It calls the integration service over its existing mTLS identity; the callback itself uses an unguessable subscription capability and per-subscription HMAC rather than a browser login.

For an active Clio Manage connection, POST `/v1/tenants/:tenantId/provider-connections/:connectionId/webhooks` with `{model:"calendar_entry", idempotencyKey:"..."}`; register another subscription for `matter` as needed. The owner needs provider.manage/provider.sync capabilities and matching connection grants. The callback stores the encrypted handshake candidate without echoing it. After the authenticated creation response supplies the expected Clio webhook ID, POST `/v1/tenants/:tenantId/provider-webhooks/:subscriptionId/activate`. This uses Clio's delayed activation endpoint and verifies that the candidate ID matches the created subscription. If Clio has not delivered the handshake yet, activation returns a pending error; retry activation after delivery. A callback alone cannot activate a subscription or replace an active secret.

Delivery HMAC-SHA256 covers the exact raw body. A receipt hash deduplicates retries, and a leased, fenced dirty-generation queue coalesces notifications into Temporal polling jobs, including notifications arriving during a running import. Revoked sync grants block processing. Callback payloads are not stored: only their digest and routing metadata persist. Subscription secret envelopes use the existing tenant-bound keyring. Preserve old keys while rotating; reauthorize/recreate subscriptions deliberately before removing old keys. Local disconnect does not remove upstream subscriptions; disable them in Clio. Subscription renewal/expiry monitoring remains an operator responsibility; check Clio's actual expiry and delivery health.

Ambiguous webhook creation is marked uncertain and is never blindly repeated. Inspect/remove any upstream subscription before creating a replacement with a new idempotency key. Ordinary polling remains available during webhook interruption.

Official protocol references: [Clio Manage security and delayed activation](https://docs.developers.clio.com/clio-manage/api-reference/#section/Webhooks), [official OpenAPI specification](https://docs.developers.clio.com/openapi.json). This implementation targets Manage X-Hook-* headers, not the separate Platform API webhook protocol.

## Synchronization and recurring events

Each job persists its recurrence window before moving through matter, calendar and entry pages. Clio expands recurring rules; LawPilot does not reinterpret RRULEs in a different timezone. Instances retain their remote ID, recurrence metadata, account/entry timezone and ETag. Generic adapters expose canonical individual occurrences; they must handle recurrence expansion themselves. Recurring or court-rule records are refused by the write proposal endpoint.

After all pages succeed, unseen bindings are individually probed. Missing/inaccessible records become reversible tombstones; calendar entries are cancelled locally and calendar APIs suppress unavailable resources. Reappearance forces a fresh native revision even if the remote ETag is unchanged. Missing calendars suppress reads of their cached entries. A provider returning 404 for a permission-hidden resource is treated as unavailable, not proven permanently deleted. No absence-only bulk deletion occurs. A failed/partial import does not enter reconciliation.

The bounded Clio expansion window rolls on each new job; old occurrences may become unavailable outside it. Calendar sync status and list coverage state that results are provider snapshots. Validate expanded instance IDs, exception/cancellation behavior, timezone/DST and account permissions against your Clio sandbox before relying on a production calendar mirror.

## Reviewed appointment creation and editing

Set both `PROVIDER_WRITES_ENABLED=true` and `PROVIDER_CALENDAR_WRITES_ENABLED=true`. POST `/v1/tenants/:tenantId/provider-calendar-operations`:

```json
{"connectionId":"UUID","calendarId":"UUID","matterId":"UUID","operation":"create","title":"Reviewed appointment","description":"","startAt":"2026-12-01T10:00:00Z","endAt":"2026-12-01T11:00:00Z","timeZone":"Europe/London","idempotencyKey":"unique-command-key"}
```

For editing use operation update and an imported entryId. Durations must be positive and at most 24 hours. Source calendar and matter must belong to the selected connection; native calendars cannot be sent to an unrelated provider. The proposer needs provider.manage/write plus connection manage and calendar.write with explicit write grant. A different reviewer needs provider.approve, connection approve, calendar.approve, calendar.read and matching grants/matter access.

GET `/provider-calendar-operations/:operationId` returns the full immutable artifact and SHA; POST `/:operationId/decision` accepts artifactSha256, approve/reject and idempotencyKey. Execution rechecks both humans and native revisions. Updates send If-Match and refuse recurring/court-rule sources. Creates attach a lawpilot_operation external property in Clio. Unknown outcomes perform GET reconciliation only; a second POST/PATCH is not blindly issued. Unresolved results remain uncertain and need manual comparison. Confirmed changes queue an import; local calendars reflect the provider's subsequent canonical result, not an optimistic local write.

Generic REST adapters must accept the same appointment fields under data, return a resource ID, expose operation_id on the created canonical occurrence, and support external_property_name/value lookup. Their server must honor If-Match for updates. Only fields in the reviewed artifact are written; Clio send_email_notification is explicitly false. Existing global provider policies and per-account restrictions still apply.

## OCR and document provenance

Enable `DOCUMENT_OCR_MODE=empty-pages` to OCR pages without a text layer, or all-pages to rasterize every PDF page. Default off preserves text-only operation. `DOCUMENT_OCR_LANGUAGES=eng` selects installed Tesseract models; add approved trained data to your parser image for other languages. `DOCUMENT_OCR_MAX_PAGES` defaults to 10 and cannot exceed 20. The parser image includes English OCR, runs with bounded resources and no outbound network, and marks the extraction profile isolated-ocr.v1. It retains source SHA and actual PDF page numbers. Upload checksum/malware verification still runs first.

Raster size, text bytes, page count, subprocess tree lifetime and concurrency are bounded. Temporary files are private and removed by the parent RPC after success, error or cancellation; Docker uses a bounded tmpfs. OCR can misread names, numbers and legal language. Exact quotation verifies only the stored extracted text, not the correctness of OCR or legal reasoning. Generated artifacts always include this limitation. Empty-pages mode does not discover every embedded image on pages that already contain text. No visual-layout, handwriting or diagram comprehension is claimed.

## Firm knowledge and calendar recipes

Administratively create a dedicated knowledge workspace matter and grant only intended staff matter.read access. POST `/v1/tenants/:tenantId/knowledge-libraries` with matterId/name, using knowledge.manage and context.read. Ingest curated documents through the existing native/file flows. Designation grants no one additional access. Readers need knowledge.read, context.read and the explicit workspace grant; every retrieval, citation resolution, cached context and result verification rechecks permissions. Model use in that workspace still requires the relevant capability, input-bound evidence grant and attorney review.

Use existing POST `/matters/:matterId/context/build` with recipeId firm.knowledge.v1. matter.research.v1 retrieves up to 16 sources/32 KiB using the existing authorized lexical/hybrid pipeline; ordinary Q&A retains its smaller budget. Cross-matter document blending is not automatically enabled: explicitly retrieve from approved workspaces without removing client-matter isolation.

POST `/matters/:matterId/context/calendar` with RFC3339 from/to (maximum 93 days) returns currently authorized matter-linked events, revisions, timezone, authority, snapshotSha256 and warnings. Requires calendar.read/context.read, a matter grant and explicit calendar read grants. Private entries remain visible only to their creator. The snapshot hash changes with revisions; request fresh facts before decisions. These facts are available to callers as a structured context recipe; they are not automatically promoted to document citations or fed to the legal model. No legal deadline calculation is inferred.

## Production operations and live checks

Go gRPC servers/clients now propagate W3C trace context and export standard RPC spans when OTEL_EXPORTER_OTLP_ENDPOINT is configured (HTTPS required in production; the standard OTLP certificate/header settings apply). The default root sampling ratio is 10%, honoring parent sampling. Payloads and sensitive metadata are not trace attributes. Temporal activities remain their own trace roots; correlation across durable acceptance/history needs separate operator tracing conventions. The isolated parser has no export endpoint by default.

Queue-owning services expose aggregate queue states through system_queue_metrics, plus collection availability and certificate expiry. `infra/production/alerts.yaml` provides Prometheus rules for dead letters, uncertain writes, certificate expiry and failed metrics collection. Labels contain fixed states, not tenant/document identifiers. Bind OPS_ADDRESS internally and apply an authenticated monitoring/network boundary.

`infra/production/certificates.yaml` specifies separate 24-hour CN/SPIFFE certificates with automatic key rotation using an operator-provisioned lawpilot-workloads ClusterIssuer. `network-policies.yaml` denies traffic by default and permits only declared internal RPC edges and non-parser DNS. These are templates, not a complete deployment: match the exact workload labels, provision a NetworkPolicy-enforcing CNI, add approved ingress, metrics, PostgreSQL, Temporal, Keycloak, scanner, model, telemetry and provider/S3 egress policies before rollout. The parser has no egress allowance. Mount only each workload's own secret, use read-only roots, non-root users, dropped capabilities and no service-account token where unnecessary. Configure trusted CA bundles independently; cert-manager leaf rotation does not itself distribute workload trust. Test atomic secret rotation and drain existing connections.

Run `python scripts/verify_deployment.py` for local template consistency (PyYAML required). Cluster admission and network enforcement must be tested in the target environment. Existing CI builds non-root images, tests recovery, and restores a PostgreSQL backup.

An optional **read-only** sandbox check is `LIVE_PROVIDER_CONFORMANCE=true PROVIDER_SANDBOX_ACCESS_TOKEN_FILE=/run/secrets/token go test -run TestLiveReadOnlyProviderConformance` from services/integration-service, with the enabled provider configuration/keyring. It fetches one bounded page per supported resource type and validates canonical mapping without logging payloads or sending mutations. It is skipped in standard CI. This is not a live write/recurrence quality certification; use a separate approved sandbox exercise before enabling writes.

An approved model endpoint, real credentials, qualified legal-quality evaluation, issuer/cluster provisioning and provider sandbox access cannot be supplied or validated from repository code alone. Those remain explicit deployment gates; no approval requirement was removed.

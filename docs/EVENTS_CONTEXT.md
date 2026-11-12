# General events, secure retrieval and context

This release adds a PostgreSQL event journal/consumer system and a Go context service behind the NestJS gateway. Source ingestion is deliberately **native UTF-8 text only**. Retrieval and exact quotation are implemented; a model-generated legal answer, binary document ingestion, malware scanning and encrypted object storage are not claimed.

## Local setup

Apply missing migrations **009** (`events_context`) and **010** (`authoritative_evidence`) once, as database administrator, before deploying the matching API, workflow worker, context server and event process. Stop these writers during the coordinated upgrade. New databases apply all migrations through Compose.

```bash
docker compose stop platform-api execution-worker context-engine domain-events
docker compose exec -T postgres psql -U postgres -d lawpilot -v ON_ERROR_STOP=1 < db/009_events_context.sql
docker compose exec -T postgres psql -U postgres -d lawpilot -v ON_ERROR_STOP=1 < db/010_authoritative_evidence.sql
```

The context server requires its own `context-engine` certificate. The updated generator creates it alongside existing identities. For an existing dev CA directory, stop services, explicitly remove the **development** `work/tls` directory, regenerate all leaves with `bash scripts/generate_dev_tls.sh`, then recreate containers. Keep `LOCAL_UID`/`LOCAL_GID` equal to your host IDs so 0600 leaf keys remain readable. Each service mounts only its own key and public CA; the CA private key stays on the host.

```bash
docker compose --env-file .env up --build -d
```

| Variable | Host process | Compose |
|---|---|---|
| `CONTEXT_DATABASE_URL` | `postgresql://lawpilot_context:local-dev-context-only@localhost:5432/lawpilot` | Same role, host `postgres` |
| `EVENT_DATABASE_URL` | `postgresql://lawpilot_events:local-dev-events-only@localhost:5432/lawpilot` | Same role, host `postgres` |
| `CONTEXT_GRPC_ADDRESS` | API: `localhost:50054`; server: `:50054` | API: `context-engine:50054`; server defaults to `:50054` |
| `GRPC_CA_FILE` | Public CA | Dedicated read-only file mount |
| `GRPC_SERVER_CERT_FILE`, `GRPC_SERVER_KEY_FILE` | Context server's own leaf | `context-engine.pem/key` only |
| `KEYCLOAK_*`, `APP_ENV` | Same delegated issuer/JWKS/audience rules as calendar | Local identity URLs, `APP_ENV=development` |
| `EVENT_REGISTRY_PATH` | Defaults to `contracts/events/registry.v1.json` | Included in event image |
| `EMBEDDING_BASE_URL` | Optional approved OpenAI-compatible base URL, e.g. `https://provider.example/v1` | Forwarded from `.env` |
| `EMBEDDING_MODEL`, `EMBEDDING_REVISION` | Required when base URL is set | Pin an operator-managed model/revision profile |
| `EMBEDDING_API_KEY` | Optional provider credential | Forwarded only to context server |

All sample passwords are local development placeholders. The new roles cannot bypass tenant RLS. The event role has no document-text SELECT privilege. The legacy API role loses direct SELECT on the old document assets/passages scaffold; document reads go through the context boundary.

Leave the embedding base URL blank for explicitly reported **lexical** retrieval. When configured, the server requests 1536-dimensional float embeddings. It checks provider model, dimensions, indexes, finite values and nonzero norm, then normalizes for cosine retrieval; failures return unavailable and never fabricate vectors or quietly downgrade a configured hybrid request. Embedding profiles bind endpoint/model/revision/dimensions; mismatched profiles are never compared. The deployment operator must use a provider/model appropriate for the data and provision its credential through environment settings. No provider credential is committed.

Model revision pinning is an operator assertion; an upstream mutable model alias is not proof of immutable weights. Existing lexical-only documents need a new document version to acquire embeddings. There is no background reindex worker in this release. Exact vector scans operate on a materialized authorized candidate set; no approximate cross-tenant index is used.

## Event publication and consumption

Business writes commit their existing outbox row in the same transaction. `domain-events` validates registered schema/metadata and normalizes eligible outcomes into `domain_event_journal`; the **outbox row UUID** becomes the stable event ID. Publication and outbox acknowledgement commit together. A lost acknowledgement can be retried safely. Global journal-position allocation is serialized through commit so a reader's cursor cannot skip an earlier uncommitted row.

Temporal commands stay on their own existing consumers: `lawpilot.ai.accepted.v1`, `lawpilot.ai.cancelled.v1`, and calendar reminder schedule/cancel commands are excluded from general claims. General events never start a second workflow engine. Calendar proposal decisions now use revision 2 after pending revision 1.

The versioned registry is `contracts/events/registry.v1.json`; canonical envelope contracts are `contracts/proto/lawpilot/events/v1/events.proto` and `contracts/jsonschema/domain-event.v1.json`. Journal fields are event ID, monotonic position, tenant/type/schema, aggregate type/ID/revision, authoritative document matter scope, source, correlation/causation IDs and timestamps. Legacy envelopes without correlation/causation use the outbox event ID. No arbitrary data object, source text, title, participant roster, prompt or token is copied into the journal. Unknown types and privileged/unknown payload fields are quarantined.

Each publication fans out to two versioned local consumers:

- `context-invalidation.v1`: invalidates older matter context when a document/matter change arrives.
- `aggregate-heads.v1`: tracks monotonic aggregate revision/position metadata.

Delivery rows have leases, fencing tokens, retry counts, backoff and dead-letter state. Inbox insertion, revision check, consumer effect and acknowledgement share one database transaction. Duplicates cannot repeat effects; a delayed older revision is recorded as stale without regressing the aggregate head. Unversioned legacy AI outcomes use journal-position watermarks and never replace authoritative AI state. New subscribers require an explicit migration/handler; there is no arbitrary external webhook registration or broker in this release.

Document ingestion/deletion also invalidates context synchronously. Reads verify current source versions and grants independently, so event lag/outage cannot authorize stale content. Context created after an earlier event is not invalidated merely because that event's delivery was delayed.

Operations use metadata-only functions, as a database administrator:

```sql
SELECT * FROM domain_event_failures('YOUR_TENANT_UUID');
-- Correct the original schema/cause first; replay does not waive validation.
SELECT replay_domain_event('EVENT_UUID');
SELECT replay_domain_event('EVENT_UUID', 'context-invalidation.v1');
```

Only administrators can replay. The event role can inspect metadata failures. Retry callbacks never log raw SQL errors, event bodies or provider responses. Retention/archival, metrics/alerts, throughput benchmarks, additional subscriber handlers and external broker deployment remain operational work.

## Source and retrieval authority

Native text ingestion is capped at 24 KiB of UTF-8 (the gateway also caps total JSON at 32 KiB). It rejects NUL/invalid text and accepts no URL, MIME/scanner result, object-storage pointer or executable parser configuration from the caller. It labels ingestion `text_only`, **not malware-scanned**. No PDF/DOCX parser runs on this path.

A logical document has immutable versions, full-source SHA-256, a parser version, and chunks with exact UTF-8 byte ranges and passage hashes. Chunks use 512 Unicode code points with 64-code-point overlap. Existing legacy passages without a version/body are excluded from retrieval. A new version requires the expected document revision; commands are idempotent and writes are matter-serialized. Deletion is a tombstone: it immediately blocks retrieval/citations and invalidates snapshots; retained historical text is not a secure-erasure claim.

Both gateway and context server verify the user token; the server derives the actor from JWT rather than a client field. Reads require active membership, `context.read` and `matter.read`; ingestion/deletion require `document.ingest` / `document.delete` and the matter grant. Authorization runs before any source is sent to an embedding provider and again after provider I/O. Filters on tenant, matter, nondeleted document and current version precede lexical/vector ranking.

Lexical retrieval uses PostgreSQL English full-text search. Configured hybrid retrieval combines lexical and same-profile vector ranks with reciprocal rank fusion and deterministic UUID tie breaks. It never searches another matter to fill a sparse result set. No match yields an empty evidence package, not invented evidence. Hybrid can include lexical candidates lacking embeddings; its profile indicates which semantic vectors are eligible. Scores are retrieval relevance, not legal authority or truth.

The supported versioned recipes are in `contracts/context/recipes.v1.json`:

- `matter.qa.v1`: one authorized matter, at most eight sources and 16 KiB of source text.
- `document.summary.v1`: all chunks of one current authorized document, within 128 sources and 16 KiB; fails explicitly when complete coverage exceeds budget. This builds summary context; it does not generate a summary.

Budgets are bytes, not a tokenizer/model-specific token estimate. Approved firm-knowledge/template recipes, conversation/task context, calendar briefings, neural reranking and actual model-generation budgets remain pending. The fixed `lawpilot.context.policy.v1` boundary treats sources as untrusted data, grants them no tools/actions, and permits only exact-quote grounding. Retrieved text remains separate typed source fields even if it contains prompt-injection instructions.

A context snapshot binds tenant/principal/matter, recipe/version/budgets, query hash, embedding profile, ordered current citations/hashes, source text and trust metadata. Canonical hashing is independent of cache UUID/time. Equivalent authorized builds reuse an unexpired package. Every get rechecks caller ownership, current permissions, expiry, snapshot integrity, current versions, full-document/passage hashes and exact source ranges. Packages expire after five minutes and cannot become permanent permission grants.

## Citation and result delivery

The resolver verifies `(tenant, matter, document, version, passage, passage SHA-256, byte range)` against authoritative rows. An exact quote must exist in that current passage. Fabricated, cross-matter, stale, deleted and hash/range-mismatched references fail. Text containing an instruction remains evidence text and never becomes trusted policy.

A completed **matter-requiring** AI result is accepted only as `output.kind=extractive_quotes.v1` with 1–16 exact quote claims and matching hashed evidence references. Free-form legal prose is rejected; exact quotation proves source support, not legal interpretation or citation quality. Existing safe mocks still abstain, and no real LLM is enabled by these changes. Intake field-count output remains a separate non-evidence capability.

`verify_execution_result` runs inside the Go result-save transaction and locks current document and permission rows through commit. This closes the gap between an earlier RPC check and source mutation. The API reruns it before returning a saved completed matter result; stale/revoked evidence suppresses result content and reports `EVIDENCE_STALE_OR_UNAUTHORIZED`. The reference-only internal verification RPC accepts only an execution reference and derives actor/matter/capability from that stored execution; it is restricted to the execution-worker certificate. That RPC is not a replacement for the transactional save/delivery check.

## Public API

All paths below start `/v1/tenants/:tenantId/matters/:matterId`. Send the Keycloak bearer token; no caller-supplied principal is accepted.

| Route | Body / behavior |
|---|---|
| `POST /documents/text` | title, text, idempotencyKey; optional documentId and expectedRevision (0 for a new document) |
| `DELETE /documents/:documentId` | expectedRevision, idempotencyKey |
| `POST /context/build` | recipeId, query; documentId required for complete-document recipe |
| `GET /context/:packageId` | Reauthorized package; stale/expired snapshots fail |
| `POST /evidence/resolve` | citation with documentId/versionId/passageId/contentSha256/byteStart/byteEnd, plus exact quote |
| `GET /events?afterPosition=0&limit=50` | Current-matter document/matter invalidations, metadata only; cursor pagination |
| `GET /events/stream?afterPosition=0` | SSE invalidations with event-position IDs; use a bearer-capable fetch client |

The feed deliberately excludes calendar and other tenant-wide event IDs; calendar/private-attendee feed authorization requires its own domain handler. SSE rechecks JWT and SQL permissions each poll, emits `access-ended` and closes when reauthorization fails. It does not stream legal text or treat a previously opened connection as permanent authority. UI actions still fetch authorized current records through their owning API.

## Verification

CI checks protobuf lint/wire compatibility and generated Go/TS/Python drift; applies 009/010; runs real PostgreSQL tests for ACLs, current-version hashes, quote support, snapshot expiry/integrity, complete-document coverage, hybrid profile mismatches, event duplicate/reorder/lease/dead-letter behavior and transactional evidence/source-update races. `scripts/test_secure_context.cjs` starts actual API/context/event processes with synthetic JWKS and mTLS identities, exercises read/write/citation APIs, restarts the event worker, verifies saved-result redaction, and revokes an open SSE stream. No real embedding provider is contacted in tests.

Binary parsing/scanning, encrypted object storage, provider synchronization/reindex orchestration, generative legal evaluation and production identity/operations remain separate acceptance gates. This native text path does not authorize deployment with confidential client material by itself.

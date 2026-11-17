# Architecture decision record — v0.1

**Updated against the real running topology (verified from `contracts/proto/`, `compose.yaml` and the actual `Register*Server`/`*Client` call sites, not the original plan).** The product boundaries, canonical domain and invariants below were right from the start and remain so; the deployable-boundaries list, service-contract state and AI-pipeline status have been corrected to match what actually ships. See [`architecture.mmd`](architecture.mmd) for the current diagram.

## Product boundaries

LawPilot is an AI legal workspace supporting four MVP journeys: (1) intake summarization/routing recommendations, (2) document summaries with source references, (3) editable legal drafts from approved templates, (4) matter Q&A with verified citations. Clio Manage is the **first integration**. Other PMS and LawPilot Native are future adapters. Initial lawyer-facing AI outputs require attorney review where legally consequential.

```
API consumer -> platform-api (NestJS) -> Keycloak JWT verify + SQL grant checks
                     |                          |                    |
                     |                          +-> calendar-service +-> context-engine +-> integration-service
                     |
                     +-> accept + audit + outbox (same Postgres transaction) -> outbox-dispatcher
                                                                                      |
                                                                                 Temporal workflow
                                                                                      |
                                                                                execution-worker (activities)
                                                                                      |
                                                              +-----------------------+------------------------+
                                                              |                                                 |
                                                   context-engine.PrepareExecutionContext         root-supervisor (TS)
                                                                                                        |
                                                                                           ai-capabilities (Python, safe-mock by default)

integration-service -> document-parser (Go mTLS shim -> sandboxed Rust lawpilot-document-worker, profile: documents) -> context-engine.IngestProcessed
integration-service -> Clio Manage v4 / generic OAuth adapter (encrypted tokens, reviewed writes only)
```

### Distinguish request orchestration

The **Root Supervisor is the only coordinator of AI agent calls**; agent invocations are bounded, versioned and use allowlisted tools. It is NOT the authority for Clio writes, document access, business state or approvals. **Temporal/Go** coordinates business transactions, retries and human signals. A deterministic request does not have to consult an LLM.

### Deployable boundaries

1. `platform-api` — identity/authz enforcement, tenant routing and BFF; gRPC client of `calendar-service`/`context-engine`/`integration-service`; NO Clio secrets, NO direct Supervisor call (AI work is fully decoupled through Postgres + outbox + Temporal).
2. `root-supervisor` — registry and AI delegation; implements `SupervisorService`, calls `ai-capabilities` over `CapabilityService` with the identical request/response shape (same-shape pass-through, not a translation layer); NO DB credential to tenant records, NO direct provider write.
3. `ai-capabilities` — specialized Python inference workers; accepts already-authorized evidence (`PreparedContext`) via the Supervisor, never calls a provider directly; runs in `safe-mock` mode until `LEGAL_AI_ENABLED`/real credentials are supplied.
4. `calendar-service` — native calendar domain: tenant/matter ACLs, overlap policy, reminders, provider-calendar import coexistence.
5. `context-engine` — two binaries from one image: the `ContextService` gRPC server (versioned text ingestion, lexical/configured-vector retrieval, approved templates, legal review lifecycle) and `domain-events` (the durable domain-event consumer/inbox, run as its own compose service).
6. `integration-service` — Clio Manage + generic OAuth provider adapter, encrypted token storage (AES-256-GCM keyring), file quarantine/scan/index pipeline, provider calendar sync, webhook registration, and the only reviewed provider-write path (matter title only). This is the real current home of everything the original plan assigned to `provider-router`.
7. `document-parser` (compose profile `documents`) — a Go gRPC shim that sandbox-execs the Rust `lawpilot-document-worker` binary (bounded PDF/DOCX extraction, optional OCR) in its own process group with a scratch tmpdir and no outbound network; `integration-service` is its only caller.
8. `workflow-service` — two binaries: `execution-worker` (Temporal activities: reauthorize, call `context-engine` then `root-supervisor`, persist results) and `outbox-dispatcher` (leases outbox rows, starts stable-ID Temporal workflows).
9. `provider-router` — **legacy, not part of the default stack** (`profiles: [legacy-mock]` in `compose.yaml`). Superseded by `integration-service`; kept only for whatever still references the old mock path. Do not build new provider logic here.

**Early MVP:** fewer deployments are acceptable if contracts remain intact. Do not create microservices solely to use a particular language. Separate scaling/fault/isolation domains first.

### Canonical domain and ownership

Canonical resources: Firm, Principal, Matter, Document, DocumentVersion, Intake, Template, Draft, Approval, AgentRun, ProviderConnection, ExternalMapping, AuditEvent.

- LawPilot IDs are stable UUIDs independent of external IDs.
- The application owns tenant identities, access grants, knowledge templates, AI outputs, internal workflow state and audits.
- External PMS is authoritative for its owned records until a migration explicitly changes ownership.
- Never dual-write to two matter systems as authoritative sources without a reconciliation policy.
- Data tables share one PostgreSQL instance today, but each Go service already connects through its own database role (`lawpilot_calendar`, `lawpilot_context`, `lawpilot_events`, `lawpilot_integration`, `lawpilot_workflow`, `lawpilot_dispatcher`, `lawpilot_app`) rather than one shared superuser credential — the role split from the original plan is partially done, not merely a future intention. Full schema/database-per-service separation remains future work as ownership solidifies further.

### Service contracts and asynchronous work

- REST/OpenAPI at the web boundary; JSON Schema + Zod/Pydantic for capability tasks; **Protobuf/gRPC with per-workload mTLS is implemented** for every inter-service call (`platform-api`↔`calendar-service`/`context-engine`/`integration-service`, `execution-worker`↔`context-engine`/`root-supervisor`, `root-supervisor`↔`ai-capabilities`, `integration-service`↔`document-parser`) — this is no longer a future item. See [CONTRACTS.md](CONTRACTS.md) for the evolution rules and `buf breaking` CI gate, and the proto sources under `contracts/proto/lawpilot/`.
- Each request carries a non-user-controlled execution correlation ID, verified actor/tenant context, and optional idempotency ID.
- For cross-service async business operations use Temporal; for independent broadcasts use an outbox + versioned event envelope (`contracts/events/registry.v1.json`, consumed by `domain-events`); do not use NATS and Temporal for the same workflow state.
- Use retries with deadlines and backoff, BUT uncertain external writes require reconciliation before retry.
- Provider adapter must advertise supported capabilities; unsupported is an explicit typed result.
- Three proto services are defined with generated code but have zero current client or server (`MatterService`, `AgentExecutor`, `MatterProvider`) — reserved contracts, not implemented paths. Don't assume they're wired to anything without checking the actual `Register*Server`/`*Client` call sites first.

### Pipeline for source-grounded AI

Clio/tenant upload -> authorized ingestion -> virus scan (ClamAV) -> sandboxed Rust/Go `document-parser` -> `context-engine.IngestProcessed` (document version and page mapping, immutable source hashes) -> tenant/matter-scoped chunks -> **lexical retrieval by default; pgvector hybrid retrieval when `EMBEDDING_*` credentials are configured** -> passage ACL check -> `PreparedContext` evidence bundle handed to `execution-worker` -> `ai-capabilities` response with exact-quote evidence references -> mandatory distinct-attorney review (`awaiting_review` until approved). Revocation is re-checked on each retrieval and answer serve. This pipeline is implemented end-to-end behind disabled-by-default configuration, not aspirational — see [CONFIGURED_INTEGRATIONS.md](CONFIGURED_INTEGRATIONS.md) and [EVENTS_CONTEXT.md](EVENTS_CONTEXT.md).

### Target running modes

- `mock`/`safe-mock` AI capability mode (local safe demonstration, the current default)
- `clio-manage` provider — connector implemented in `integration-service`; real OAuth app registration and live account validation remain an operator step, not a code gap
- `lawpilot-native` provider (future product)
- `custom` provider SDK via the `GENERIC_PROVIDER_*` adapter (documented protocol, not automatic compatibility with every vendor)

### Architecture invariants

- Tenant context frozen per request. Never trust tenant IDs passed from agent output.
- No generic `if provider == clio` inside core domain; all differences in adapters.
- Compatibility verified at CI via schema changes and conformance fixtures (`buf breaking`, `scripts/verify_structure.py`).
- Keys, OAuth tokens and legal document contents never appear in prompts, audit payloads or telemetry.
- Supervisor never calls unrestricted code execution or arbitrary external URLs; the `ExecuteCapabilityRequest`/`Response` contract itself carries no provider-write/action field, so an agent cannot request an effect through this channel even if it tried.

## Implemented durable mock command path

The API now atomically accepts execution + audit + outbox, returning 202 and an owner-authorized status URL. A leased/fenced dispatcher starts a stable-ID Temporal workflow; Go activities load and reauthorize the stored request, fetch a `PreparedContext` from `context-engine`, delegate to the existing Root Supervisor, and persist validated outcomes with audit/outbox metadata. Cancellation and dead-letter requeue preserve operation identity. Local Temporal history persists via SQLite (`temporal server start-dev --db-filename`), written to the named `temporaldata` volume rather than ephemeral storage.

Raw inputs/results remain in RLS-protected PostgreSQL records; Temporal histories/outbox commands carry references only. The API holds no internal HMAC secret. Every inter-service hop listed above uses generated gRPC contracts with workload mTLS; Calendar also verifies delegated Keycloak tokens. See [calendar/gRPC boundaries](CALENDAR_GRPC.md); production certificate lifecycle and Temporal identity remain pending. No provider mutations beyond the single reviewed matter-title update are enabled. Provider, evidence, context, calendar and general-event work follow [the uploaded remediation roadmap](REMEDIATION_STATUS.md).

# Architecture decision record — v0.1

## Product boundaries

LawPilot is an AI legal workspace supporting four MVP journeys: (1) intake summarization/routing recommendations, (2) document summaries with source references, (3) editable legal drafts from approved templates, (4) matter Q&A with verified citations. Clio Manage is the **first integration**. Other PMS and LawPilot Native are future adapters. Initial lawyer-facing AI outputs require attorney review where legally consequential.

```
Browser/Next.js -> NestJS BFF -> firm membership + matter access -> Temporal workflow (Go)
                                                      |                  |
                                                      |                  +-> typed domain API -> provider router -> Clio/mock/native
                                                      +-> Root Supervisor (TS) -> AI capabilities (Python)
                                                                                  -> evidence index (future)
                                      Rust file worker <- authorized processing workflow
```

### Distinguish request orchestration

The **Root Supervisor is the only coordinator of AI agent calls**; agent invocations are bounded, versioned and use allowlisted tools. It is NOT the authority for Clio writes, document access, business state or approvals. **Temporal/Go** coordinates business transactions, retries and human signals. A deterministic request does not have to consult an LLM.

### Deployable boundaries

1. `platform-api` — identity/authz enforcement, tenant routing and BFF; NO Clio secrets.
2. `root-supervisor` — registry and AI delegation; NO DB credential to tenant records, NO direct provider write.
3. `ai-capabilities` — specialized Python inference workers; accepts already authorized evidence, never calls provider directly.
4. `provider-router` — provider capability negotiation + adapter contract; Clio and mock adapters, secrets only within connector service.
5. `workflow-service` — Go Temporal workers for persistent approval and provider operations.
6. `document-worker` — Rust isolated document preprocessing with safe parsing limits.

**Early MVP:** fewer deployments are acceptable if contracts remain intact. Do not create microservices solely to use a particular language. Separate scaling/fault/isolation domains first.

### Canonical domain and ownership

Canonical resources: Firm, Principal, Matter, Document, DocumentVersion, Intake, Template, Draft, Approval, AgentRun, ProviderConnection, ExternalMapping, AuditEvent.

- LawPilot IDs are stable UUIDs independent of external IDs.
- The application owns tenant identities, access grants, knowledge templates, AI outputs, internal workflow state and audits.
- External PMS is authoritative for its owned records until a migration explicitly changes ownership.
- Never dual-write to two matter systems as authoritative sources without a reconciliation policy.
- Data tables in SQL are a **starting schema**, not permission to give every service shared write access. Split schemas/databases by service as ownership solidifies.

### Service contracts and asynchronous work

- REST/OpenAPI at web boundary; JSON Schema + Zod/Pydantic for capability tasks; versioned Protobuf for future gRPC service APIs.
- Each request carries a non-user-controlled execution correlation ID, verified actor/tenant context, and optional idempotency ID.
- For cross-service async business operations use Temporal; for independent broadcasts use an outbox + versioned event envelope; do not use NATS and Temporal for the same workflow state.
- Use retries with deadlines and backoff, BUT uncertain external writes require reconciliation before retry.
- Provider adapter must advertise supported capabilities; unsupported is an explicit typed result.

### Pipeline for source-grounded AI (not implemented)

Clio/tenant upload -> authorized ingestion -> virus scan -> Rust/Python parse -> document version and page mapping -> tenant/matter-scoped chunks -> embeddings + hybrid search (pgvector extension enabled; retrieval pending) -> passage ACL check -> Agent evidence bundle -> validated response with supporting passage references -> lawyer review. Revocation requires re-check on each retrieval and answer serve.

### Target running modes

- `mock` provider (local safe demonstration)
- `clio-manage` provider (connector implementation and OAuth needed)
- `lawpilot-native` provider (future product)
- `custom` provider SDK (future with explicit vetting / sandbox)

### Architecture invariants

- Tenant context frozen per request. Never trust tenant IDs passed from agent output.
- No generic `if provider == clio` inside core domain; all differences in adapters.
- Compatibility verified at CI via schema changes and conformance fixtures.
- Keys, OAuth tokens and legal document contents never appear in prompts, audit payloads or telemetry.
- Supervisor never calls unrestricted code execution or arbitrary external URLs.

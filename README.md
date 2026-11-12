# LawPilot Backend Starter v0.1

**Contract-first, multi-provider, AI-assisted legal platform foundation.** Clio Manage is **the first adapter**, not the LawPilot domain model. This is a **starter framework**, not a commercially deployable legal application.

## Implemented versus deliberately not implemented

| Component | State | What exists |
|---|---|---|
| NestJS platform API | Starter implementation | Keycloak JWT validation, tenant membership and matter-grant checks, AI execution endpoint, health check |
| TypeScript Root Supervisor | Runnable starter | Signed internal API, versioned capability discovery and bounded delegation to Python |
| Python AI capability service | Runnable **safe mock** | Four registered capabilities, schema-checked requests, explicit abstention for unsupported evidence |
| Go provider router | Runnable starter | Capability discovery, mock legal provider and explicit non-implemented Clio adapter |
| PostgreSQL + pgvector | PostgreSQL 18 / pgvector 0.8.7 | Canonical records, permissions, audit/outbox, tenant RLS and vector similarity primitives |
| Keycloak | Dev infrastructure | Importable demo realm, OIDC/JWKS boundary; no production users provisioned |
| Go Temporal worker | Integration scaffold | Example approval workflow with explicit lawyer signal; separate startup |
| Rust document worker | Source scaffold | SHA-256/metadata processor; **no legal PDF extraction** |
| OpenTelemetry | AI request-path instrumentation | OTLP export and W3C traces across API, Supervisor and Python; Go/Temporal/Rust instrumentation pending |
| Clio OAuth + webhooks | **NOT IMPLEMENTED** | Exact API boundaries and handshake/security checklist, no credentials or pretend success |
| RAG, legal drafting, real LLM | **NOT IMPLEMENTED** | Capability contracts + safe mock only; no invented citations or drafts |

## Backend layers and implementation order

| Layer | Technology | Responsibility |
|---|---|---|
| API Gateway | NestJS / TypeScript | Authentication, request validation and tenant context |
| AI Orchestration | TypeScript | Root Supervisor, agent registry and delegation |
| AI Capabilities | Python | Specialist agents with structured input/output |
| Business Workflows | Go + Temporal | Durable execution, retries and approval lifecycles |
| Provider Integration | Go | Canonical interfaces, capability registry and adapters |
| Document Processing | Rust | Isolated document-processing workers |
| Identity | Keycloak | OIDC, organizations and authorization integration |
| Persistence | PostgreSQL | Tenant-owned records, permissions, workflow and audit data |
| Observability | OpenTelemetry | Cross-service tracing and operational telemetry |

See [layer status and upgrade steps](docs/LAYER_STATUS.md) for the fixes, tests and remaining work at each boundary. These responsibilities describe the target architecture; the implementation table above identifies current starter functionality.

## Architecture rules

1. **Only trusted domain/integration services mutate provider records.** The Supervisor proposes, never writes to Clio.
2. All public requests require verified Keycloak OIDC identity. Tenant ID is checked against **server-side membership**, not trusted from the client.
3. All matter requests require a **matter grant**. A tenant role alone is not equivalent to permission on every matter.
4. Provider-neutral IDs are owned by LawPilot; `external_mappings` tracks provider IDs.
5. New capability = registered schema + implementation + conformance test + permission + release review.
6. Durable business writes and approvals belong in Temporal-backed Go workflows, **not LLM reasoning**.
7. Evidence-based answers must cite authorized, versioned source passages. The mock explicitly abstains without evidence.
8. LawPilot audit events are never replaced by potentially sampled operational traces.

## Local start (Docker required)

Prerequisites: Git, Docker Engine/Desktop with Compose v2, and OpenSSL. Node.js 22+ is required only when running or testing the TypeScript services on your host.

```bash
git clone https://github.com/bh-premnath-git/LawPilot.git
cd LawPilot
cp .env.example .env
openssl rand -hex 32
```

Paste the generated value into `INTERNAL_HMAC_KEY` in `.env`, then start:

```bash
docker compose --env-file .env up --build -d
docker compose --env-file .env ps
curl -fsS http://localhost:3000/v1/healthz
curl -fsS http://localhost:3001/healthz
curl -fsS http://localhost:8000/healthz
curl -fsS http://localhost:8082/healthz
python3 scripts/verify_structure.py
```

Keycloak may take longer to initialize on the first start. Inspect its logs with `docker compose --env-file .env logs --tail=100 keycloak`. A healthy API does not mean a user has permission to access tenant data.

### PostgreSQL and pgvector

Docker and CI use `pgvector/pgvector:0.8.7-pg18-trixie`: the latest supported stable PostgreSQL major (18) with pgvector 0.8.7. The baseline is PostgreSQL 18.6; PostgreSQL 19 is still a release candidate. The pgvector release fixes an IVFFlat buffer-overflow vulnerability. Image tags may receive PostgreSQL 18 minor updates; the SQL smoke test checks the supported baseline and extension version.

Fresh initialization enables `pgcrypto` and `vector`. PostgreSQL 18 stores its cluster under `/var/lib/postgresql/18/docker`, so Compose mounts the new `pgdata18` volume at `/var/lib/postgresql`.

This change deliberately starts a **fresh database**. Existing PostgreSQL 16 data is not migrated. Start the updated stack normally, then seed synthetic tenant membership and grants again. Keycloak retains its separate `keycloakdata` volume. The old PostgreSQL 16 volume can be removed after its container has been replaced; do not use `docker compose down -v` if you want to keep Keycloak users.

Check the installed versions and vector operations:

```bash
docker compose --env-file .env exec -T postgres psql -U postgres -d lawpilot -c "SELECT version(); SELECT extversion FROM pg_extension WHERE extname='vector';"
docker compose --env-file .env exec -T postgres psql -U postgres -d lawpilot -v ON_ERROR_STOP=1 < db/tests/pgvector.sql
```

pgvector is enabled as infrastructure. Embedding generation, matter-scoped retrieval and verified citation/RAG workflows are still pending; no model-specific vector columns are created by this change.

### Local environment variables

The complete template is [`.env.example`](.env.example). The following values match the local development topology:

| Variable | API running on your host | API running in Compose |
|---|---|---|
| `KEYCLOAK_ISSUER` | `http://localhost:8080/realms/lawpilot-dev` | Same browser-visible issuer |
| `KEYCLOAK_JWKS_URL` | `http://localhost:8080/realms/lawpilot-dev/protocol/openid-connect/certs` | `http://keycloak:8080/realms/lawpilot-dev/protocol/openid-connect/certs` |
| `KEYCLOAK_AUDIENCE` | `lawpilot-api` | `lawpilot-api` |
| `DATABASE_URL` | `postgresql://lawpilot_app:local-dev-app-only@localhost:5432/lawpilot` | `postgresql://lawpilot_app:local-dev-app-only@postgres:5432/lawpilot` |
| `SUPERVISOR_URL` | `http://localhost:3001` | `http://root-supervisor:3001` |
| `AI_URL` (supervisor process) | `http://localhost:8000` | `http://ai-capabilities:8000` |
| `INTERNAL_HMAC_KEY` | Your generated secret, at least 32 characters | Same secret across API, supervisor, AI worker and provider router |

`NODE_ENV=development` is the local setting. `API_PORT=3000` and `SUPERVISOR_PORT=3001` are the default service ports.

For a host-run API, load the variables into its process environment; NestJS does not automatically load `.env`. Compose sets container URLs in [compose.yaml](compose.yaml); `--env-file .env` supplies interpolation values. The template's `*_DOCKER` variables document the container addresses and are not automatically substituted for the corresponding host variables.

### Keycloak 26.8.2 setup

The image is pinned to `quay.io/keycloak/keycloak:26.8.2`, from the [official Keycloak distribution](https://www.keycloak.org/downloads).

1. Open `http://localhost:8080` and sign in to the administration console. The local defaults are username `admin` and password `local-dev-keycloak-only`, configurable through `KEYCLOAK_ADMIN_USERNAME` and `KEYCLOAK_ADMIN_PASSWORD` before the first start.
2. Select the imported `lawpilot-dev` realm, then create a user under **Users**. Set a password under that user's **Credentials** tab. Use synthetic test data.
3. Copy the user's ID. This must match the access token's `sub` claim and the `principal_id` used in the SQL grants below.
4. Use an OAuth/OIDC client tool that supports a local callback and **Authorization Code + PKCE (S256)** to obtain an access token with these settings:

| Setting | Value |
|---|---|
| Client ID | `lawpilot-web` (public client; no client secret) |
| Authorization endpoint | `http://localhost:8080/realms/lawpilot-dev/protocol/openid-connect/auth` |
| Token endpoint | `http://localhost:8080/realms/lawpilot-dev/protocol/openid-connect/token` |
| Scope | `openid` |
| PKCE method | `S256` |
| Redirect URI | A callback served by your tool under `http://localhost:3000/*` or `http://localhost:3003/*` |

The callback URI sent during authorization and code exchange must match exactly. This repository has no browser frontend or implemented login callback; your OAuth tool must handle the callback. External tool callback URLs require an explicit client redirect configuration in Keycloak.

The `lawpilot-web` audience mapper includes `lawpilot-api` in access tokens. Password/direct-access grants are disabled. Use the **access token**, not the ID token or refresh token. The API verifies RS256 signatures, exact issuer/audience, `sub`, `exp`, `iat`, and `typ=Bearer`. Missing issuer/audience fails startup; production issuer and JWKS URLs must use HTTPS.

The browser-visible issuer remains `localhost` even though Compose fetches signing keys through `keycloak`. Do not change the issuer to the container hostname or disable validation to resolve a mismatch.

### Grant tenant and matter access

Keycloak authenticates the user. LawPilot authorizes requests using database membership, capability grants and matter grants. Keycloak realm roles, including `firm_admin`, do not bypass these checks. **No users are automatically granted access.**

Replace every `REPLACE_WITH_ACTUAL_KEYCLOAK_SUB` in a local copy of [scripts/seed_local_tenant.sql](scripts/seed_local_tenant.sql) with your user's ID, then apply that copy using the database administrator:

```bash
# Save your edited copy outside the repository, for example /tmp/lawpilot-seed.sql.
docker compose --env-file .env exec -T postgres psql -U postgres -d lawpilot -v ON_ERROR_STOP=1 < /tmp/lawpilot-seed.sql
```

The seed creates synthetic firm `11111111-1111-4111-8111-111111111111`, matter `22222222-2222-4222-8222-222222222222`, active membership, all four capability grants, and `matter.read`. It does not create a Keycloak user.

### Call protected endpoints

In Bash, enter the access token without adding it to shell history:

```bash
read -r -s -p 'Access token: ' LAWPILOT_ACCESS_TOKEN
printf '\n'
export LAWPILOT_ACCESS_TOKEN

curl -fsS \
  -H "Authorization: Bearer $LAWPILOT_ACCESS_TOKEN" \
  http://localhost:3000/v1/tenants/11111111-1111-4111-8111-111111111111/matters/22222222-2222-4222-8222-222222222222

curl -fsS \
  -H "Authorization: Bearer $LAWPILOT_ACCESS_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"capabilityId":"intake.summarize.v1","idempotencyKey":"local-intake-test-001","input":{"answers":{"issue":"Synthetic intake example"}}}' \
  http://localhost:3000/v1/tenants/11111111-1111-4111-8111-111111111111/ai/execute

unset LAWPILOT_ACCESS_TOKEN
```

The matter request returns the synthetic matter. Intake returns a mock field count, not an AI legal summary. Other capabilities abstain because evidence/templates are not wired. Reusing the same idempotency key and body returns the existing execution; use a new key for a new request. Reusing a key for a different body returns `409`.

The AI execution endpoint is a synchronous demonstration path. It checks permissions before dispatch and again after the worker returns; a revoked grant blocks result delivery. Production durability and asynchronous processing require the Temporal command/outbox integration.

### Troubleshooting and persistence

| Symptom | What to check |
|---|---|
| `401` | Access token rather than ID/refresh token; expiry, issuer, audience, signing keys, and machine clock |
| `403` | Active membership for the token's `sub`, capability grant, and `matter.read` grant when a matter is supplied |
| `400` or `422` | Request schema, UUIDs, supported capability ID, and required matter ID |
| `503` from AI execution | Supervisor/AI logs, matching HMAC secrets, downstream reachability and response validation |
| Keycloak changes are missing | Startup import only creates a missing realm; it does not update an existing realm |
| Database tables are missing after a failed first start | Inspect PostgreSQL initialization logs and ensure `db/001_init.sql` is readable by the container user; an existing database volume does not rerun initialization scripts |

Keycloak data persists in `keycloakdata`; PostgreSQL 18 data persists in `pgdata18`. `docker compose --env-file .env down` preserves both volumes. Apply changes to an existing realm through the admin console or an explicit migration. Bootstrap admin environment values are for initial creation; changing them does not reset an existing administrator's password. Removing volumes deletes local users and application data.

### Upgrading an existing local installation

The PostgreSQL 18 change uses a fresh `pgdata18` volume; PostgreSQL 16 data is not migrated. For an existing PostgreSQL 18 database, apply [the database migrations](docs/LAYER_STATUS.md#upgrade-an-existing-local-database). Existing Keycloak realms still require the organization setting described there.

### Request tracing

Compose configures `OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318` for the API, supervisor and Python worker. On your host, use `http://localhost:4318` with a running collector; unset it to disable export. These services propagate W3C trace context and export static operation names and HTTP response status without request bodies, legal text or tokens. Audit events are stored separately. Go, Temporal and Rust instrumentation remains future work.

## Validate without Docker

```bash
python3 scripts/verify_structure.py
cd services/ai-capabilities && python3 -m pip install -r requirements.txt && python3 -m pytest -q
cd ../provider-router && go test ./...
# From project root, with npm registry access:
npm ci && npm run build && npm test
```

## Document map

- [Layer status](docs/LAYER_STATUS.md): ordered fixes, migrations, verification and remaining integrations
- [Architecture](docs/ARCHITECTURE.md): ownership, request flow, service isolation
- [Implementation roadmap](docs/ROADMAP.md): milestones and release gates
- [Security and threat model](docs/SECURITY.md): tenancy, Clio, AI, audit
- [Provider contracts](docs/PROVIDERS.md): capability negotiation and migration
- [Contract policy](docs/CONTRACTS.md): versioning and compatibility
- [Operational checklist](docs/OPERATIONS.md): traces, health, failure handling
- [Open decisions](docs/DECISIONS.md): approvals still required from owner

## Sources

- Clio Manage v4: https://docs.developers.clio.com/clio-manage/api-reference/
- Clio Regions: https://docs.developers.clio.com/handbook/getting-started/regions/
- Clio API limits: https://docs.developers.clio.com/api-docs/clio-manage/rate-limits/
- Temporal Go SDK: https://docs.temporal.io/develop/go
- Keycloak Authorization Services: https://www.keycloak.org/docs/latest/authorization_services/

Never use real privileged documents, client files, or live Clio secrets in local seed data.

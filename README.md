# LawPilot Backend Starter v0.1

**Contract-first, multi-provider, AI-assisted legal platform foundation.** Clio Manage is **the first adapter**, not the LawPilot domain model. This is a **starter framework**, not a commercially deployable legal application.

## Implemented versus deliberately not implemented

| Component | State | What exists |
|---|---|---|
| NestJS platform API | Starter implementation | Keycloak JWT validation, tenant membership and matter-grant checks, AI execution endpoint, health check |
| TypeScript Root Supervisor | Runnable starter | Signed internal API, versioned capability discovery and bounded delegation to Python |
| Python AI capability service | Runnable **safe mock** | Four registered capabilities, schema-checked requests, explicit abstention for unsupported evidence |
| Go provider router | Runnable starter | Capability discovery, mock legal provider and explicit non-implemented Clio adapter |
| PostgreSQL | Migration provided | Canonical matters, grants, provider mappings, AI execution, audit, outbox, tenant RLS |
| Keycloak | Dev infrastructure | Importable demo realm, OIDC/JWKS boundary; no production users provisioned |
| Go Temporal worker | Integration scaffold | Example approval workflow with explicit lawyer signal; separate startup |
| Rust document worker | Source scaffold | SHA-256/metadata processor; **no legal PDF extraction** |
| OpenTelemetry | Local collector skeleton | Collector and trace conventions; full SDK instrumentation still required |
| Clio OAuth + webhooks | **NOT IMPLEMENTED** | Exact API boundaries and handshake/security checklist, no credentials or pretend success |
| RAG, legal drafting, real LLM | **NOT IMPLEMENTED** | Capability contracts + safe mock only; no invented citations or drafts |

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

```bash
cp .env.example .env
# Change the LOCAL internal HMAC key. Do not use any of these sample passwords in production.
docker compose --env-file .env up --build -d
curl http://localhost:3000/v1/healthz
curl http://localhost:3001/healthz
curl http://localhost:8000/healthz
curl http://localhost:8082/healthz
python3 scripts/verify_structure.py
```

### Authentication and demo access

Copy `.env.example` to `.env` for the local settings used in validation. For a host-run API, load `DATABASE_URL`, `KEYCLOAK_ISSUER`, `KEYCLOAK_JWKS_URL`, `KEYCLOAK_AUDIENCE`, `SUPERVISOR_URL`, and your generated `INTERNAL_HMAC_KEY` into its process environment. NestJS does not automatically load `.env`. Docker Compose sets the API container values in `compose.yaml`, including internal database/JWKS hostnames; `--env-file` supplies Compose interpolation values.

The dev Keycloak image is pinned to `quay.io/keycloak/keycloak:26.8.2` (see [official downloads](https://www.keycloak.org/downloads)). Its local database persists in the `keycloakdata` volume. Realm import creates a missing realm only; editing the import file does not update an existing realm. Apply client changes through the admin console or an explicit realm migration.

The dev Keycloak service is at `http://localhost:8080` (admin login from `.env`). Realm: `lawpilot-dev`. **No test users are automatically granted access**, and all tenant APIs deny by default. To use protected endpoints:

1. Create a Keycloak user in the dev realm and obtain an OIDC access token using the `lawpilot-web` public client through the **Authorization Code + PKCE (S256)** flow. Its configured audience mapper includes `lawpilot-api` in access tokens; audience identifies the API, not a permission grant. Never obtain production browser tokens using password grant.
2. Insert a matching tenant membership and explicit capability/matter grants using `scripts/seed_local_tenant.sql`, after replacing its example placeholders with real values.
3. Call `GET /v1/tenants/{tenantId}/matters/{matterId}` or `POST /v1/tenants/{tenantId}/ai/execute` with `Authorization: Bearer <token>`.
4. The AI execution endpoint is a **synchronous demonstration path**; use the Temporal command/outbox flow for production durability and asynchronous processing.

The API requires signed RS256 Keycloak access tokens (`typ=Bearer`) with subject, expiration, issued-at, the exact configured issuer, and the `lawpilot-api` audience. ID and refresh tokens are rejected. Membership, capability grants and matter grants remain server-side database decisions; Keycloak realm roles do not bypass them. Missing issuer/audience configuration fails startup. In production both issuer and JWKS URLs must use HTTPS.

**Known local setup note:** the browser issuer URL is `localhost`, while containerized services reach Keycloak through the internal `keycloak` hostname for JWKS. A real deployment must pin and verify the issuer, audience and network DNS configuration rather than disabling validation.

## Validate without Docker

```bash
python3 scripts/verify_structure.py
cd services/ai-capabilities && python3 -m pip install -r requirements.txt && python3 -m pytest -q
cd ../provider-router && go test ./...
# From project root, with npm registry access:
npm install && npm run build && npm test
```

## Document map

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

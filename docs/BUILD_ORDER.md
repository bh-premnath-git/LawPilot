# Concrete backend build order / definition of done

## Before code freeze

**Decision gate:** owner chooses lawyer-facing AI versus client-facing release, practice area, original document-chasing features versus four AI MVP features, and whether Clio Grow is needed.

## Sprint A — identity and tenancy

- Keycloak realm/clients: login and organization selection, OIDC audience, PKCE, MFA policy.
- Provision tenant memberships from Keycloak org identity via trusted provisioning; do not trust browser-selected tenant without membership check.
- Grant matter scopes explicitly; prove cross-tenant, cross-matter denied.
- Integrate Keycloak native Authorization Services if chosen as PDP; policy errors deny by default.
- PostgreSQL RLS and per-service roles; migrations and tenant-data purge policy.

**Acceptance:** a user's valid JWT cannot read a different tenant's matter; copied tenant ID / matter ID doesn't grant access.

## Sprint B — provider neutrality and Clio connector

- Canonical Matter/Contact/Document/Version models and ownership mappings.
- Provider registry with capability contracts; mock provider passes the same conformance suite as Clio sandbox.
- Clio OAuth auth code + refresh with vault-backed secrets, regions, scopes, audit.
- Contacts, matters and documents; pagination/ETag, webhook handshake, signature, renewal, and reconciliation.

**Acceptance:** switching mock and real provider doesn't modify AI/Workflow types. Unsupported capability returns typed error.

## Sprint C — durable workflow services

- Go Temporal workers, task queues, domain activities and tenant-aware workflow IDs.
- Document ingestion, approval signals and user-visible state; authoritative verification of approval at time of action.
- Idempotency + outbox + reconcile unknown external writes.

**Acceptance:** kill workers mid-run, restart, no duplicate sends/Clio mutation; stale approvals rejected.

## Sprint D — AI Supervisor and agents

- Typed capabilities and schema registry; bounded calls, versions, safe errors, policy allowlisted tools.
- Intake summaries with fact labels; document summaries with page references; approved-template drafts; cited matter Q&A.
- Per-tenant model settings, provider terms and evaluation dataset with lawyer sign-off.

**Acceptance:** unsupported evidence leads to abstain, citation integrity verified, no agent direct Clio write.

## Sprint E — production controls

- W3C trace context, OTel SDKs on every service, GenAI usage and redacted logs; operational audit separate.
- Secrets KMS, regional storage, file scanning, data retention, backup/restore, revocation propagation.
- Kubernetes/managed deployment if required for SLO, horizontal scaling, rate-limit fairness.
- Performance tests, failure injection, blue-green/rollback and security review.

**Acceptance:** pilot signed off with measured SLO/AI quality gates and no known cross-tenant or approval bypass defects.

# Provider-neutral connector contract

Current extension: see [configured integrations and operations](CONFIGURED_INTEGRATIONS.md) for implemented file processing, OAuth/polling adapters, configured legal generation, authoritative review, migrations 011–015 and current limitations. Earlier gap lists below describe the preceding baseline.

## Contract principles

Provider router resolves `(tenantId, resource_owner, resource_type)` to a connection and adapter; it NEVER assumes Clio. Capability manifests say what a provider can do, using explicit contract versions. The router does not claim a Clio operation is working unless its adapter implements it.

`MatterRef`, `DocumentRef`, `ExternalMapping` have internal LawPilot IDs and source-specific opaque external IDs. Do not embed Clio IDs into domain identifiers. Preserve provider-specific optional fields in versioned validated extensions, not unbounded JSON.

## Planned provider onboarding

1. Register manifest, identity of adaptor, supported contracts, regions and authentication style.
2. Run conformance suite on sandbox fixtures and negative auth tests.
3. Store opaque credential reference in secrets manager, not the provider manifest.
4. Configure tenant connection and resource ownership, complete initial read-only sync.
5. Run reconciliation and staged write tests before activation.
6. Roll back by disabling writes while retaining mappings and audit history.

## Clio Manage adapter checklist (not completed)

- OAuth2 authorization code flow; secure encrypted refresh token storage, CSRF state, scopes and user grants.
- Choose US/EU/CA/AU regional base URL based on Clio account; tokens do not cross regions.
- Read/write contact and matter fields with explicit `fields` requests; handle pagination.
- Documents: supported upload flow, accepted formats, external-storage limitations, version mapping.
- Webhooks: initial `X-Hook-Secret` handshake, HMAC-SHA256 `X-Hook-Signature` validation with raw bytes, renew before expiry and periodic reconciliation.
- Rate limits: default peak 50 req/min/access token; read `X-RateLimit-*` and `Retry-After`, fair priority per connection.
- API version pinning via `X-API-VERSION`; ETag/concurrency where supported.
- Never refresh tokens or retry non-idempotent writes from agent code.

Official reference: https://docs.developers.clio.com/clio-manage/api-reference/

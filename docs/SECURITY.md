# Security / legal confidentiality checklist

**Do not use with real legal client data until reviewed and hardened.**

- Keycloak JWT: validate issuer, audience, expiry, signature using JWKS. Do not use unverified decode or bypass auth in production.
- Tenants: select active tenant per request, verify current membership in PostgreSQL, then set RLS context with transaction-scoped `set_config`.
- Matter access: explicit current grant in database; no automatic blanket lawyer access. Use Keycloak Authz Services if adopted as PDP, but dynamic matter grants remain enforced in domain layer.
- RLS: application role must not be superuser/BYPASSRLS; never let app use migration owner credentials.
- Sessions: browser uses Authorization Code+PKCE and HttpOnly Secure cookies via BFF; CSRF controls required.
- Internal APIs: HMAC is LOCAL SCAFFOLD ONLY; production replace with workload identity and mTLS / verified JWT service tokens and replay defenses.
- Agent execution context: immutable, bounded token/time/step budget, tools allowlisted per capability, no direct Clio credentials.
- Prompt injection: all documents, emails and retrieved text untrusted; never treat as system instruction.
- Source-grounded Q&A: no answer without verified citations to authorized versioned passages, explicit abstention on insufficient evidence.
- Legal drafts: only approved templates; flagged unresolved placeholders; lawyer approval bound to immutable draft version before publish/export.
- Files: malware scan, size/type limits, encrypted object storage, signed short-lived upload URLs, content checksum and provenance.
- Privacy: regional data placement, provider DPA and zero-training/no-retention assessment before sending documents to LLM.
- Logs: remove OAuth tokens, passwords, legal content, contact data and user text. Keep audit as immutable access-controlled records, independently from telemetry.
- Operations: secrets manager, KMS rotation, backups/restore testing, incident response, SAST/SCA, container scanning, dependency pinning, least-privilege IAM.
- Security release gate: cross-tenant tests, cross-matter tests, ACL revocation during workflows, malicious-document tests, approval bypass tests.

## Durable command boundary

The API accepts a command only after tenant membership/capability/matter checks, atomically with a metadata-only dispatch intent. Status/cancel/requeue require the original principal and current grants. Worker activities reauthorize both before inference and result persistence; cancellation/terminal-state guards prevent late overwrites. Raw request/results remain in RLS-protected execution rows, never workflow histories or outbox metadata. Dispatcher roles can execute narrow metadata/lease functions but cannot SELECT private inputs.

Worker-to-Supervisor transport still uses development HMAC, and authoritative approval/evidence resolution are not implemented. Consequently real provider writes, confidential-data processing and production deployment remain outside the implemented scope. See [remediation tracking](REMEDIATION_STATUS.md) for P0-02/P0-03/P0-04 gates.

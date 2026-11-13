# Active remediation roadmap

Current extension: see [configured integrations and operations](CONFIGURED_INTEGRATIONS.md) for implemented file processing, OAuth/polling adapters, configured legal generation, authoritative review, migrations 011–015 and current limitations. Earlier gap lists below describe the preceding baseline.

Follow [REMEDIATION_STATUS.md](REMEDIATION_STATUS.md) and the uploaded calendar-expanded specification for the current PR sequence. Durable mock execution (P0-01) is implemented locally; the next slices are trusted service identity/authoritative approvals and contract/migration governance. Calendar, Context Engine, real AI, Clio and general event delivery remain pending. The earlier milestone summary below provides product context, not a claim that those capabilities are shipped.

# Phased implementation (updated LawPilot direction)

## Milestone 0 — Contracts and product decisions
Select first US practice area; define lawyer-only vs client-facing AI boundaries, approved templates, Clio Manage vs Grow integration, model provider & privacy terms. Owner approves acceptance tests and budget.

## Milestone 1 — Runnable foundation (this repository)
Keycloak, canonical domain/matter ACL, RLS, API, Root Supervisor, mock AI and provider adapters, contract tests, health and basic audit. **No real Clio or AI use yet.**

## Milestone 2 — Real Clio and workflow integration
Complete OAuth, encrypted credential reference, regional API pinning, contacts/matters/documents, webhook verification, provider conformance tests, Temporal workflow activities and reliable outbox. Validate with Clio sandbox.

## Milestone 3 — Secure evidence pipeline
Secure uploads/downloads, scanning, parsing with provenance, text chunks, version metadata, matter-scoped retrieval using pgvector (extension enabled; retrieval implementation pending), verified citations, access revocation.

## Milestone 4 — Four product capabilities
1. Intake summary and routing recommendations; 2. document summary; 3. draft from approved template; 4. lawyer-facing cited Q&A. Each has schema validated agent result, lawyer review and audit.

## Milestone 5 — Pilot readiness
Threat review, US data residency, AI provider terms, lawyer-evaluated golden datasets, failure injection, prompt-injection defenses, Clio sync recovery, tracing, cost budgets, backup restoration and explicit go/no-go.

## Release gates (quantitative targets must be set with pilot lawyers)
- Zero demonstrated cross-tenant / cross-matter leakage in adversarial tests.
- Approved drafts only, approval version guaranteed; unauthorized writes blocked.
- No invented citations in evaluation: unverifiable answers abstain.
- No silent loss of accepted integration tasks; reconcile uncertain external outcomes.
- End-to-end traces for all key paths, without privileged document contents.
- Clio API rate-limit and webhook disruption simulated successfully.

**No credible date or team commitment until project owner confirms changed scope.** The original 14-week two-developer plan was for the earlier template-only MVP.

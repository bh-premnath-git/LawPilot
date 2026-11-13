# Observability and operations contract

Latest scope: [capability extensions and validation](CAPABILITY_EXTENSIONS.md) covers webhooks, reconciled/bounded calendar sync, reviewed appointments, OCR, curated knowledge/calendar recipes, Go tracing and production policy templates (migrations 016–018).

Current extension: see [configured integrations and operations](CONFIGURED_INTEGRATIONS.md) for implemented file processing, OAuth/polling adapters, configured legal generation, authoritative review, migrations 011–015 and current limitations. Earlier gap lists below describe the preceding baseline.

Required span attributes (privacy-safe only): `service.name`, `request_id`, `trace_id`, `workflow_id`, `agent_capability`, `agent_version`, `provider_id`, `provider_operation`, `decision_code` (non-sensitive). Tenant IDs are high-cardinality and must NOT become metrics labels.

- Propagate W3C traceparent/tracestate across HTTP, gRPC, Temporal activity and messaging boundaries.
- Correlate separate traces for long-lived approval workflows using persisted workflow IDs and OpenTelemetry links; do not leave a span open for days.
- Separate immutable legal audit actions from sampled traces and expiring logs.
- Expose /healthz (process up) and /readyz (dependencies ready) per workload; TODO replace starter health with dependency-aware readiness probes.
- Watch Clio 429 rate, Remaining/Reset budget, token refresh, webhook validation failures and reconciliation lag.
- Watch AI abstain/validation rate, model cost, source-citation correctness, manual review ratio, retries and queue backlog.
- Alerts: actual customer-facing SLO impact, approval bypass attempts, tenant leaks, stuck workflows, provider outage, audit write failure.
- Collector config in `infra/otel-collector.yaml` exports to debug only; replace with properly secured Tempo/Mimir/Loki sinks in production.

## Implemented request tracing

`@lawpilot/telemetry` configures the TypeScript SDK and HTTP OTLP exporter when `OTEL_EXPORTER_OTLP_ENDPOINT` is set. The Supervisor propagates W3C context to the Python worker. The API exports separate request spans; the durable Go/Temporal boundary does not yet propagate tracing context. Span names are static and custom attributes contain HTTP response status only. Do not attach request input, result text, authentication headers or arbitrary exception messages.

Compose uses the collector's internal HTTP address; host-run services use `http://localhost:4318`. Export is disabled when the endpoint is unset. Shutdown hooks flush pending spans. Legal audit and outbox records are transactional database writes and remain independent of sampling or collector availability.

Go provider, Temporal and Rust instrumentation, metrics and operational dashboards are still pending. The collector's debug exporter provides local inspection, not durable telemetry storage.

## Durable execution operations

Execution status exposes state, activity attempts and start-intent delivery state/attempts/next retry/error code. Monitor `dead_letter`, old pending leases, stale queued/running states and terminated workflows. Dispatch retries preserve workflow IDs, with reject-duplicate reuse policy and fenced acknowledgements. Only execution command events are delivered by this dispatcher; do not infer that lifecycle broadcasts have consumers.

The API database readiness endpoint is `/v1/readyz`; acceptance remains available while Temporal/AI are offline. API, worker, dispatcher and Temporal recovery verification and the admin migration procedure are documented in [DURABLE_EXECUTIONS.md](DURABLE_EXECUTIONS.md). Production SLOs, Go tracing, metrics, alert routing and backup/restore remain unimplemented.

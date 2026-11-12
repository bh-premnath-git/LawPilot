# Observability and operations contract

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

`@lawpilot/telemetry` configures the TypeScript SDK and HTTP OTLP exporter when `OTEL_EXPORTER_OTLP_ENDPOINT` is set. The API and Supervisor propagate W3C context to the Python worker, which exports with the Python SDK. Span names are static and custom attributes contain HTTP response status only. Do not attach request input, result text, authentication headers or arbitrary exception messages.

Compose uses the collector's internal HTTP address; host-run services use `http://localhost:4318`. Export is disabled when the endpoint is unset. Shutdown hooks flush pending spans. Legal audit and outbox records are transactional database writes and remain independent of sampling or collector availability.

Go provider, Temporal and Rust instrumentation, metrics and operational dashboards are still pending. The collector's debug exporter provides local inspection, not durable telemetry storage.

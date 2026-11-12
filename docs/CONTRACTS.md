# Contract governance

## Types of contracts

- HTTP: OpenAPI + JSON Schema; generated clients for TypeScript/UI.
- Inter-service: Protobuf/gRPC for high-traffic typed operations, `buf breaking` in CI. Initial skeleton transports signed JSON as a migration-friendly first step.
- Events: CloudEvents-shaped envelope with immutable event ID, source, tenant scope, subject, schema version, occurred_at, correlation ID and payload. Document with AsyncAPI.
- AI capability task/result: schema registry, typed `capability_id`, versioned tool contract; reject unknown fields/operations.
- Provider manifests: advertised versioned capabilities with contract and restrictions; conformance suite per adapter.

## Evolution rules

1. Add optional fields only when old consumers can safely ignore them.
2. Never repurpose Protobuf field numbers; use reserved names/numbers after removals.
3. A semantics change creates a new capability contract; deprecate old consumers with telemetry.
4. Never use protobuf Any or arbitrary JSON without a type URL and registered schema validation.
5. Use explicit `UNSUPPORTED_CAPABILITY` and `SCHEMA_VERSION_MISMATCH` errors.
6. Rollout supports N and N-1 across services; contract consumers must be tested independently.
7. Contract validation is not authorization. Never trust actor claims from a capability payload without a verified service caller.

## Golden tests

- A mock provider and Clio adapter return equivalent canonical Matter on equivalent known fixtures.
- Both implement manifest versions, pagination, rate-limit error mapping and idempotency semantics.
- Generated TypeScript, Go, Python and Rust clients validate identical fixtures.
- AI tools cannot fetch evidence without authorized matter scoping.

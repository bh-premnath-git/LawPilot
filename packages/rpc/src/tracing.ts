import {
  context,
  propagation,
  ROOT_CONTEXT,
  trace,
  SpanStatusCode,
} from "@opentelemetry/api";
import { Metadata, type ServerUnaryCall } from "@grpc/grpc-js";
export function traceMetadata(): Metadata {
  const values: Record<string, string> = {};
  propagation.inject(context.active(), values);
  const metadata = new Metadata();
  for (const [key, value] of Object.entries(values)) metadata.set(key, value);
  return metadata;
}
export function withRpcSpan<T>(
  call: ServerUnaryCall<any, any>,
  operation: string,
  action: () => Promise<T>,
): Promise<T> {
  const values: Record<string, string> = {};
  for (const [key, value] of Object.entries(call.metadata.getMap()))
    if (
      typeof value === "string" &&
      (key === "traceparent" || key === "tracestate")
    )
      values[key] = value;
  return context.with(propagation.extract(ROOT_CONTEXT, values), () =>
    trace.getTracer("lawpilot").startActiveSpan(operation, async (span) => {
      try {
        return await action();
      } catch (error) {
        span.setStatus({ code: SpanStatusCode.ERROR });
        throw error;
      } finally {
        span.end();
      }
    }),
  );
}

import { context, propagation, ROOT_CONTEXT, SpanStatusCode, trace } from '@opentelemetry/api';
import { NodeSDK } from '@opentelemetry/sdk-node';
import { OTLPTraceExporter } from '@opentelemetry/exporter-trace-otlp-http';
import { IncomingMessage, ServerResponse } from 'node:http';

let sdk: NodeSDK | undefined;
export async function shutdownTelemetry(): Promise<void> { await sdk?.shutdown(); }

export function startTelemetry(serviceName: string): void {
  if (!process.env.OTEL_EXPORTER_OTLP_ENDPOINT) return;
  sdk=new NodeSDK({serviceName,traceExporter:new OTLPTraceExporter()});
  sdk.start();
}

// Only static operation names and response status are recorded. Never attach bodies,
// tokens, query strings, tenant IDs or user-provided error text to spans.
export function withHttpSpan(name: string, req: IncomingMessage, res: ServerResponse, action: () => unknown): void {
  const parent=propagation.extract(ROOT_CONTEXT,req.headers);
  context.with(parent,()=>trace.getTracer('lawpilot').startActiveSpan(name,span=>{
    res.once('finish',()=>{
      span.setAttribute('http.response.status_code',res.statusCode);
      if(res.statusCode>=500) span.setStatus({code:SpanStatusCode.ERROR});
      span.end();
    });
    res.once('close',()=>{ if(!res.writableFinished) span.end(); });
    try {
      Promise.resolve(action()).catch(()=>{
        span.setStatus({code:SpanStatusCode.ERROR});
        if(!res.headersSent){res.writeHead(500,{'content-type':'application/json'});res.end('{"error":"internal_error"}');}
        else res.destroy();
      });
    } catch {
      span.setStatus({code:SpanStatusCode.ERROR});
      if(!res.headersSent){res.writeHead(500,{'content-type':'application/json'});res.end('{"error":"internal_error"}');}
      else res.destroy();
    }
  }));
}
export function internalTraceHeaders(): Record<string,string> {
  const headers:Record<string,string>={};
  propagation.inject(context.active(),headers);
  return headers;
}
export function activeTraceId(): string | undefined {return trace.getSpan(context.active())?.spanContext().traceId;}

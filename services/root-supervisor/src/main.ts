import {
  startTelemetry,
  shutdownTelemetry,
  withHttpSpan,
} from "@lawpilot/telemetry";
import {
  Capability as RPC,
  serverCredentials,
  clientCredentials,
  requireWorkload,
  traceMetadata,
  withRpcSpan,
} from "@lawpilot/rpc";
import { Server, status, type ServiceError } from "@grpc/grpc-js";
import { createServer } from "node:http";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import {
  capabilityRequestSchema,
  capabilityRegistrySchema,
  findCapability,
  validateAgentResult,
} from "@lawpilot/contracts";
startTelemetry("root-supervisor");
const registry = capabilityRegistrySchema.parse(
  JSON.parse(
    readFileSync(
      process.env.CAPABILITY_REGISTRY_PATH ||
        resolve(__dirname, "../../../contracts/capabilities/registry.v1.json"),
      "utf8",
    ),
  ),
);
const target = process.env.AI_GRPC_ADDRESS;
if (!target) throw new Error("AI_GRPC_ADDRESS is required");
const worker = new RPC.CapabilityServiceClient(target, clientCredentials(), {
  "grpc.ssl_target_name_override": "ai-capabilities",
  "grpc.max_receive_message_length": 1048576,
});
const grpc = new Server({
  "grpc.max_receive_message_length": 32768,
  "grpc.max_send_message_length": 1048576,
});
grpc.addService(RPC.SupervisorServiceService, {
  executeCapability: async (call: any, callback: any) => {
    try {
      const accepted = await withRpcSpan(
        call,
        "supervisor.execute.grpc",
        async () => {
          requireWorkload(call, "execution-worker");
          const message = call.request as RPC.ExecuteCapabilityRequest;
          const parsed = capabilityRequestSchema.safeParse({
            executionId: message.executionId,
            tenantId: message.tenantId,
            principalId: message.principalId,
            ...(message.matterId ? { matterId: message.matterId } : {}),
            capabilityId: message.capabilityId,
            input: message.input,
            traceId: message.traceId,
          });
          if (!parsed.success)
            throw Object.assign(new Error("INVALID_TASK"), {
              code: status.INVALID_ARGUMENT,
            });
          const task = parsed.data,
            cap = findCapability(registry, task.capabilityId);
          if (!cap)
            throw Object.assign(new Error("UNSUPPORTED_CAPABILITY"), {
              code: status.UNIMPLEMENTED,
            });
          if (cap.requiresMatter && !task.matterId)
            throw Object.assign(new Error("MATTER_REQUIRED"), {
              code: status.INVALID_ARGUMENT,
            });
          if (Buffer.byteLength(JSON.stringify(task.input)) > cap.maxInputBytes)
            throw Object.assign(new Error("INPUT_TOO_LARGE"), {
              code: status.RESOURCE_EXHAUSTED,
            });
          const response = await new Promise<RPC.ExecuteCapabilityResponse>(
            (ok, fail) =>
              worker.executeCapability(
                message,
                traceMetadata(),
                {
                  deadline: Math.min(
                    Date.now() + 20000,
                    Number(call.getDeadline()),
                  ),
                },
                (error, result) => (error ? fail(error) : ok(result)),
              ),
          );
          const result = validateAgentResult(
            {
              executionId: response.executionId,
              agentId: response.agentId,
              agentVersion: response.agentVersion,
              status: response.status,
              output: response.output,
              evidence: response.evidence.map((ref) => ({
                ...ref,
                ...(ref.pageNumber === undefined
                  ? {}
                  : { pageNumber: ref.pageNumber }),
              })),
              proposedActions: [],
              requiresAttorneyReview: response.requiresAttorneyReview,
            },
            task,
            cap,
          );
          return RPC.ExecuteCapabilityResponse.fromPartial({
            ...result,
            output: result.output,
          });
        },
      );
      callback(null, accepted);
    } catch (error) {
      const code = (error as ServiceError).code;
      callback({
        code: typeof code === "number" ? code : status.FAILED_PRECONDITION,
        message:
          typeof code === "number"
            ? "CAPABILITY_REQUEST_REJECTED"
            : "INVALID_AGENT_RESULT",
      });
    }
  },
});
const http = createServer((req, res) =>
  withHttpSpan("supervisor.health", req, res, () => {
    res.setHeader("content-type", "application/json");
    res.setHeader("cache-control", "no-store");
    if (req.method === "GET" && req.url === "/healthz") {
      res.end(
        JSON.stringify({
          service: "root-supervisor",
          status: "up",
          transport: "grpc-mtls",
        }),
      );
      return;
    }
    res.writeHead(req.url === "/internal/execute" ? 410 : 404);
    res.end(JSON.stringify({ error: "USE_AUTHENTICATED_GRPC" }));
  }),
);
grpc.bindAsync(
  process.env.SUPERVISOR_GRPC_BIND || "0.0.0.0:50052",
  serverCredentials(),
  (error) => {
    if (error) throw error;
    http.listen(Number(process.env.SUPERVISOR_PORT || 3001), "0.0.0.0");
  },
);
for (const signal of ["SIGTERM", "SIGINT"])
  process.once(signal, () => {
    const timer = setTimeout(() => process.exit(1), 10000);
    timer.unref();
    grpc.tryShutdown(() => {
      worker.close();
      http.close(() => {
        void shutdownTelemetry().finally(() => process.exit(0));
      });
    });
  });

import {
  Body,
  Controller,
  Get,
  Post,
  Put,
  Param,
  Req,
  HttpCode,
  HttpException,
  Injectable,
  OnModuleDestroy,
} from "@nestjs/common";
import { Request } from "express";
import { randomUUID } from "node:crypto";
import { status, type ServiceError } from "@grpc/grpc-js";
import {
  Integrations as RPC,
  Context as CTX,
  clientCredentials,
  traceMetadata,
} from "@lawpilot/rpc";
import { z } from "zod";
import { AuthService } from "../auth.service";
import { ContextClient } from "../context/context.controller";
const uuid = z.uuid(),
  key = z.string().min(8).max(128),
  sha = z.string().regex(/^[a-f0-9]{64}$/);
function parse<T>(schema: z.ZodType<T>, raw: unknown): T {
  const value = schema.safeParse(raw);
  if (!value.success)
    throw new HttpException({ error: "INVALID_INTEGRATION_REQUEST" }, 400);
  return value.data;
}
@Injectable()
export class IntegrationClient implements OnModuleDestroy {
  private client?: RPC.IntegrationServiceClient;
  onModuleDestroy() {
    this.client?.close();
  }
  async invoke(
    method: string,
    payload: any,
    authorization: string | undefined,
  ): Promise<any> {
    try {
      if (!this.client) {
        const target = process.env.INTEGRATION_GRPC_ADDRESS;
        if (!target) throw Error("integration address required");
        this.client = new RPC.IntegrationServiceClient(
          target,
          clientCredentials(),
          {
            "grpc.ssl_target_name_override": "integration-service",
            "grpc.max_receive_message_length": 1048576,
            "grpc.max_send_message_length": 65536,
          },
        );
      }
      const aliases: Record<string, string> = {
        finalizeUpload: "FileRequest",
        getFile: "FileRequest",
        downloadFile: "FileRequest",
        authorizeConnection: "ConnectionRequest",
        disconnect: "ConnectionRequest",
        completeOAuth: "OAuthCompleteRequest",
        requestSync: "SyncRequest",
        getJob: "JobRequest",
        getProviderProposal: "ProviderProposalRequest",
      };
      const type = (RPC as any)[
        aliases[method] || method[0].toUpperCase() + method.slice(1) + "Request"
      ];
      const metadata = traceMetadata();
      metadata.set("authorization", authorization || "");
      return await new Promise((resolve, reject) =>
        (this.client as any)[method](
          type.fromPartial(payload),
          metadata,
          { deadline: Date.now() + 25000 },
          (err: ServiceError | null, result: unknown) =>
            err ? reject(err) : resolve(result),
        ),
      );
    } catch (error) {
      const e = error as ServiceError;
      const map: Record<number, number> = {
        [status.INVALID_ARGUMENT]: 400,
        [status.UNAUTHENTICATED]: 401,
        [status.PERMISSION_DENIED]: 403,
        [status.NOT_FOUND]: 404,
        [status.ALREADY_EXISTS]: 409,
        [status.ABORTED]: 409,
        [status.FAILED_PRECONDITION]: 409,
        [status.RESOURCE_EXHAUSTED]: 429,
        [status.DEADLINE_EXCEEDED]: 504,
      };
      throw new HttpException(
        {
          error:
            map[e.code] && /^[A-Z_]{1,100}$/.test(e.details || "")
              ? e.details
              : "INTEGRATION_SERVICE_UNAVAILABLE",
        },
        map[e.code] || 503,
      );
    }
  }
}
@Controller("v1/tenants/:tenantId")
export class IntegrationController {
  constructor(
    private auth: AuthService,
    private rpc: IntegrationClient,
  ) {}
  private async call(
    method: string,
    tenant: string,
    req: Request,
    value: any = {},
  ) {
    parse(uuid, tenant);
    await this.auth.subject(req.headers.authorization);
    return this.rpc.invoke(
      method,
      {
        ...value,
        scope: {
          tenantId: tenant,
          requestId: (req as any).lawpilotRequestId || randomUUID(),
        },
      },
      req.headers.authorization,
    );
  }
  @Get("integration-status") async status(
    @Param("tenantId") tenant: string,
    @Req() req: Request,
  ) {
    return RPC.StatusResponse.toJSON(await this.call("status", tenant, req));
  }
  @Post("matters/:matterId/files/uploads") async upload(
    @Param("tenantId") tenant: string,
    @Param("matterId") matterId: string,
    @Req() req: Request,
    @Body() body: unknown,
  ) {
    parse(uuid, matterId);
    const value = parse(
      z
        .object({
          filename: z.string().min(1).max(200),
          mediaType: z.enum([
            "application/pdf",
            "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
          ]),
          sizeBytes: z.number().int().min(1).max(10485760),
          sha256: sha,
          idempotencyKey: key,
        })
        .strict(),
      body,
    );
    return RPC.CreateUploadResponse.toJSON(
      await this.call("createUpload", tenant, req, { ...value, matterId }),
    );
  }
  @Post("matters/:matterId/files/:fileId/finalize")
  @HttpCode(202)
  async finalize(
    @Param("tenantId") tenant: string,
    @Param("matterId") matterId: string,
    @Param("fileId") fileId: string,
    @Req() req: Request,
  ) {
    parse(uuid, matterId);
    parse(uuid, fileId);
    return RPC.FileResponse.toJSON(
      await this.call("finalizeUpload", tenant, req, { matterId, fileId }),
    );
  }
  @Get("matters/:matterId/files/:fileId") async file(
    @Param("tenantId") tenant: string,
    @Param("matterId") matterId: string,
    @Param("fileId") fileId: string,
    @Req() req: Request,
  ) {
    parse(uuid, matterId);
    parse(uuid, fileId);
    return RPC.FileResponse.toJSON(
      await this.call("getFile", tenant, req, { matterId, fileId }),
    );
  }
  @Get("matters/:matterId/files/:fileId/download") async download(
    @Param("tenantId") tenant: string,
    @Param("matterId") matterId: string,
    @Param("fileId") fileId: string,
    @Req() req: Request,
  ) {
    parse(uuid, matterId);
    parse(uuid, fileId);
    return RPC.DownloadResponse.toJSON(
      await this.call("downloadFile", tenant, req, { matterId, fileId }),
    );
  }
  @Post("provider-connections") async connection(
    @Param("tenantId") tenant: string,
    @Req() req: Request,
    @Body() body: unknown,
  ) {
    const value = parse(
      z
        .object({
          providerId: z.enum(["clio-manage", "generic-rest"]),
          idempotencyKey: key,
        })
        .strict(),
      body,
    );
    return RPC.ConnectionResponse.toJSON(
      await this.call("createConnection", tenant, req, value),
    );
  }
  @Post("provider-connections/:connectionId/oauth/start")
  @HttpCode(200)
  async authorize(
    @Param("tenantId") tenant: string,
    @Param("connectionId") connectionId: string,
    @Req() req: Request,
  ) {
    parse(uuid, connectionId);
    return RPC.AuthorizeResponse.toJSON(
      await this.call("authorizeConnection", tenant, req, { connectionId }),
    );
  }
  @Post("provider-connections/:connectionId/oauth/complete")
  @HttpCode(200)
  async complete(
    @Param("tenantId") tenant: string,
    @Param("connectionId") connectionId: string,
    @Req() req: Request,
    @Body() body: unknown,
  ) {
    parse(uuid, connectionId);
    const value = parse(
      z
        .object({
          code: z.string().min(1).max(4096),
          state: z.string().min(32).max(128),
        })
        .strict(),
      body,
    );
    return RPC.ConnectionResponse.toJSON(
      await this.call("completeOAuth", tenant, req, { ...value, connectionId }),
    );
  }
  @Post("provider-connections/:connectionId/disconnect")
  @HttpCode(200)
  async disconnect(
    @Param("tenantId") tenant: string,
    @Param("connectionId") connectionId: string,
    @Req() req: Request,
  ) {
    parse(uuid, connectionId);
    return RPC.ConnectionResponse.toJSON(
      await this.call("disconnect", tenant, req, { connectionId }),
    );
  }
  @Put("provider-connections/:connectionId/grants/:principalId") async grant(
    @Param("tenantId") tenant: string,
    @Param("connectionId") connectionId: string,
    @Param("principalId") principalId: string,
    @Req() req: Request,
    @Body() body: unknown,
  ) {
    parse(uuid, connectionId);
    parse(z.string().min(1).max(200), principalId);
    const value = parse(
      z
        .object({
          permissions: z
            .array(z.enum(["read", "manage", "sync", "approve"]))
            .max(4),
          expectedRevision: z
            .number()
            .int()
            .positive()
            .max(Number.MAX_SAFE_INTEGER),
          idempotencyKey: key,
        })
        .strict(),
      body,
    );
    return RPC.ConnectionResponse.toJSON(
      await this.call("grantConnection", tenant, req, {
        ...value,
        connectionId,
        principalId,
      }),
    );
  }
  @Post("provider-connections/:connectionId/sync") @HttpCode(202) async sync(
    @Param("tenantId") tenant: string,
    @Param("connectionId") connectionId: string,
    @Req() req: Request,
    @Body() body: unknown,
  ) {
    parse(uuid, connectionId);
    const value = parse(z.object({ idempotencyKey: key }).strict(), body);
    return RPC.JobResponse.toJSON(
      await this.call("requestSync", tenant, req, { ...value, connectionId }),
    );
  }
  @Get("integration-jobs/:jobId") async job(
    @Param("tenantId") tenant: string,
    @Param("jobId") jobId: string,
    @Req() req: Request,
  ) {
    parse(uuid, jobId);
    return RPC.JobResponse.toJSON(
      await this.call("getJob", tenant, req, { jobId }),
    );
  }
  @Post("provider-updates") async propose(
    @Param("tenantId") tenant: string,
    @Req() req: Request,
    @Body() body: unknown,
  ) {
    const value = parse(
      z
        .object({
          connectionId: uuid,
          matterId: uuid,
          title: z.string().min(1).max(1000),
          idempotencyKey: key,
        })
        .strict(),
      body,
    );
    return RPC.ProviderProposalResponse.toJSON(
      await this.call("proposeProviderUpdate", tenant, req, value),
    );
  }
  @Get("provider-updates/:operationId") async preview(
    @Param("tenantId") tenant: string,
    @Param("operationId") operationId: string,
    @Req() req: Request,
  ) {
    parse(uuid, operationId);
    return RPC.ProviderProposalResponse.toJSON(
      await this.call("getProviderProposal", tenant, req, { operationId }),
    );
  }
  @Post("provider-updates/:operationId/decision") @HttpCode(200) async decide(
    @Param("tenantId") tenant: string,
    @Param("operationId") operationId: string,
    @Req() req: Request,
    @Body() body: unknown,
  ) {
    parse(uuid, operationId);
    const value = parse(
      z
        .object({
          artifactSha256: sha,
          decision: z.enum(["approve", "reject"]),
          idempotencyKey: key,
        })
        .strict(),
      body,
    );
    return RPC.ProviderProposalResponse.toJSON(
      await this.call("decideProviderUpdate", tenant, req, {
        ...value,
        operationId,
      }),
    );
  }
}
@Controller("v1/tenants/:tenantId")
export class LegalReviewController {
  constructor(
    private auth: AuthService,
    private rpc: ContextClient,
  ) {}
  private async call(method: string, tenant: string, req: Request, value: any) {
    parse(uuid, tenant);
    await this.auth.subject(req.headers.authorization);
    return this.rpc.invoke(
      method,
      {
        ...value,
        scope: {
          tenantId: tenant,
          requestId: (req as any).lawpilotRequestId || randomUUID(),
        },
      },
      req.headers.authorization,
    );
  }
  @Post("legal/templates") async create(
    @Param("tenantId") tenant: string,
    @Req() req: Request,
    @Body() body: unknown,
  ) {
    const value = parse(
      z
        .object({
          name: z.string().min(1).max(120),
          version: z.number().int().positive(),
          body: z.string().min(1).max(16384),
          idempotencyKey: key,
        })
        .strict(),
      body,
    );
    return CTX.TemplateResponse.toJSON(
      await this.call("createTemplate", tenant, req, value),
    );
  }
  @Get("legal/templates/:templateId") async template(
    @Param("tenantId") tenant: string,
    @Param("templateId") templateId: string,
    @Req() req: Request,
  ) {
    parse(uuid, templateId);
    return CTX.TemplateResponse.toJSON(
      await this.call("getTemplate", tenant, req, { templateId }),
    );
  }
  @Post("legal/templates/:templateId/decision")
  @HttpCode(200)
  async templateDecision(
    @Param("tenantId") tenant: string,
    @Param("templateId") templateId: string,
    @Req() req: Request,
    @Body() body: unknown,
  ) {
    parse(uuid, templateId);
    const value = parse(
      z
        .object({
          expectedSha256: sha,
          decision: z.enum(["approve", "revoke"]),
        })
        .strict(),
      body,
    );
    return CTX.TemplateResponse.toJSON(
      await this.call("decideTemplate", tenant, req, { ...value, templateId }),
    );
  }
  @Get("executions/:executionId/review") async preview(
    @Param("tenantId") tenant: string,
    @Param("executionId") executionId: string,
    @Req() req: Request,
  ) {
    parse(uuid, executionId);
    return CTX.ReviewResponse.toJSON(
      await this.call("getLegalReview", tenant, req, { executionId }),
    );
  }
  @Post("executions/:executionId/review") @HttpCode(200) async review(
    @Param("tenantId") tenant: string,
    @Param("executionId") executionId: string,
    @Req() req: Request,
    @Body() body: unknown,
  ) {
    parse(uuid, executionId);
    const value = parse(
      z
        .object({
          expectedSha256: sha,
          decision: z.enum(["approve", "reject"]),
        })
        .strict(),
      body,
    );
    return CTX.ReviewResponse.toJSON(
      await this.call("decideLegalReview", tenant, req, {
        ...value,
        executionId,
      }),
    );
  }
}

import {
  Body,
  Controller,
  Get,
  Post,
  Delete,
  Param,
  Query,
  Req,
  HttpException,
  Injectable,
  OnModuleDestroy,
  Sse,
  MessageEvent,
} from "@nestjs/common";
import { Request } from "express";
import { randomUUID } from "node:crypto";
import { Metadata, status, type ServiceError } from "@grpc/grpc-js";
import {
  Context as RPC,
  clientCredentials,
  traceMetadata,
} from "@lawpilot/rpc";
import { Observable } from "rxjs";
import { z } from "zod";
import { AuthService } from "../auth.service";
const uuid = z.uuid(),
  key = z.string().min(8).max(128),
  revision = z.number().int().min(0).max(2147483646);
const citation = z
  .object({
    documentId: uuid,
    versionId: uuid,
    passageId: uuid,
    contentSha256: z.string().regex(/^[a-f0-9]{64}$/),
    byteStart: z.number().int().min(0).max(262144),
    byteEnd: z.number().int().positive().max(262144),
    pageNumber: z.number().int().min(1).max(200).optional(),
  })
  .strict();
function parse<T>(schema: z.ZodType<T>, value: unknown): T {
  const result = schema.safeParse(value);
  if (!result.success)
    throw new HttpException({ error: "INVALID_CONTEXT_REQUEST" }, 400);
  return result.data;
}
@Injectable()
export class ContextClient implements OnModuleDestroy {
  private client?: RPC.ContextServiceClient;
  onModuleDestroy() {
    this.client?.close();
  }
  async invoke(
    method: string,
    payload: unknown,
    authorization: string | undefined,
  ): Promise<any> {
    try {
      if (!this.client) {
        const target = process.env.CONTEXT_GRPC_ADDRESS;
        if (!target) throw new Error("CONTEXT_GRPC_ADDRESS required");
        this.client = new RPC.ContextServiceClient(
          target,
          clientCredentials(),
          {
            "grpc.ssl_target_name_override": "context-engine",
            "grpc.max_receive_message_length": 1048576,
            "grpc.max_send_message_length": 65536,
          },
        );
      }
      const aliases:Record<string,string>={getTemplate:"TemplateDecisionRequest",createTemplate:"TemplateRequest",decideTemplate:"TemplateDecisionRequest",getLegalReview:"ReviewRequest",decideLegalReview:"ReviewRequest"};
      const requestType=(RPC as any)[aliases[method] || method[0].toUpperCase()+method.slice(1)+"Request"];
      payload = requestType.fromPartial(payload);
      const metadata = traceMetadata();
      metadata.set("authorization", authorization || "");
      return await new Promise((resolve, reject) =>
        (this.client as any)[method](
          payload,
          metadata,
          { deadline: Date.now() + 25000 },
          (err: ServiceError | null, result: unknown) =>
            err ? reject(err) : resolve(result),
        ),
      );
    } catch (error) {
      const code = (error as ServiceError).code;
      const map: Record<number, number> = {
        [status.INVALID_ARGUMENT]: 400,
        [status.UNAUTHENTICATED]: 401,
        [status.PERMISSION_DENIED]: 403,
        [status.NOT_FOUND]: 404,
        [status.ALREADY_EXISTS]: 409,
        [status.ABORTED]: 409,
        [status.FAILED_PRECONDITION]: 409,
        [status.RESOURCE_EXHAUSTED]: 413,
        [status.UNIMPLEMENTED]: 501,
        [status.DEADLINE_EXCEEDED]: 504,
      };
      const details = (error as ServiceError).details || "";
      throw new HttpException(
        {
          error:
            map[code] && /^[A-Z_]{1,100}$/.test(details)
              ? details
              : "CONTEXT_SERVICE_UNAVAILABLE",
        },
        map[code] || 503,
      );
    }
  }
}
@Controller("v1/tenants/:tenantId/matters/:matterId")
export class ContextController {
  constructor(
    private auth: AuthService,
    private rpc: ContextClient,
  ) {}
  private async call(
    method: string,
    tenant: string,
    matter: string,
    req: Request,
    payload: any,
  ) {
    parse(uuid, tenant);
    parse(uuid, matter);
    await this.auth.subject(req.headers.authorization);
    return this.rpc.invoke(
      method,
      {
        ...payload,
        matterId: matter,
        scope: {
          tenantId: tenant,
          requestId: (req as any).lawpilotRequestId || randomUUID(),
        },
      },
      req.headers.authorization,
    );
  }
  @Post("documents/text") async ingest(
    @Param("tenantId") tenant: string,
    @Param("matterId") matter: string,
    @Req() req: Request,
    @Body() body: unknown,
  ) {
    const value = parse(
      z
        .object({
          documentId: uuid.optional(),
          expectedRevision: revision.default(0),
          title: z.string().trim().min(1).max(256),
          text: z
            .string()
            .min(1)
            .refine((v) => Buffer.byteLength(v) <= 24576 && !v.includes("\0")),
          idempotencyKey: key,
        })
        .strict(),
      body,
    );
    return RPC.IngestTextResponse.toJSON(
      await this.call("ingestText", tenant, matter, req, value),
    );
  }
  @Delete("documents/:documentId") async remove(
    @Param("tenantId") tenant: string,
    @Param("matterId") matter: string,
    @Param("documentId") documentId: string,
    @Req() req: Request,
    @Body() body: unknown,
  ) {
    parse(uuid, documentId);
    const value = parse(
      z
        .object({
          expectedRevision: revision.refine((v) => v > 0),
          idempotencyKey: key,
        })
        .strict(),
      body,
    );
    return RPC.DeleteDocumentResponse.toJSON(
      await this.call("deleteDocument", tenant, matter, req, {
        ...value,
        documentId,
      }),
    );
  }
  @Post("context/build") async build(
    @Param("tenantId") tenant: string,
    @Param("matterId") matter: string,
    @Req() req: Request,
    @Body() body: unknown,
  ) {
    const value = parse(
      z
        .object({
          recipeId: z.enum(["matter.qa.v1", "document.summary.v1"]),
          query: z.string().max(4096).default(""),
          documentId: uuid.optional(),
        })
        .strict(),
      body,
    );
    return RPC.BuildContextResponse.toJSON(
      await this.call("buildContext", tenant, matter, req, value),
    );
  }
  @Get("context/:packageId") async get(
    @Param("tenantId") tenant: string,
    @Param("matterId") matter: string,
    @Param("packageId") packageId: string,
    @Req() req: Request,
  ) {
    parse(uuid, packageId);
    return RPC.GetContextResponse.toJSON(
      await this.call("getContext", tenant, matter, req, { packageId }),
    );
  }
  @Post("evidence/resolve") async resolve(
    @Param("tenantId") tenant: string,
    @Param("matterId") matter: string,
    @Req() req: Request,
    @Body() body: unknown,
  ) {
    const value = parse(
      z.object({ citation, quote: z.string().min(1).max(4096) }).strict(),
      body,
    );
    return RPC.ResolveCitationResponse.toJSON(
      await this.call("resolveCitation", tenant, matter, req, value),
    );
  }
  @Get("events") async events(
    @Param("tenantId") tenant: string,
    @Param("matterId") matter: string,
    @Req() req: Request,
    @Query() query: unknown,
  ) {
    const value = parse(
      z
        .object({
          afterPosition: z.coerce
            .number()
            .int()
            .min(0)
            .max(Number.MAX_SAFE_INTEGER)
            .default(0),
          limit: z.coerce.number().int().min(1).max(100).default(50),
        })
        .strict(),
      query,
    );
    return RPC.ListMatterEventsResponse.toJSON(
      await this.call("listMatterEvents", tenant, matter, req, value),
    );
  }
  @Sse("events/stream") stream(
    @Param("tenantId") tenant: string,
    @Param("matterId") matter: string,
    @Req() req: Request,
    @Query() query: unknown,
  ): Observable<MessageEvent> {
    const value = parse(
      z
        .object({
          afterPosition: z.coerce
            .number()
            .int()
            .min(0)
            .max(Number.MAX_SAFE_INTEGER)
            .default(0),
        })
        .strict(),
      query,
    );
    parse(uuid, tenant);
    parse(uuid, matter);
    return new Observable((subscriber) => {
      let cursor = value.afterPosition,
        closed = false,
        busy = false;
      const poll = async () => {
        if (closed || busy) return;
        busy = true;
        try {
          // JWT, membership, capability and matter authorization are rerun on every poll.
          const result = await this.call(
            "listMatterEvents",
            tenant,
            matter,
            req,
            { afterPosition: cursor, limit: 100 },
          );
          if (closed) return;
          for (const event of result.events) {
            subscriber.next({
              id: String(event.position),
              type: "invalidate",
              data: RPC.EventNotification.toJSON(event) as object,
            });
            cursor = event.position;
          }
        } catch {
          if (!closed) {
            subscriber.next({
              type: "access-ended",
              data: { reason: "STREAM_REAUTHORIZATION_FAILED" },
            });
            subscriber.complete();
          }
        } finally {
          busy = false;
        }
      };
      void poll();
      const timer = setInterval(() => void poll(), 1000);
      return () => {
        closed = true;
        clearInterval(timer);
      };
    });
  }
}

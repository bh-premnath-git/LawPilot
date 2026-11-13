import {
  Body,
  Controller,
  Get,
  Post,
  Param,
  Req,
  HttpCode,
  HttpException,
} from "@nestjs/common";
import { Request } from "express";
import { randomUUID } from "node:crypto";
import { z } from "zod";
import { AuthService } from "../auth.service";
import { IntegrationClient } from "./integrations.controller";
import { ContextClient } from "../context/context.controller";
const uuid = z.uuid(),
  key = z.string().min(8).max(128),
  sha = z.string().regex(/^[a-f0-9]{64}$/);
function parse<T>(s: z.ZodType<T>, value: unknown): T {
  const r = s.safeParse(value);
  if (!r.success)
    throw new HttpException({ error: "INVALID_EXTENSION_REQUEST" }, 400);
  return r.data;
}
@Controller("v1/tenants/:tenantId")
export class ExtensionsController {
  constructor(
    private auth: AuthService,
    private integration: IntegrationClient,
    private context: ContextClient,
  ) {}
  private async scope(r: Request, tenantId: string) {
    await this.auth.subject(r.headers.authorization);
    return { tenantId: parse(uuid, tenantId), requestId: randomUUID() };
  }
  @Post("provider-connections/:connectionId/webhooks") async hook(
    @Req() r: Request,
    @Param("tenantId") tenant: string,
    @Param("connectionId") conn: string,
    @Body() body: unknown,
  ) {
    const scope = await this.scope(r, tenant);
    return this.integration.invoke(
      "registerWebhook",
      {
        scope,
        connectionId: parse(uuid, conn),
        ...parse(
          z
            .object({
              model: z.enum(["matter", "calendar_entry"]),
              idempotencyKey: key,
            })
            .strict(),
          body,
        ),
      },
      r.headers.authorization,
    );
  }
  @Post("provider-webhooks/:subscriptionId/activate")
  @HttpCode(200)
  async activate(
    @Req() r: Request,
    @Param("tenantId") tenant: string,
    @Param("subscriptionId") id: string,
  ) {
    return this.integration.invoke(
      "activateWebhook",
      { scope: await this.scope(r, tenant), subscriptionId: parse(uuid, id) },
      r.headers.authorization,
    );
  }
  @Post("provider-calendar-operations") async appointment(
    @Req() r: Request,
    @Param("tenantId") tenant: string,
    @Body() body: unknown,
  ) {
    return this.integration.invoke(
      "proposeCalendarWrite",
      {
        scope: await this.scope(r, tenant),
        ...parse(
          z
            .object({
              connectionId: uuid,
              calendarId: uuid,
              matterId: uuid,
              entryId: uuid.optional(),
              operation: z.enum(["create", "update"]),
              title: z.string().min(1).max(256),
              description: z.string().max(4096).default(""),
              startAt: z.iso.datetime({ offset: true }),
              endAt: z.iso.datetime({ offset: true }),
              timeZone: z.string().min(1).max(80),
              idempotencyKey: key,
            })
            .strict(),
          body,
        ),
      },
      r.headers.authorization,
    );
  }
  @Get("provider-calendar-operations/:operationId") async preview(
    @Req() r: Request,
    @Param("tenantId") tenant: string,
    @Param("operationId") id: string,
  ) {
    return this.integration.invoke(
      "getCalendarOperation",
      { scope: await this.scope(r, tenant), operationId: parse(uuid, id) },
      r.headers.authorization,
    );
  }
  @Post("provider-calendar-operations/:operationId/decision")
  @HttpCode(200)
  async decide(
    @Req() r: Request,
    @Param("tenantId") tenant: string,
    @Param("operationId") id: string,
    @Body() body: unknown,
  ) {
    return this.integration.invoke(
      "decideCalendarWrite",
      {
        scope: await this.scope(r, tenant),
        operationId: parse(uuid, id),
        ...parse(
          z
            .object({
              artifactSha256: sha,
              decision: z.enum(["approve", "reject"]),
              idempotencyKey: key,
            })
            .strict(),
          body,
        ),
      },
      r.headers.authorization,
    );
  }
  @Post("knowledge-libraries") async library(
    @Req() r: Request,
    @Param("tenantId") tenant: string,
    @Body() body: unknown,
  ) {
    return this.context.invoke(
      "registerKnowledgeLibrary",
      {
        scope: await this.scope(r, tenant),
        ...parse(
          z
            .object({ matterId: uuid, name: z.string().min(1).max(120) })
            .strict(),
          body,
        ),
      },
      r.headers.authorization,
    );
  }
  @Post("matters/:matterId/context/calendar") @HttpCode(200) async calendar(
    @Req() r: Request,
    @Param("tenantId") tenant: string,
    @Param("matterId") matter: string,
    @Body() body: unknown,
  ) {
    return this.context.invoke(
      "buildCalendarContext",
      {
        scope: await this.scope(r, tenant),
        matterId: parse(uuid, matter),
        ...parse(
          z
            .object({
              from: z.iso.datetime({ offset: true }),
              to: z.iso.datetime({ offset: true }),
            })
            .strict(),
          body,
        ),
      },
      r.headers.authorization,
    );
  }
}
@Controller("v1/provider-webhooks")
export class ProviderWebhookController {
  constructor(private integration: IntegrationClient) {}
  // Raw bytes are preserved before JSON decoding; these routes intentionally use
  // a scoped callback capability and HMAC instead of a browser OIDC token.
  @Post(":subscriptionId/:callbackToken") @HttpCode(200) async receive(
    @Req() r: Request & { rawBody?: Buffer },
    @Param("subscriptionId") id: string,
    @Param("callbackToken") token: string,
  ) {
    if (!r.rawBody || r.rawBody.length > 65536)
      throw new HttpException({ error: "INVALID_WEBHOOK_BODY" }, 400);
    return this.integration.invoke(
      "receiveWebhook",
      {
        subscriptionId: parse(uuid, id),
        callbackToken: token,
        body: r.rawBody,
        secret: r.headers["x-hook-secret"] || "",
        signature: r.headers["x-hook-signature"] || "",
      },
      undefined,
    );
  }
}

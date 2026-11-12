import {
  Body,
  Controller,
  Get,
  Post,
  Patch,
  Put,
  Param,
  Query,
  Req,
  Res,
  HttpCode,
  HttpException,
  Injectable,
  OnModuleDestroy,
} from "@nestjs/common";
import { Request, Response } from "express";
import { randomUUID } from "node:crypto";
import {
  Metadata,
  status as GrpcStatus,
  type ServiceError,
} from "@grpc/grpc-js";
import { Calendar as RPC, clientCredentials } from "@lawpilot/rpc";
import { z } from "zod";
import { AuthService } from "../auth.service";
const uuid = z.uuid(),
  key = z.string().min(8).max(128),
  revision = z.number().int().positive().max(Number.MAX_SAFE_INTEGER);
const instant = z.iso
    .datetime({ offset: true })
    .refine(
      (value) => !/[.,]\d{4,}/.test(value),
      "Millisecond precision required",
    ),
  civil = z.string().regex(/^\d{4}-\d{2}-\d{2}$/);
const schedule = z.discriminatedUnion("type", [
  z
    .object({
      type: z.literal("timed"),
      start: instant,
      end: instant,
      timeZone: z.string().min(1).max(100),
    })
    .strict(),
  z
    .object({
      type: z.literal("all_day"),
      startDate: civil,
      endDateExclusive: civil,
      timeZone: z.string().min(1).max(100),
    })
    .strict(),
]);
const draft = z
  .object({
    title: z.string().min(1).max(256),
    description: z.string().max(4096).default(""),
    kind: z.enum([
      "appointment",
      "consultation",
      "hearing",
      "task_block",
      "deadline_candidate",
      "verified_deadline",
    ]),
    visibility: z.enum(["private", "internal", "shared"]).default("private"),
    matterId: uuid.optional(),
    schedule,
    recurrence: z
      .object({
        frequency: z.enum(["daily", "weekly"]),
        interval: z.number().int().min(1).max(52).default(1),
        count: z.number().int().min(1).max(366),
      })
      .strict()
      .optional(),
    participantPrincipalIds: z
      .array(z.string().min(1).max(200))
      .max(50)
      .default([]),
    reminders: z
      .array(
        z
          .object({ minutesBefore: z.number().int().min(0).max(40320) })
          .strict(),
      )
      .max(3)
      .default([]),
  })
  .strict();
const changeScope = z.enum(["occurrence", "this_and_following", "series"]);
const change = z
  .object({
    entryId: uuid,
    expectedRevision: revision,
    expectedSeriesRevision: revision.optional(),
    scope: changeScope,
    entry: draft,
  })
  .strict();
const removal = change.omit({ entry: true });
const command = z.discriminatedUnion("type", [
  z.object({ type: z.literal("create"), entry: draft }).strict(),
  change.extend({ type: z.literal("update") }).strict(),
  removal.extend({ type: z.literal("cancel") }).strict(),
]);
function parse<T>(schema: z.ZodType<T>, raw: unknown): T {
  const value = schema.safeParse(raw);
  if (!value.success)
    throw new HttpException({ error: "INVALID_CALENDAR_REQUEST" }, 400);
  return value.data;
}
function civilDate(value: string) {
  const [year, month, day] = value.split("-").map(Number);
  return { year, month, day };
}
function entry(value: z.infer<typeof draft>): RPC.EntryDraft {
  return RPC.EntryDraft.fromPartial({
    ...value,
    kind: RPC.entryKindFromJSON("ENTRY_KIND_" + value.kind.toUpperCase()),
    visibility: RPC.visibilityFromJSON(
      "VISIBILITY_" + value.visibility.toUpperCase(),
    ),
    timed:
      value.schedule.type === "timed"
        ? {
            start: new Date(value.schedule.start),
            end: new Date(value.schedule.end),
            timeZone: value.schedule.timeZone,
          }
        : undefined,
    allDay:
      value.schedule.type === "all_day"
        ? {
            startDate: civilDate(value.schedule.startDate),
            endDateExclusive: civilDate(value.schedule.endDateExclusive),
            timeZone: value.schedule.timeZone,
          }
        : undefined,
    recurrence: value.recurrence
      ? {
          ...value.recurrence,
          frequency: RPC.frequencyFromJSON(
            "FREQUENCY_" + value.recurrence.frequency.toUpperCase(),
          ),
        }
      : undefined,
  });
}
function scopeEnum(value: z.infer<typeof changeScope>) {
  return RPC.changeScopeFromJSON("CHANGE_SCOPE_" + value.toUpperCase());
}
function rpcCommand(value: z.infer<typeof command>): RPC.CalendarCommand {
  if (value.type === "create") return { create: { draft: entry(value.entry) } };
  const source = {
    entryId: value.entryId,
    expectedRevision: value.expectedRevision,
    expectedSeriesRevision: value.expectedSeriesRevision || 0,
    scope: scopeEnum(value.scope),
  };
  return value.type === "update"
    ? { update: { ...source, draft: entry(value.entry) } }
    : { cancel: source };
}
@Injectable()
export class CalendarClient implements OnModuleDestroy {
  private client?: RPC.CalendarServiceClient;
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
        const target = process.env.CALENDAR_GRPC_ADDRESS;
        if (!target) throw new Error("CALENDAR_GRPC_ADDRESS required");
        this.client = new RPC.CalendarServiceClient(
          target,
          clientCredentials(),
          {
            "grpc.ssl_target_name_override": "calendar-service",
            "grpc.max_receive_message_length": 1048576,
            "grpc.max_send_message_length": 262144,
          },
        );
      }
      const metadata = new Metadata();
      metadata.set("authorization", authorization || "");
      return await new Promise((resolve, reject) =>
        (this.client as any)[method](
          payload,
          metadata,
          { deadline: Date.now() + 15000 },
          (error: ServiceError | null, result: unknown) =>
            error ? reject(error) : resolve(result),
        ),
      );
    } catch (error) {
      const code = (error as ServiceError).code;
      const map: Record<number, number> = {
        [GrpcStatus.INVALID_ARGUMENT]: 400,
        [GrpcStatus.UNAUTHENTICATED]: 401,
        [GrpcStatus.PERMISSION_DENIED]: 403,
        [GrpcStatus.NOT_FOUND]: 404,
        [GrpcStatus.ALREADY_EXISTS]: 409,
        [GrpcStatus.ABORTED]: 409,
        [GrpcStatus.FAILED_PRECONDITION]: 409,
        [GrpcStatus.RESOURCE_EXHAUSTED]: 429,
        [GrpcStatus.UNIMPLEMENTED]: 501,
        [GrpcStatus.DEADLINE_EXCEEDED]: 504,
      };
      const details = (error as ServiceError).details || "";
      throw new HttpException(
        {
          error:
            map[code] && /^[A-Z_]{1,100}$/.test(details)
              ? details
              : "CALENDAR_SERVICE_UNAVAILABLE",
        },
        map[code] || 503,
      );
    }
  }
}
@Controller("v1/tenants/:tenantId")
export class CalendarController {
  constructor(
    private auth: AuthService,
    private rpc: CalendarClient,
  ) {}
  private async call(
    method: string,
    tenantId: string,
    req: Request,
    payload: any,
  ) {
    parse(uuid, tenantId);
    await this.auth.subject(req.headers.authorization);
    return this.rpc.invoke(
      method,
      {
        ...payload,
        scope: {
          tenantId,
          requestId: (req as any).lawpilotRequestId || randomUUID(),
        },
      },
      req.headers.authorization,
    );
  }
  @Get("calendars") async calendars(
    @Param("tenantId") tenant: string,
    @Req() req: Request,
  ) {
    return RPC.ListCalendarsResponse.toJSON(
      await this.call("listCalendars", tenant, req, {}),
    );
  }
  @Post("calendars") async create(
    @Param("tenantId") tenant: string,
    @Req() req: Request,
    @Body() body: unknown,
  ) {
    const value = parse(
      z
        .object({
          name: z.string().min(1).max(120),
          defaultTimeZone: z.string().min(1).max(100),
          approvalRequired: z.boolean().default(true),
          idempotencyKey: key,
        })
        .strict(),
      body,
    );
    return RPC.CreateCalendarResponse.toJSON(
      await this.call("createCalendar", tenant, req, value),
    );
  }
  @Put("calendars/:calendarId/grants/:principalId") async grants(
    @Param("tenantId") tenant: string,
    @Param("calendarId") calendarId: string,
    @Param("principalId") principalId: string,
    @Req() req: Request,
    @Body() body: unknown,
  ) {
    parse(uuid, calendarId);
    parse(z.string().min(1).max(200), principalId);
    const value = parse(
      z
        .object({
          permissions: z
            .array(z.enum(["read", "write", "approve", "manage"]))
            .max(4),
          expectedRevision: revision,
          idempotencyKey: key,
        })
        .strict(),
      body,
    );
    return RPC.GrantCalendarResponse.toJSON(
      await this.call("grantCalendar", tenant, req, {
        ...value,
        calendarId,
        principalId,
      }),
    );
  }
  @Get("calendar-entries") async list(
    @Param("tenantId") tenant: string,
    @Req() req: Request,
    @Query() query: unknown,
  ) {
    const value = parse(
      z
        .object({
          calendarId: uuid,
          from: instant,
          to: instant,
          matterId: uuid.optional(),
          pageSize: z.coerce.number().int().min(1).max(100).default(50),
          pageToken: z.string().max(512).optional(),
        })
        .strict(),
      query,
    );
    return RPC.ListEntriesResponse.toJSON(
      await this.call("listEntries", tenant, req, {
        ...value,
        from: new Date(value.from),
        to: new Date(value.to),
      }),
    );
  }
  @Get("calendar-entries/:entryId") async get(
    @Param("tenantId") tenant: string,
    @Param("entryId") entryId: string,
    @Req() req: Request,
    @Res({ passthrough: true }) res: Response,
    @Query() query: unknown,
  ) {
    parse(uuid, entryId);
    const value = parse(z.object({ calendarId: uuid }).strict(), query);
    const reply = await this.call("getEntry", tenant, req, {
      ...value,
      entryId,
    });
    res.setHeader("ETag", `"${reply.entry.revision}"`);
    return RPC.GetEntryResponse.toJSON(reply);
  }
  @Post("calendar-entries") async createEntry(
    @Param("tenantId") tenant: string,
    @Req() req: Request,
    @Body() body: unknown,
  ) {
    const value = parse(
      z
        .object({ calendarId: uuid, entry: draft, idempotencyKey: key })
        .strict(),
      body,
    );
    return RPC.MutateEntryResponse.toJSON(
      await this.call("mutateEntry", tenant, req, {
        calendarId: value.calendarId,
        idempotencyKey: value.idempotencyKey,
        command: { create: { draft: entry(value.entry) } },
      }),
    );
  }
  @Patch("calendar-entries/:entryId") async update(
    @Param("tenantId") tenant: string,
    @Param("entryId") entryId: string,
    @Req() req: Request,
    @Body() body: unknown,
  ) {
    parse(uuid, entryId);
    const value = parse(
      change
        .omit({ entryId: true })
        .extend({ calendarId: uuid, idempotencyKey: key })
        .strict(),
      body,
    );
    this.match(req, value.expectedRevision);
    return RPC.MutateEntryResponse.toJSON(
      await this.call("mutateEntry", tenant, req, {
        calendarId: value.calendarId,
        idempotencyKey: value.idempotencyKey,
        command: rpcCommand({ ...value, entryId, type: "update" }),
      }),
    );
  }
  @Post("calendar-entries/:entryId/cancel") @HttpCode(200) async cancel(
    @Param("tenantId") tenant: string,
    @Param("entryId") entryId: string,
    @Req() req: Request,
    @Body() body: unknown,
  ) {
    parse(uuid, entryId);
    const value = parse(
      removal
        .omit({ entryId: true })
        .extend({ calendarId: uuid, idempotencyKey: key })
        .strict(),
      body,
    );
    this.match(req, value.expectedRevision);
    return RPC.MutateEntryResponse.toJSON(
      await this.call("mutateEntry", tenant, req, {
        calendarId: value.calendarId,
        idempotencyKey: value.idempotencyKey,
        command: rpcCommand({ ...value, entryId, type: "cancel" }),
      }),
    );
  }
  private match(req: Request, expected: number) {
    const header = req.headers["if-match"];
    if (header !== undefined && header !== `"${expected}"`)
      throw new HttpException({ error: "IF_MATCH_MISMATCH" }, 409);
  }
  @Post("calendar-entry-proposals") async propose(
    @Param("tenantId") tenant: string,
    @Req() req: Request,
    @Body() body: unknown,
  ) {
    const value = parse(
      z.object({ calendarId: uuid, command, idempotencyKey: key }).strict(),
      body,
    );
    return RPC.ProposeEntryResponse.toJSON(
      await this.call("proposeEntry", tenant, req, {
        ...value,
        command: rpcCommand(value.command),
      }),
    );
  }
  @Get("calendar-entry-proposals/:proposalId") async proposal(
    @Param("tenantId") tenant: string,
    @Param("proposalId") proposalId: string,
    @Req() req: Request,
    @Query() query: unknown,
  ) {
    parse(uuid, proposalId);
    const value = parse(z.object({ calendarId: uuid }).strict(), query);
    return RPC.GetProposalResponse.toJSON(
      await this.call("getProposal", tenant, req, { ...value, proposalId }),
    );
  }
  @Post("calendar-entry-proposals/:proposalId/approve")
  @HttpCode(200)
  async approve(
    @Param("tenantId") tenant: string,
    @Param("proposalId") proposalId: string,
    @Req() req: Request,
    @Body() body: unknown,
  ) {
    return this.decide(tenant, proposalId, req, body, "approve");
  }
  @Post("calendar-entry-proposals/:proposalId/reject")
  @HttpCode(200)
  async reject(
    @Param("tenantId") tenant: string,
    @Param("proposalId") proposalId: string,
    @Req() req: Request,
    @Body() body: unknown,
  ) {
    return this.decide(tenant, proposalId, req, body, "reject");
  }
  private async decide(
    tenant: string,
    proposalId: string,
    req: Request,
    body: unknown,
    decision: string,
  ) {
    parse(uuid, proposalId);
    const value = parse(
      z
        .object({
          calendarId: uuid,
          expectedSha256: z.string().regex(/^[a-f0-9]{64}$/),
          idempotencyKey: key,
        })
        .strict(),
      body,
    );
    return RPC.DecideProposalResponse.toJSON(
      await this.call("decideProposal", tenant, req, {
        ...value,
        proposalId,
        decision,
      }),
    );
  }
  @Post("calendar-availability/query") @HttpCode(200) async availability(
    @Param("tenantId") tenant: string,
    @Req() req: Request,
    @Body() body: unknown,
  ) {
    const value = parse(
      z.object({ calendarId: uuid, from: instant, to: instant }).strict(),
      body,
    );
    return RPC.AvailabilityResponse.toJSON(
      await this.call("availability", tenant, req, {
        ...value,
        from: new Date(value.from),
        to: new Date(value.to),
      }),
    );
  }
  @Get("calendar-sync/status") async sync(
    @Param("tenantId") tenant: string,
    @Req() req: Request,
    @Query() query: unknown,
  ) {
    const value = parse(z.object({ calendarId: uuid }).strict(), query);
    return RPC.SyncStatusResponse.toJSON(
      await this.call("syncStatus", tenant, req, value),
    );
  }
  @Get("calendar-entries/:entryId/reminders") async reminders(
    @Param("tenantId") tenant: string,
    @Param("entryId") entryId: string,
    @Req() req: Request,
    @Query() query: unknown,
  ) {
    parse(uuid, entryId);
    const value = parse(z.object({ calendarId: uuid }).strict(), query);
    return RPC.ListRemindersResponse.toJSON(
      await this.call("listReminders", tenant, req, { ...value, entryId }),
    );
  }
}

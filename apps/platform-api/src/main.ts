import { startTelemetry, shutdownTelemetry, withHttpSpan, internalTraceHeaders, activeTraceId } from '@lawpilot/telemetry';
import 'reflect-metadata';
import { Module, Controller, Get, Post, Param, Req, Body, HttpException, HttpCode, Injectable, OnApplicationShutdown } from '@nestjs/common';
import { NestFactory } from '@nestjs/core';
import { Request, json } from 'express';
import { randomUUID, createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { clientRequestSchema, findCapability, signInternal, CapabilityRegistry, capabilityRegistrySchema, validateAgentResult, CapabilityRequest } from '@lawpilot/contracts';
import { AuthService } from './auth.service';
import { DbService } from './db.service';
import { recordExecutionEvent } from './execution-events';
import { AccessService } from './access.service';

startTelemetry('platform-api');
const REGISTRY: CapabilityRegistry = capabilityRegistrySchema.parse(JSON.parse(readFileSync(process.env.CAPABILITY_REGISTRY_PATH || 'contracts/capabilities/registry.v1.json','utf8')));
function invalid(status:number, message:string):never{throw new HttpException({error:message},status);}

@Controller('v1')
export class ApiController {
  constructor(private auth:AuthService,private access:AccessService,private db:DbService){}
  @Get('healthz') health(){return {service:'platform-api',status:'up',liveProvider:'not-configured'};}

  @Get('tenants/:tenantId/matters/:matterId')
  async matter(@Param('tenantId') tenantId:string,@Param('matterId') matterId:string,@Req() req:Request){
    const principal=await this.auth.subject(req.headers.authorization);
    return this.access.withAccess(tenantId,principal,undefined,matterId,async c=>{
      const r=await c.query('SELECT id,title,status FROM matters WHERE tenant_id=$1 AND id=$2',[tenantId,matterId]);
      if(!r.rowCount) invalid(404,'Matter not found');
      return {matter:r.rows[0]};
    });
  }

  @Post('tenants/:tenantId/ai/execute')
  @HttpCode(200)
  async execute(@Param('tenantId') tenantId:string,@Req() req:Request,@Body() raw:unknown){
    const principal=await this.auth.subject(req.headers.authorization);
    const parsed=clientRequestSchema.safeParse(raw);
    if(!parsed.success) invalid(400,'Invalid AI request schema');
    const input=parsed.data;
    const cap=findCapability(REGISTRY,input.capabilityId);
    if(!cap) invalid(422,'UNSUPPORTED_CAPABILITY');
    if(cap.requiresMatter&&!input.matterId) invalid(400,'Capability requires matterId');
    if(Buffer.byteLength(JSON.stringify(input.input))>cap.maxInputBytes) invalid(413,'AI input too large');
    const requestHash=createHash('sha256').update(JSON.stringify({principal,tenantId,...input})).digest('hex');
    const id=randomUUID();
    const record=await this.access.withAccess(tenantId,principal,cap.id,input.matterId,async c=>{
      const inserted=await c.query(`INSERT INTO ai_executions (id,tenant_id,principal_id,idempotency_key,request_sha256,capability_id,matter_id,status)
        VALUES($1,$2,$3,$4,$5,$6,$7,'accepted') ON CONFLICT (tenant_id,principal_id,idempotency_key) DO NOTHING RETURNING id,status,request_sha256,result`,
        [id,tenantId,principal,input.idempotencyKey,requestHash,input.capabilityId,input.matterId||null]);
      if(inserted.rowCount) {
        await recordExecutionEvent(c,tenantId,principal,id,'accepted','accepted');
        return {row:inserted.rows[0],fresh:true};
      }
      const existing=await c.query('SELECT id,status,request_sha256,result FROM ai_executions WHERE tenant_id=$1 AND principal_id=$2 AND idempotency_key=$3',[tenantId,principal,input.idempotencyKey]);
      return {row:existing.rows[0],fresh:false};
    });
    if(record.row.request_sha256!==requestHash) invalid(409,'Idempotency key reused for different request');
    if(!record.fresh) return {executionId:record.row.id,status:record.row.status,result:record.row.result||null};

    const task:CapabilityRequest={executionId:id,tenantId,principalId:principal,capabilityId:cap.id,input:input.input,traceId:activeTraceId()||randomUUID(),...(input.matterId?{matterId:input.matterId}:{})};
    const body=JSON.stringify(task);const ts=Date.now();
    try{
      const resp=await fetch(`${process.env.SUPERVISOR_URL||'http://localhost:3001'}/internal/execute`,{
        method:'POST',headers:{...internalTraceHeaders(),'content-type':'application/json','x-lawpilot-timestamp':String(ts),'x-lawpilot-signature':signInternal(body,ts,process.env.INTERNAL_HMAC_KEY||'')},body,signal:AbortSignal.timeout(25_000)});
      if(!resp.ok) throw new Error(`Supervisor failed: ${resp.status}`);
      const result=validateAgentResult(await resp.json(),task,cap);
      // Grants may have been revoked while the worker was running. Recheck before storing/serving output.
      await this.access.withAccess(tenantId,principal,cap.id,input.matterId,async c=>{
        await c.query("UPDATE ai_executions SET status='completed',result=$1,completed_at=now() WHERE tenant_id=$2 AND id=$3",[JSON.stringify(result),tenantId,id]);
        await recordExecutionEvent(c,tenantId,principal,id,'completed',result.status);
      });
      return {executionId:id,status:'completed',result};
    }catch(e){
      await this.db.withTenant(tenantId,async c=>{
        await c.query("UPDATE ai_executions SET status='failed',completed_at=now() WHERE tenant_id=$1 AND id=$2",[tenantId,id]);
        await recordExecutionEvent(c,tenantId,principal,id,'failed',e instanceof HttpException && e.getStatus()===403?'access_revoked':'worker_error');
      });
      if(e instanceof HttpException && e.getStatus()===403) throw e;
      invalid(503,'AI service unavailable or invalid result');
    }
  }
}

@Injectable()
class TelemetryLifecycle implements OnApplicationShutdown {
  async onApplicationShutdown(): Promise<void> { await shutdownTelemetry(); }
}

@Module({controllers:[ApiController],providers:[AuthService,DbService,AccessService,TelemetryLifecycle]})
class AppModule{}
async function bootstrap(){
  if(!process.env.KEYCLOAK_ISSUER||!process.env.KEYCLOAK_AUDIENCE||!process.env.INTERNAL_HMAC_KEY||process.env.INTERNAL_HMAC_KEY.length<32||!process.env.DATABASE_URL){throw new Error('Missing required auth/DB env');}
  const app=await NestFactory.create(AppModule,{cors:false,bodyParser:false});
  app.use((req: any,res: any,next: () => void)=>withHttpSpan('platform.request',req,res,next));
  app.use((_req: unknown, res: any, next: () => void) => {
    res.setHeader('Cache-Control','no-store');
    res.setHeader('X-Content-Type-Options','nosniff');
    res.setHeader('X-Request-ID',randomUUID());
    next();
  });
  app.use(json({limit: '32kb', strict: true}));
  app.enableShutdownHooks();
  await app.listen(Number(process.env.API_PORT||3000),'0.0.0.0');
}
if (require.main === module) bootstrap().catch(e=>{console.error(e);process.exit(1)});

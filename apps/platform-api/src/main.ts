import 'reflect-metadata';
import { Module, Controller, Get, Post, Param, Req, Body, HttpException, HttpStatus, Injectable } from '@nestjs/common';
import { NestFactory } from '@nestjs/core';
import { Request } from 'express';
import { randomUUID, createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { clientRequestSchema, agentResultSchema, findCapability, signInternal, CapabilityRegistry, CapabilityRequest } from '@lawpilot/contracts';
import { AuthService } from './auth.service';
import { DbService } from './db.service';

const REGISTRY: CapabilityRegistry = JSON.parse(readFileSync(process.env.CAPABILITY_REGISTRY_PATH || 'contracts/capabilities/registry.v1.json','utf8'));
function invalid(status:number, message:string):never{throw new HttpException({error:message},status);}
function uuid(x:string):boolean{return /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(x);}

@Injectable()
class AccessService {
  constructor(private db:DbService){}
  async authorize(tenantId:string,principalId:string,capabilityId?:string,matterId?:string):Promise<void>{
    if(!uuid(tenantId)||(matterId&&!uuid(matterId))) invalid(400,'Invalid resource identifier');
    await this.db.withTenant(tenantId,async c=>{
      const member=await c.query("SELECT 1 FROM memberships WHERE tenant_id=$1 AND principal_id=$2 AND status='active'",[tenantId,principalId]);
      if(member.rowCount!==1) invalid(403,'Not a member of this tenant');
      if(capabilityId){
        const cap=await c.query('SELECT 1 FROM principal_capabilities WHERE tenant_id=$1 AND principal_id=$2 AND capability_id=$3',[tenantId,principalId,capabilityId]);
        if(cap.rowCount!==1) invalid(403,'Capability not granted');
      }
      if(matterId){
        const access=await c.query("SELECT 1 FROM matter_grants WHERE tenant_id=$1 AND principal_id=$2 AND matter_id=$3 AND permission='matter.read'",[tenantId,principalId,matterId]);
        if(access.rowCount!==1) invalid(403,'Matter access denied');
      }
    });
  }
}

@Controller('v1')
class ApiController {
  constructor(private auth:AuthService,private access:AccessService,private db:DbService){}
  @Get('healthz') health(){return {service:'platform-api',status:'up',liveProvider:'not-configured'};}

  @Get('tenants/:tenantId/matters/:matterId')
  async matter(@Param('tenantId') tenantId:string,@Param('matterId') matterId:string,@Req() req:Request){
    const principal=await this.auth.subject(req.headers.authorization);
    await this.access.authorize(tenantId,principal,undefined,matterId);
    return this.db.withTenant(tenantId,async c=>{
      const r=await c.query('SELECT id,title,status FROM matters WHERE tenant_id=$1 AND id=$2',[tenantId,matterId]);
      if(!r.rowCount) invalid(404,'Matter not found');
      return {matter:r.rows[0]};
    });
  }

  @Post('tenants/:tenantId/ai/execute')
  async execute(@Param('tenantId') tenantId:string,@Req() req:Request,@Body() raw:unknown){
    const principal=await this.auth.subject(req.headers.authorization);
    const parsed=clientRequestSchema.safeParse(raw);
    if(!parsed.success) invalid(400,'Invalid AI request schema');
    const input=parsed.data;
    const cap=findCapability(REGISTRY,input.capabilityId);
    if(!cap) invalid(422,'UNSUPPORTED_CAPABILITY');
    if(cap.requiresMatter&&!input.matterId) invalid(400,'Capability requires matterId');
    if(Buffer.byteLength(JSON.stringify(input.input))>cap.maxInputBytes) invalid(413,'AI input too large');
    await this.access.authorize(tenantId,principal,cap.id,input.matterId);
    const requestHash=createHash('sha256').update(JSON.stringify({principal,tenantId,...input})).digest('hex');
    const id=randomUUID();
    const record=await this.db.withTenant(tenantId,async c=>{
      const inserted=await c.query(`INSERT INTO ai_executions (id,tenant_id,principal_id,idempotency_key,request_sha256,capability_id,matter_id,status)
        VALUES($1,$2,$3,$4,$5,$6,$7,'accepted') ON CONFLICT (tenant_id,principal_id,idempotency_key) DO NOTHING RETURNING id,status,request_sha256,result`,
        [id,tenantId,principal,input.idempotencyKey,requestHash,input.capabilityId,input.matterId||null]);
      if(inserted.rowCount) return {row:inserted.rows[0],fresh:true};
      const existing=await c.query('SELECT id,status,request_sha256,result FROM ai_executions WHERE tenant_id=$1 AND principal_id=$2 AND idempotency_key=$3',[tenantId,principal,input.idempotencyKey]);
      return {row:existing.rows[0],fresh:false};
    });
    if(record.row.request_sha256!==requestHash) invalid(409,'Idempotency key reused for different request');
    if(!record.fresh) return {executionId:record.row.id,status:record.row.status,result:record.row.result||null};

    const task:CapabilityRequest={executionId:id,tenantId,principalId:principal,capabilityId:cap.id,input:input.input,traceId:randomUUID(),...(input.matterId?{matterId:input.matterId}:{})};
    const body=JSON.stringify(task);const ts=Date.now();
    try{
      const resp=await fetch(`${process.env.SUPERVISOR_URL||'http://localhost:3001'}/internal/execute`,{
        method:'POST',headers:{'content-type':'application/json','x-lawpilot-timestamp':String(ts),'x-lawpilot-signature':signInternal(body,ts,process.env.INTERNAL_HMAC_KEY||'')},body,signal:AbortSignal.timeout(25_000)});
      if(!resp.ok) throw new Error(`Supervisor failed: ${resp.status}`);
      const result=agentResultSchema.parse(await resp.json());
      if(result.executionId!==id) throw new Error('Wrong executionId in AI result');
      await this.db.withTenant(tenantId,async c=>{
        await c.query("UPDATE ai_executions SET status='completed',result=$1,completed_at=now() WHERE tenant_id=$2 AND id=$3",[JSON.stringify(result),tenantId,id]);
        await c.query("INSERT INTO audit_events(tenant_id,actor_id,action,resource_id,decision_code) VALUES($1,$2,'ai.result',$3,$4)",[tenantId,principal,id,result.status]);
      });
      return {executionId:id,status:'completed',result};
    }catch(e){
      await this.db.withTenant(tenantId,async c=>c.query("UPDATE ai_executions SET status='failed',completed_at=now() WHERE tenant_id=$1 AND id=$2",[tenantId,id]));
      invalid(503,'AI service unavailable or invalid result');
    }
  }
}

@Module({controllers:[ApiController],providers:[AuthService,DbService,AccessService]})
class AppModule{}
async function bootstrap(){
  if(!process.env.KEYCLOAK_ISSUER||!process.env.KEYCLOAK_AUDIENCE||!process.env.INTERNAL_HMAC_KEY||!process.env.DATABASE_URL){throw new Error('Missing required auth/DB env');}
  const app=await NestFactory.create(AppModule,{cors:false});
  app.enableShutdownHooks();
  await app.listen(Number(process.env.API_PORT||3000),'0.0.0.0');
}
bootstrap().catch(e=>{console.error(e);process.exit(1)});

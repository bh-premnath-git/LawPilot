import { Injectable, HttpException } from '@nestjs/common';
import { createHash, randomUUID } from 'node:crypto';
import { resolve } from 'node:path';
import { readFileSync } from 'node:fs';
import { capabilityRegistrySchema, clientRequestSchema, findCapability } from '@lawpilot/contracts';
import { AccessService } from './access.service';
import { recordExecutionEvent } from './execution-events';

const registry=capabilityRegistrySchema.parse(JSON.parse(readFileSync(process.env.CAPABILITY_REGISTRY_PATH||resolve(__dirname,'../../../contracts/capabilities/registry.v1.json'),'utf8')));
export function canonicalJSON(value: any): string {
  if(Array.isArray(value)) return '['+value.map(canonicalJSON).join(',')+']';
  if(value!==null && typeof value==='object') return '{'+Object.keys(value).sort().map(key=>JSON.stringify(key)+':'+canonicalJSON(value[key])).join(',')+'}';
  return JSON.stringify(value);
}
const terminal=new Set(['completed','rejected','cancelled','failed']);
function fail(status:number,error:string):never {throw new HttpException({error},status);}
@Injectable()
export class ExecutionsService {
  constructor(private access:AccessService){}
  async accept(tenant:string,principal:string,raw:unknown){
    const parsed=clientRequestSchema.safeParse(raw);if(!parsed.success)fail(400,'INVALID_REQUEST');
    const input=parsed.data,cap=findCapability(registry,input.capabilityId);
    if(!cap)fail(422,'UNSUPPORTED_CAPABILITY');
    if(cap.requiresMatter&&!input.matterId)fail(400,'MATTER_REQUIRED');
    if(Buffer.byteLength(JSON.stringify(input.input))>cap.maxInputBytes)fail(413,'INPUT_TOO_LARGE');
    tenant=tenant.toLowerCase();
    const hash=createHash('sha256').update(canonicalJSON({principal,tenant,...input})).digest('hex');
    const id=randomUUID(),workflowId=`lawpilot:${tenant}:ai:${id}`;
    return this.access.withAccess(tenant,principal,cap.id,input.matterId,async c=>{
      const created=await c.query(`INSERT INTO ai_executions(id,tenant_id,principal_id,idempotency_key,request_sha256,capability_id,matter_id,status,request_input,workflow_id)
        VALUES($1,$2,$3,$4,$5,$6,$7,'accepted',$8,$9) ON CONFLICT(tenant_id,principal_id,idempotency_key) DO NOTHING RETURNING id,status,request_sha256`,
        [id,tenant,principal,input.idempotencyKey,hash,cap.id,input.matterId||null,JSON.stringify(input.input),workflowId]);
      if(created.rowCount)await recordExecutionEvent(c,tenant,principal,id,'accepted','accepted');
      const row=created.rowCount?created.rows[0]:(await c.query('SELECT id,status,request_sha256 FROM ai_executions WHERE tenant_id=$1 AND principal_id=$2 AND idempotency_key=$3',[tenant,principal,input.idempotencyKey])).rows[0];
      if(row.request_sha256!==hash)fail(409,'CONFLICT');
      return {executionId:row.id,status:row.status,statusUrl:`/v1/tenants/${tenant}/executions/${row.id}`};
    });
  }
  async inspect(tenant:string,principal:string,id:string,operation?:'cancel'|'retry'){
    if(!/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(id))fail(400,'INVALID_EXECUTION_ID');
    return this.access.withAccess(tenant,principal,undefined,undefined,async c=>{
      const found=await c.query('SELECT * FROM ai_executions WHERE tenant_id=$1 AND id=$2 AND principal_id=$3 FOR UPDATE',[tenant,id,principal]);
      if(!found.rowCount)fail(404,'NOT_FOUND');const row=found.rows[0];
      await this.access.check(c,tenant,principal,row.capability_id,row.matter_id||undefined);
      if(operation==='cancel'&&!terminal.has(row.status)){
        await c.query("UPDATE ai_executions SET status='cancelled',result=NULL,error_code=NULL,updated_at=now(),completed_at=now() WHERE tenant_id=$1 AND id=$2",[tenant,id]);
        await recordExecutionEvent(c,tenant,principal,id,'cancelled','user_cancelled');row.status='cancelled';row.result=null;row.error_code=null;row.updated_at=new Date();
      }
      if(operation==='retry'){
        if(terminal.has(row.status))fail(409,'CONFLICT');
        const retried=await c.query("UPDATE outbox_events SET publish_state='pending',delivery_attempts=0,available_at=now(),last_error_code=NULL,lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL WHERE tenant_id=$1 AND aggregate_id=$2 AND event_type='lawpilot.ai.accepted.v1' AND publish_state='dead_letter' RETURNING id",[tenant,id]);
        if(!retried.rowCount)fail(409,'CONFLICT');
        await c.query("INSERT INTO audit_events(tenant_id,actor_id,action,resource_id,decision_code) VALUES($1,$2,'ai.dispatch_retry',$3,'owner_retry')",[tenant,principal,id]);
      }
      if(row.matter_id && row.result?.status==='completed' && (findCapability(registry,row.capability_id)?.requiresMatter ?? true)){
        const verified=await c.query('SELECT verify_ai_execution_result($1,$2,$3::jsonb) AS verified',[tenant,id,JSON.stringify(row.result)]);
        if(!verified.rows[0]?.verified){row.result=null;row.error_code='EVIDENCE_STALE_OR_UNAUTHORIZED';}
      }
      const delivery=await c.query("SELECT publish_state,delivery_attempts,last_error_code,available_at FROM outbox_events WHERE tenant_id=$1 AND aggregate_id=$2 AND event_type='lawpilot.ai.accepted.v1'",[tenant,id]);
      return {executionId:row.id,status:row.status,result:row.result,errorCode:row.error_code,activityAttempts:row.activity_attempts,updatedAt:row.updated_at,delivery:delivery.rows[0]||null};
    });
  }
}

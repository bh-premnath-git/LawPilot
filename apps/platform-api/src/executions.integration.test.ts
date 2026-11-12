import 'reflect-metadata';
import {test} from 'node:test';
import assert from 'node:assert/strict';
import {randomUUID} from 'node:crypto';
import {Pool} from 'pg';
import {DbService} from './db.service';
import {AccessService} from './access.service';
import {ExecutionsService} from './executions.service';

test('durable acceptance, privacy, idempotency, cancellation and revoked grants',{skip:!process.env.TEST_DATABASE_URL},async()=>{
 const admin=new Pool({connectionString:process.env.TEST_DATABASE_URL});
 process.env.DATABASE_URL=process.env.TEST_APP_DATABASE_URL;
 const db=new DbService(),service=new ExecutionsService(new AccessService(db));
 const tenant=randomUUID(),principal='execution-test-'+randomUUID(),other='other-'+randomUUID();
 try{
  await admin.query('INSERT INTO tenants(id,name) VALUES($1,$2)',[tenant,'synthetic execution test']);
  await admin.query("INSERT INTO memberships(tenant_id,principal_id,status) VALUES($1,$2,'active'),($1,$3,'active')",[tenant,principal,other]);
  await admin.query("INSERT INTO principal_capabilities(tenant_id,principal_id,capability_id) VALUES($1,$2,'intake.summarize.v1'),($1,$3,'intake.summarize.v1')",[tenant,principal,other]);
  const input={capabilityId:'intake.summarize.v1',idempotencyKey:'durable-test-001',input:{answers:{secret:'DO_NOT_EXPORT',second:1}}};
  const first=await service.accept(tenant,principal,input);
  assert.equal(first.status,'accepted');assert.match(first.statusUrl,/\/executions\//);
  const repeat=await service.accept(tenant,principal,{...input,input:{answers:{second:1,secret:'DO_NOT_EXPORT'}}});
  assert.equal(repeat.executionId,first.executionId);
  await assert.rejects(service.accept(tenant,principal,{...input,input:{answers:{changed:true}}}),{status:409});
  await assert.rejects(service.inspect(tenant,other,first.executionId),{status:404});
  const record=(await admin.query('SELECT * FROM ai_executions WHERE id=$1',[first.executionId])).rows[0];
  assert.equal(record.request_input.answers.secret,'DO_NOT_EXPORT');
  const events=(await admin.query('SELECT event_body FROM outbox_events WHERE aggregate_id=$1',[first.executionId])).rows;
  assert.equal(events.length,1);assert(!JSON.stringify(events).includes('DO_NOT_EXPORT'));
  assert.equal((await service.inspect(tenant,principal,first.executionId,'cancel')).status,'cancelled');
  await service.inspect(tenant,principal,first.executionId,'cancel');
  assert.equal(Number((await admin.query("SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='lawpilot.ai.cancelled.v1'",[first.executionId])).rows[0].count),1);
  const dead=await service.accept(tenant,principal,{...input,idempotencyKey:'dead-letter-test-002'});
  await admin.query("UPDATE outbox_events SET publish_state='dead_letter',delivery_attempts=10,last_error_code='TEMPORAL_UNAVAILABLE' WHERE aggregate_id=$1",[dead.executionId]);
  const retry=await service.inspect(tenant,principal,dead.executionId,'retry');
  assert.equal(retry.delivery.publish_state,'pending');assert.equal(retry.delivery.delivery_attempts,0);
  await assert.rejects(service.inspect(tenant,principal,dead.executionId,'retry'),{status:409});
  await admin.query("UPDATE memberships SET status='revoked' WHERE tenant_id=$1 AND principal_id=$2",[tenant,principal]);
  await assert.rejects(service.inspect(tenant,principal,first.executionId),{status:403});
 }finally{
  for(const table of ['outbox_events','audit_events','ai_executions','principal_capabilities','memberships'])await admin.query(`DELETE FROM ${table} WHERE tenant_id=$1`,[tenant]);
  await admin.query('DELETE FROM tenants WHERE id=$1',[tenant]);await db.onModuleDestroy();await admin.end();
 }
});

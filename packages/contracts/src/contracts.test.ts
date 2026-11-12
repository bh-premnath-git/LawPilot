import test from 'node:test';
import assert from 'node:assert/strict';
import { signInternal, verifyInternal, capabilityRequestSchema, agentResultSchema, capabilityRegistrySchema, validateAgentResult } from './index';
const key='local-test-only-key-must-be-longer-than-32';
test('internal HMAC validates exact payload; not a forged tenant',()=>{
 const body=JSON.stringify({tenantId:'tenant-a',operation:'read'});
 const now=Date.now();const sig=signInternal(body,now,key);
 assert.equal(verifyInternal(body,String(now),sig,key,now),true);
 assert.equal(verifyInternal(body.replace('tenant-a','tenant-b'),String(now),sig,key,now),false);
 assert.equal(verifyInternal(body,String(now-120_000),sig,key,now),false);
});
test('requests reject unknown fields',()=>{
 assert.equal(capabilityRequestSchema.safeParse({foo:1}).success,false);
});
test('results require evidence and attorney review flag',()=>{
 assert.equal(agentResultSchema.safeParse({status:'completed'}).success,false);
});

test('registry rejects duplicate capabilities and invalid limits', () => {
 const cap={id:'intake.summarize.v1',agentId:'intake-agent',agentVersion:'0.1.0',requiresMatter:false,requiresAttorneyReview:true,maxInputBytes:8192};
 assert.equal(capabilityRegistrySchema.safeParse({registryVersion:'1.0',capabilities:[cap]}).success,true);
 assert.equal(capabilityRegistrySchema.safeParse({registryVersion:'1.0',capabilities:[cap,cap]}).success,false);
 assert.equal(capabilityRegistrySchema.safeParse({registryVersion:'1.0',capabilities:[{...cap,maxInputBytes:-1}]}).success,false);
});
test('delegated results cannot change agent identity, skip review or propose actions', () => {
 const task={executionId:'11111111-1111-4111-8111-111111111111',tenantId:'22222222-2222-4222-8222-222222222222',principalId:'user',capabilityId:'intake.summarize.v1',input:{},traceId:'trace'};
 const cap={id:task.capabilityId,agentId:'intake-agent',agentVersion:'0.1.0',requiresMatter:false,requiresAttorneyReview:true,maxInputBytes:8192};
 const result={executionId:task.executionId,agentId:cap.agentId,agentVersion:cap.agentVersion,status:'abstained',output:{},evidence:[],proposedActions:[],requiresAttorneyReview:true};
 assert.equal(validateAgentResult(result,task,cap).status,'abstained');
 for(const change of [{agentId:'other'}, {agentVersion:'9.0.0'}, {executionId:'other'}, {requiresAttorneyReview:false}, {proposedActions:[{type:'write'}]}]) {
  assert.throws(()=>validateAgentResult({...result,...change},task,cap));
 }
});
test('a completed matter result requires source evidence', () => {
 const task={executionId:'11111111-1111-4111-8111-111111111111',tenantId:'22222222-2222-4222-8222-222222222222',principalId:'user',matterId:'33333333-3333-4333-8333-333333333333',capabilityId:'matter.qa.v1',input:{},traceId:'trace'};
 const cap={id:task.capabilityId,agentId:'case-qa-agent',agentVersion:'0.1.0',requiresMatter:true,requiresAttorneyReview:false,maxInputBytes:8192};
 assert.throws(()=>validateAgentResult({executionId:task.executionId,agentId:cap.agentId,agentVersion:cap.agentVersion,status:'completed',output:{},evidence:[],proposedActions:[],requiresAttorneyReview:false},task,cap),/evidence/);
});

import test from 'node:test';
import assert from 'node:assert/strict';
import { signInternal, verifyInternal, capabilityRequestSchema, agentResultSchema } from './index';
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

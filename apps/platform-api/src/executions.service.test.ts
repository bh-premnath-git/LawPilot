import 'reflect-metadata';
import {test} from 'node:test';
import assert from 'node:assert/strict';
import {canonicalJSON,ExecutionsService} from './executions.service';
import {AccessService} from './access.service';
test('idempotency canonicalization ignores object key order but preserves arrays',()=>{
 assert.equal(canonicalJSON({b:2,a:{y:1,x:0}}),canonicalJSON({a:{x:0,y:1},b:2}));
 assert.notEqual(canonicalJSON([1,2]),canonicalJSON([2,1]));
});
test('accept rejects client-supplied identity and invalid capabilities before any database call',async()=>{
 const access={withAccess:()=>assert.fail('unexpected DB access')} as unknown as AccessService;
 const service=new ExecutionsService(access);
 const body={capabilityId:'intake.summarize.v1',idempotencyKey:'test-key-001',input:{}};
 await assert.rejects(service.accept('11111111-1111-4111-8111-111111111111','user',{...body,principalId:'admin'}),{status:400});
 await assert.rejects(service.accept('11111111-1111-4111-8111-111111111111','user',{...body,capabilityId:'arbitrary.execute.v1'}),{status:422});
});

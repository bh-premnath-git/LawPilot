// Opt-in synthetic integration test. See docs/DURABLE_EXECUTIONS.md.
const assert=require('node:assert/strict');
const {spawn,execFile}=require('node:child_process');
const {promisify}=require('node:util');
const fs=require('node:fs'),path=require('node:path'),http=require('node:http');
const {randomUUID,randomBytes,createHmac,timingSafeEqual}=require('node:crypto');
const {Pool}=require('pg');
const exec=promisify(execFile),sleep=ms=>new Promise(r=>setTimeout(r,ms));
for(const variable of ['TEST_DATABASE_URL','TEST_APP_DATABASE_URL','TEST_WORKFLOW_DATABASE_URL','TEST_DISPATCHER_DATABASE_URL'])assert(process.env[variable],variable+' required');
const children=[],pool=new Pool({connectionString:process.env.TEST_DATABASE_URL});
const tenant=randomUUID(),principal=randomUUID(),other=randomUUID(),matter=randomUUID();
fs.mkdirSync('work',{recursive:true});const scratch=fs.mkdtempSync(path.resolve('work/recovery-'));
const temporalBin=process.env.TEMPORAL_BIN||'temporal',workerBin=path.resolve(process.env.WORKER_BIN||'work/execution-worker'),dispatcherBin=path.resolve(process.env.DISPATCHER_BIN||'work/outbox-dispatcher');
const secret=randomBytes(32).toString('hex');
let jwksServer,supervisor,temporal,api,worker,dispatcher,apiPort,temporalPort,token,otherToken,issuer,supervisorPort;
const calls=new Map();let blockNext=false,invalidNext=false,blockedResponse;
function launch(cmd,args,name,extra={}){
 const fd=fs.openSync(path.join(scratch,name+'.log'),'a');
 const child=spawn(cmd,args,{cwd:process.cwd(),env:{...process.env,LEGAL_AI_ENABLED:'false',INTEGRATION_GRPC_ADDRESS:'',ENABLE_INTEGRATION_JOBS:'false',NODE_ENV:'test',TEMPORAL_NAMESPACE:'default',INTERNAL_HMAC_KEY:secret,OTEL_EXPORTER_OTLP_ENDPOINT:'',...extra},stdio:['ignore',fd,fd]});fs.closeSync(fd);children.push(child);return child;
}
async function stop(child,signal='SIGTERM'){
 if(!child||child.exitCode!==null||child.signalCode!==null)return;
 child.kill(signal);await new Promise(resolve=>{const timer=setTimeout(()=>{child.kill('SIGKILL');resolve();},8000);child.once('exit',()=>{clearTimeout(timer);resolve();});});
}
async function until(action,timeout=60000){const end=Date.now()+timeout;let last;while(Date.now()<end){try{const result=await action();if(result)return result;}catch(e){last=e;}await sleep(200);}throw last||Error('integration timeout');}
async function port(){const s=http.createServer();await new Promise(r=>s.listen(0,'127.0.0.1',r));const p=s.address().port;await new Promise(r=>s.close(r));return p;}
function startAPI(){api=launch(process.execPath,['apps/platform-api/dist/main.js'],'api',{API_PORT:String(apiPort),DATABASE_URL:process.env.TEST_APP_DATABASE_URL,KEYCLOAK_ISSUER:issuer,KEYCLOAK_AUDIENCE:'lawpilot-api',KEYCLOAK_JWKS_URL:issuer+'/certs'});}
function startWorker(){worker=launch(workerBin,[],'worker',{APP_ENV:'development',ENABLE_LEGACY_INTERNAL_HTTP:'true',SUPERVISOR_GRPC_ADDRESS:'',CALENDAR_GRPC_ADDRESS:'',ENABLE_CALENDAR_REMINDERS:'false',WORKFLOW_DATABASE_URL:process.env.TEST_WORKFLOW_DATABASE_URL,TEMPORAL_ADDRESS:`127.0.0.1:${temporalPort}`,TEMPORAL_TASK_QUEUE:'test-'+tenant,SUPERVISOR_URL:`http://127.0.0.1:${supervisorPort}`});}
function startDispatcher(){dispatcher=launch(dispatcherBin,[],'dispatcher',{DISPATCHER_DATABASE_URL:process.env.TEST_DISPATCHER_DATABASE_URL,WORKFLOW_DATABASE_URL:process.env.TEST_WORKFLOW_DATABASE_URL,TEMPORAL_ADDRESS:`127.0.0.1:${temporalPort}`,TEMPORAL_TASK_QUEUE:'test-'+tenant});}
async function startTemporal(){temporal=launch(temporalBin,['server','start-dev','--ip','127.0.0.1','--port',String(temporalPort),'--ui-port',String(await port()),'--db-filename',path.join(scratch,'temporal.db')],'temporal');await until(async()=>{await exec(temporalBin,['operator','cluster','health','--address',`127.0.0.1:${temporalPort}`],{timeout:3000});return true;});}
async function ready(){await until(async()=>{const r=await fetch(`http://127.0.0.1:${apiPort}/v1/readyz`);return r.ok;});}
async function request(method,url,body,credential=token){return fetch(`http://127.0.0.1:${apiPort}${url}`,{method,headers:{Authorization:'Bearer '+credential,'Content-Type':'application/json'},...(body?{body:JSON.stringify(body)}:{})});}
async function accept(key){const r=await request('POST',`/v1/tenants/${tenant}/executions`,{capabilityId:'matter.qa.v1',matterId:matter,idempotencyKey:key,input:{question:'PRIVATE_LEGAL_TEST_SENTINEL'}});assert.equal(r.status,202,await r.clone().text());const value=await r.json();assert.equal(r.headers.get('location'),value.statusUrl);return value;}
async function state(value,wanted,timeout=60000){return until(async()=>{const r=await request('GET',value.statusUrl);assert.equal(r.status,200);const body=await r.json();return body.status===wanted?body:false;},timeout);}
async function delivery(value){return until(async()=>{const row=(await pool.query("SELECT publish_state FROM outbox_events WHERE tenant_id=$1 AND aggregate_id=$2 AND event_type='lawpilot.ai.accepted.v1'",[tenant,value.executionId])).rows[0];return row.publish_state==='delivered';});}
let integrationLock;
(async()=>{
 integrationLock=await pool.connect();
 await integrationLock.query("SELECT pg_advisory_lock(604182701)");
 for(const variable of ['TEST_DATABASE_URL','TEST_APP_DATABASE_URL','TEST_WORKFLOW_DATABASE_URL','TEST_DISPATCHER_DATABASE_URL'])assert(process.env[variable],variable+' required');
 const {generateKeyPair,exportJWK,SignJWT}=await import('jose');const {publicKey,privateKey}=await generateKeyPair('RS256');
 const key={...await exportJWK(publicKey),kid:'recovery-test',alg:'RS256',use:'sig'};
 jwksServer=http.createServer((_req,res)=>{res.setHeader('Content-Type','application/json');res.end(JSON.stringify({keys:[key]}));});
 await new Promise(r=>jwksServer.listen(0,'127.0.0.1',r));issuer=`http://127.0.0.1:${jwksServer.address().port}/realm`;
 async function jwt(sub){return new SignJWT({typ:'Bearer'}).setProtectedHeader({alg:'RS256',kid:key.kid}).setSubject(sub).setIssuer(issuer).setAudience('lawpilot-api').setIssuedAt().setExpirationTime('15m').sign(privateKey);}
 token=await jwt(principal);otherToken=await jwt(other);
 supervisor=http.createServer(async(req,res)=>{
  const chunks=[];for await(const chunk of req)chunks.push(chunk);const raw=Buffer.concat(chunks);
  const timestamp=req.headers['x-lawpilot-timestamp'],actual=req.headers['x-lawpilot-signature']||'';
  const expected=createHmac('sha256',secret).update(timestamp+'.').update(raw).digest('hex');
  if(actual.length!==expected.length||!timingSafeEqual(Buffer.from(actual),Buffer.from(expected))){res.writeHead(401);res.end();return;}
  const task=JSON.parse(raw);calls.set(task.executionId,(calls.get(task.executionId)||0)+1);
  const result={executionId:task.executionId,agentId:'case-qa-agent',agentVersion:'0.1.0',status:'abstained',output:{kind:'safe-test'},evidence:[],proposedActions:[],requiresAttorneyReview:false};
  if(invalidNext){invalidNext=false;result.agentId='forged-agent';}
  const send=()=>{if(!res.destroyed){res.setHeader('Content-Type','application/json');res.end(JSON.stringify(result));}};
  if(blockNext){blockNext=false;blockedResponse=send;}else send();
 });await new Promise(r=>supervisor.listen(0,'127.0.0.1',r));supervisorPort=supervisor.address().port;
 await pool.query("INSERT INTO tenants(id,name)VALUES($1,'synthetic recovery test')",[tenant]);
 await pool.query("INSERT INTO memberships(tenant_id,principal_id,status)VALUES($1,$2,'active'),($1,$3,'active')",[tenant,principal,other]);
 await pool.query("INSERT INTO principal_capabilities(tenant_id,principal_id,capability_id)VALUES($1,$2,'matter.qa.v1'),($1,$3,'matter.qa.v1')",[tenant,principal,other]);
 await pool.query("INSERT INTO matters(id,tenant_id,title,resource_owner)VALUES($1,$2,'synthetic','mock')",[matter,tenant]);
 await pool.query("INSERT INTO matter_grants(tenant_id,matter_id,principal_id,permission)VALUES($1,$2,$3,'matter.read')",[tenant,matter,principal]);
 apiPort=await port();temporalPort=await port();startAPI();await ready();
 const accepted=await accept('restart-api-001');assert.equal(accepted.status,'accepted');
 await stop(api,'SIGKILL');startAPI();await ready();assert.equal((await accept('restart-api-001')).executionId,accepted.executionId);
 await startTemporal();startDispatcher();await delivery(accepted);await state(accepted,'queued');
 await stop(dispatcher,'SIGKILL');startDispatcher();startWorker();await state(accepted,'completed');assert.equal(calls.get(accepted.executionId),1);
 console.log('PASS: API crash, offline worker queue, dispatcher restart and stable workflow ID');
 blockNext=true;const mid=await accept('restart-worker-002');await until(()=>calls.get(mid.executionId)===1);await stop(worker,'SIGKILL');blockedResponse();startWorker();await state(mid,'completed',180000);
 const completions=await pool.query("SELECT count(*) FROM audit_events WHERE tenant_id=$1 AND resource_id=$2 AND action='ai.completed'",[tenant,mid.executionId]);assert.equal(Number(completions.rows[0].count),1);
 console.log('PASS: worker killed mid-activity; retried read-only call, one committed result');
 await stop(worker,'SIGKILL');const persisted=await accept('restart-temporal-003');await delivery(persisted);await stop(temporal,'SIGKILL');await startTemporal();startWorker();await state(persisted,'completed');
 console.log('PASS: Temporal restart from durable SQLite retains queued workflow');
 await stop(dispatcher,'SIGKILL');const cancelled=await accept('cancel-before-start-004');assert.equal((await request('POST',cancelled.statusUrl+'/cancel')).status,202);startDispatcher();await delivery(cancelled);await state(cancelled,'cancelled');await sleep(1200);assert.equal(calls.get(cancelled.executionId)||0,0);
 assert.equal((await request('GET',accepted.statusUrl,undefined,otherToken)).status,404);
 invalidNext=true;const forged=await accept('invalid-result-005');const rejected=await state(forged,'failed');assert.equal(rejected.errorCode,'INVALID_RESULT');
 await stop(worker,'SIGKILL');const revoked=await accept('revoke-before-work-006');await pool.query("UPDATE memberships SET status='revoked' WHERE tenant_id=$1 AND principal_id=$2",[tenant,principal]);startWorker();
 await until(async()=>{const r=(await pool.query('SELECT status,error_code FROM ai_executions WHERE id=$1',[revoked.executionId])).rows[0];return r.status==='failed'&&r.error_code==='PERMISSION_DENIED';});
 assert.equal(calls.get(revoked.executionId)||0,0);assert.equal((await request('GET',accepted.statusUrl)).status,403);
 const events=(await pool.query('SELECT event_body FROM outbox_events WHERE tenant_id=$1',[tenant])).rows;assert(!JSON.stringify(events).includes('PRIVATE_LEGAL_TEST_SENTINEL'));
 const history=await exec(temporalBin,['workflow','show','--workflow-id',`lawpilot:${tenant}:ai:${accepted.executionId}`,'--address',`127.0.0.1:${temporalPort}`,'--output','json'],{maxBuffer:5e6});const inspectPayload=value=>{
  if(typeof value==='string'){
   assert(!value.includes('PRIVATE_LEGAL_TEST_SENTINEL'));
   assert(!Buffer.from(value,'base64').toString('utf8').includes('PRIVATE_LEGAL_TEST_SENTINEL'));
  }else if(value&&typeof value==='object')Object.values(value).forEach(inspectPayload);
 };inspectPayload(JSON.parse(history.stdout));
 console.log('PASS: cancellation, revoked grants, owner-only status, forged results and metadata-only outbox/history');
})().catch(e=>{console.error(e);console.error('Test logs:',scratch);process.exitCode=1;}).finally(async()=>{
 if(blockedResponse)blockedResponse();for(const child of children.reverse())await stop(child);
 for(const server of [supervisor,jwksServer])if(server){server.closeAllConnections();server.close();}
 await pool.query('DELETE FROM domain_event_deliveries WHERE event_id IN(SELECT id FROM domain_event_journal WHERE tenant_id=$1)',[tenant]);
 for(const table of ['domain_event_inbox','domain_aggregate_heads','domain_event_journal','outbox_events','audit_events','ai_executions','principal_capabilities','matter_grants','matters','memberships'])await pool.query(`DELETE FROM ${table} WHERE tenant_id=$1`,[tenant]);
 await pool.query('DELETE FROM tenants WHERE id=$1',[tenant]);if(integrationLock){await integrationLock.query("SELECT pg_advisory_unlock(604182701)");integrationLock.release();}await pool.end();
});

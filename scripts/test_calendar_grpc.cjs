// Synthetic real-process integration: API -> Go calendar and Go -> TS -> Python via mTLS.
const assert=require('node:assert/strict'),fs=require('node:fs'),path=require('node:path'),http=require('node:http');
const {spawn,execFile}=require('node:child_process'),{promisify}=require('node:util'),{randomUUID}=require('node:crypto');
const {Pool}=require('pg'),grpc=require('@grpc/grpc-js');
const {Calendar,Capability}=require('../packages/rpc/dist');
const exec=promisify(execFile),sleep=ms=>new Promise(r=>setTimeout(r,ms));
for(const name of ['TEST_DATABASE_URL','TEST_APP_DATABASE_URL','TEST_CALENDAR_DATABASE_URL','TEST_WORKFLOW_DATABASE_URL','TEST_DISPATCHER_DATABASE_URL'])assert(process.env[name],name+' required');
const pool=new Pool({connectionString:process.env.TEST_DATABASE_URL}),tenant=randomUUID(),owner=randomUUID(),client=randomUUID(),children=[],channels=[];
const scratch=fs.mkdtempSync(path.resolve('work/calendar-grpc-')),tls=path.join(scratch,'tls');let jwks;
async function port(){const s=http.createServer();await new Promise(r=>s.listen(0,'127.0.0.1',r));const p=s.address().port;await new Promise(r=>s.close(r));return p;}
async function until(action,timeout=60000){const end=Date.now()+timeout;let last;while(Date.now()<end){try{const result=await action();if(result)return result;}catch(e){last=e;}await sleep(150);}throw last||Error('integration timeout');}
function launch(command,args,name,env={}){const fd=fs.openSync(path.join(scratch,name+'.log'),'a');const child=spawn(command,args,{env:{...process.env,LEGAL_AI_ENABLED:'false',INTEGRATION_GRPC_ADDRESS:'',ENABLE_INTEGRATION_JOBS:'false',NODE_ENV:'test',APP_ENV:'development',OTEL_EXPORTER_OTLP_ENDPOINT:'',ENABLE_LEGACY_INTERNAL_HTTP:'false',...env},stdio:['ignore',fd,fd]});fs.closeSync(fd);children.push(child);return child;}
function identity(name){return {GRPC_CA_FILE:path.join(tls,'ca.pem'),GRPC_CLIENT_CERT_FILE:path.join(tls,name+'.pem'),GRPC_CLIENT_KEY_FILE:path.join(tls,name+'.key'),GRPC_SERVER_CERT_FILE:path.join(tls,name+'.pem'),GRPC_SERVER_KEY_FILE:path.join(tls,name+'.key')};}
function credentials(name){return grpc.credentials.createSsl(fs.readFileSync(path.join(tls,'ca.pem')),fs.readFileSync(path.join(tls,name+'.key')),fs.readFileSync(path.join(tls,name+'.pem')));}
function channel(Type,address,role,server){const result=new Type(address,credentials(role),{'grpc.ssl_target_name_override':server});channels.push(result);return result;}
function rpc(stub,method,body,token){const metadata=new grpc.Metadata();if(token)metadata.set('authorization','Bearer '+token);return new Promise((resolve,reject)=>stub[method](body,metadata,{deadline:Date.now()+5000},(err,result)=>err?reject(err):resolve(result)));}
let integrationLock;
(async()=>{
 integrationLock=await pool.connect();
 await integrationLock.query("SELECT pg_advisory_lock(604182701)");
 await exec('bash',['scripts/generate_dev_tls.sh',tls]);
 const {generateKeyPair,exportJWK,SignJWT}=await import('jose'),{publicKey,privateKey}=await generateKeyPair('RS256');
 const key={...await exportJWK(publicKey),kid:'synthetic',alg:'RS256',use:'sig'};
 jwks=http.createServer((req,res)=>{res.setHeader('Content-Type','application/json');res.end(JSON.stringify({keys:[key]}));});await new Promise(r=>jwks.listen(0,'127.0.0.1',r));const issuer=`http://127.0.0.1:${jwks.address().port}/realm`;
 const jwt=sub=>new SignJWT({typ:'Bearer'}).setProtectedHeader({alg:'RS256',kid:'synthetic'}).setIssuer(issuer).setAudience('lawpilot-api').setSubject(sub).setIssuedAt().setExpirationTime('10m').sign(privateKey);
 const token=await jwt(owner),clientToken=await jwt(client),calendarPort=await port(),apiPort=await port(),supervisorPort=await port(),aiPort=await port(),temporalPort=await port();
 const address=`127.0.0.1:${calendarPort}`,supervisorAddress=`127.0.0.1:${supervisorPort}`;
 const auth={KEYCLOAK_ISSUER:issuer,KEYCLOAK_JWKS_URL:issuer+'/certs',KEYCLOAK_AUDIENCE:'lawpilot-api'};
 await pool.query("INSERT INTO tenants(id,name)VALUES($1,'synthetic grpc')",[tenant]);
 for(const who of [owner,client])await pool.query("INSERT INTO memberships(tenant_id,principal_id,status)VALUES($1,$2,'active')",[tenant,who]);
 for(const capability of ['calendar.manage','calendar.read','calendar.write','calendar.approve','calendar.availability','intake.summarize.v1'])await pool.query('INSERT INTO principal_capabilities(tenant_id,principal_id,capability_id)VALUES($1,$2,$3)',[tenant,owner,capability]);
 await pool.query("INSERT INTO principal_capabilities(tenant_id,principal_id,capability_id)VALUES($1,$2,'calendar.client_read')",[tenant,client]);
 launch(path.resolve(process.env.CALENDAR_BIN||'work/calendar-service'),[],'calendar',{...identity('calendar-service'),...auth,CALENDAR_DATABASE_URL:process.env.TEST_CALENDAR_DATABASE_URL,CALENDAR_GRPC_ADDRESS:address});
 const gateway=channel(Calendar.CalendarServiceClient,address,'api-gateway','calendar-service');
 await until(()=>rpc(gateway,'listCalendars',{scope:{tenantId:tenant,requestId:randomUUID()}},token));
 await assert.rejects(rpc(gateway,'listCalendars',{scope:{tenantId:tenant}},undefined),e=>e.code===grpc.status.UNAUTHENTICATED);
 await assert.rejects(rpc(gateway,'listCalendars',{scope:{tenantId:tenant}},'invalid'),e=>e.code===grpc.status.UNAUTHENTICATED);
 const wrong=channel(Calendar.CalendarServiceClient,address,'root-supervisor','calendar-service');
 await assert.rejects(rpc(wrong,'listCalendars',{scope:{tenantId:tenant}},token),e=>e.code===grpc.status.PERMISSION_DENIED);
 await assert.rejects(rpc(gateway,'loadReminderWork',{tenantId:tenant,reminderId:randomUUID()}),e=>e.code===grpc.status.PERMISSION_DENIED);
 launch(process.execPath,['apps/platform-api/dist/main.js'],'api',{...identity('api-gateway'),...auth,DATABASE_URL:process.env.TEST_APP_DATABASE_URL,API_PORT:String(apiPort),CALENDAR_GRPC_ADDRESS:address});
 await until(async()=>{const r=await fetch(`http://127.0.0.1:${apiPort}/v1/readyz`);return r.ok;});
 async function request(method,route,body,expected=200,credential=token){const r=await fetch(`http://127.0.0.1:${apiPort}/v1/tenants/${tenant}/${route}`,{method,headers:{Authorization:'Bearer '+credential,'Content-Type':'application/json'},...(body?{body:JSON.stringify(body)}:{})});const value=await r.json();assert.equal(r.status,expected,JSON.stringify(value));return value;}
 const created=await request('POST','calendars',{name:'Synthetic native',defaultTimeZone:'America/New_York',approvalRequired:false,overlapPolicy:'exclusive',idempotencyKey:randomUUID()},201),calendarId=created.calendar.id;
 await request('PUT',`calendars/${calendarId}/grants/${client}`,{permissions:['read'],expectedRevision:created.calendar.revision,idempotencyKey:randomUUID()});
 const start=new Date(Date.now()+65000),end=new Date(start.getTime()+3600000);
 const entry={title:'Synthetic appointment',description:'SYNTHETIC_PRIVATE_NOTES',kind:'appointment',visibility:'shared',schedule:{type:'timed',start:start.toISOString(),end:end.toISOString(),timeZone:'America/New_York'},participantPrincipalIds:[owner,client],reminders:[{minutesBefore:1}]};
 const command={calendarId,entry,idempotencyKey:randomUUID()};const mutation=await request('POST','calendar-entries',command,201),entryId=mutation.result.entryIds[0];
 assert.deepEqual(await request('POST','calendar-entries',command,201),mutation);
 const listed=await request('GET',`calendar-entries?calendarId=${calendarId}&from=${encodeURIComponent(start.toISOString())}&to=${encodeURIComponent(end.toISOString())}`);assert.equal(listed.entries.length,1);
 await request('POST','calendar-entries',{...command,idempotencyKey:randomUUID()},409);
 const firmCalendar=await request('POST','calendars',{name:'Shared firm',defaultTimeZone:'UTC',approvalRequired:false,idempotencyKey:randomUUID()},201);
 assert.equal(firmCalendar.calendar.overlapPolicy,'shared');
 const sharedCommand={calendarId:firmCalendar.calendar.id,entry:{...entry,reminders:[]},idempotencyKey:randomUUID()};
 await request('POST','calendar-entries',sharedCommand,201);
 await request('POST','calendar-entries',{...sharedCommand,idempotencyKey:randomUUID()},201);
 await request('PATCH',`calendars/${firmCalendar.calendar.id}/overlap-policy`,{overlapPolicy:'exclusive',expectedRevision:firmCalendar.calendar.revision,idempotencyKey:randomUUID()},409);
 const occupancy=await request('POST','calendar-availability/query',{calendarId:firmCalendar.calendar.id,from:start.toISOString(),to:end.toISOString()});
 assert.equal(occupancy.overlapPolicy,'shared');assert(occupancy.warnings.some(x=>x.includes('occupancy')));
 const visible=await request('GET',`calendar-entries/${entryId}?calendarId=${calendarId}`,undefined,200,clientToken);
 assert.equal(visible.entry.description,'');assert.deepEqual(visible.entry.participantPrincipalIds,[client]);
 await request('PATCH',`calendar-entries/${entryId}`,{calendarId,entry,expectedRevision:99,scope:'occurrence',idempotencyKey:randomUUID()},409);
 const sync=await request('GET',`calendar-sync/status?calendarId=${calendarId}`);assert.equal(sync.state,'native_only');
 // Real Python server and TypeScript supervisor, invoked by the real Go Temporal worker.
 launch(process.env.RPC_PYTHON||'python3',['-m','app.grpc_server'],'python',{...identity('ai-capabilities'),PYTHONPATH:path.resolve('services/ai-capabilities'),RPC_PYTHON_PATH:path.resolve('contracts/gen/python'),AI_GRPC_BIND:`127.0.0.1:${aiPort}`,AI_PORT:String(await port())});
 launch(process.execPath,['services/root-supervisor/dist/main.js'],'supervisor',{...identity('root-supervisor'),AI_GRPC_ADDRESS:`127.0.0.1:${aiPort}`,SUPERVISOR_GRPC_BIND:supervisorAddress,SUPERVISOR_PORT:String(await port())});
 const trusted=channel(Capability.SupervisorServiceClient,supervisorAddress,'execution-worker','root-supervisor');
 const task=Capability.ExecuteCapabilityRequest.fromPartial({executionId:randomUUID(),tenantId:tenant,principalId:owner,capabilityId:'intake.summarize.v1',input:{answers:{summary:'synthetic test'}},traceId:randomUUID()});
 const result=await until(()=>rpc(trusted,'executeCapability',task));assert.equal(result.executionId,task.executionId);assert.equal(result.agentId,'intake-agent');
 const denied=channel(Capability.SupervisorServiceClient,supervisorAddress,'api-gateway','root-supervisor');await assert.rejects(rpc(denied,'executeCapability',task),e=>e.code===grpc.status.PERMISSION_DENIED);
 const temporal=process.env.TEMPORAL_BIN||'temporal';launch(temporal,['server','start-dev','--ip','127.0.0.1','--port',String(temporalPort),'--ui-port',String(await port()),'--db-filename',path.join(scratch,'temporal.db')],'temporal');
 await until(async()=>{await exec(temporal,['operator','cluster','health','--address',`127.0.0.1:${temporalPort}`],{timeout:3000});return true;});
 const shared={TEMPORAL_ADDRESS:`127.0.0.1:${temporalPort}`,TEMPORAL_NAMESPACE:'default',TEMPORAL_TASK_QUEUE:'calendar-test-'+tenant,WORKFLOW_DATABASE_URL:process.env.TEST_WORKFLOW_DATABASE_URL};
 launch(path.resolve(process.env.WORKER_BIN||'work/execution-worker'),[],'worker',{...shared,...identity('execution-worker'),SUPERVISOR_GRPC_ADDRESS:supervisorAddress,CALENDAR_GRPC_ADDRESS:address});
 launch(path.resolve(process.env.DISPATCHER_BIN||'work/outbox-dispatcher'),[],'dispatcher',{...shared,DISPATCHER_DATABASE_URL:process.env.TEST_DISPATCHER_DATABASE_URL,ENABLE_CALENDAR_REMINDERS:'true'});
 const execution=await request('POST','executions',{capabilityId:'intake.summarize.v1',input:{answers:{summary:'synthetic test'}},idempotencyKey:randomUUID()},202);
 await until(async()=>{const state=await request('GET',`executions/${execution.executionId}`);assert(!['failed','cancelled'].includes(state.status),JSON.stringify(state));return state.status==='awaiting_review';});
 await until(async()=>{const reminders=await request('GET',`calendar-entries/${entryId}/reminders?calendarId=${calendarId}`);return reminders.reminders.some(x=>x.status==='due');},90000);
 console.log('PASS: API/Go calendar, JWT/role denials, ACL redaction, idempotency/conflicts, Go -> TS -> Python mTLS, Temporal reminder');
})().catch(e=>{console.error(e);console.error('Synthetic diagnostics:',scratch);process.exitCode=1;}).finally(async()=>{
 for(const channel of channels)channel.close();
 for(const child of children.reverse()){if(child.exitCode!==null||child.signalCode!==null)continue;child.kill('SIGTERM');await Promise.race([new Promise(r=>child.once('exit',r)),sleep(4000)]);if(child.exitCode===null&&child.signalCode===null)child.kill('SIGKILL');}
 if(jwks){jwks.closeAllConnections();jwks.close();}
 for(const table of ['calendar_commands','calendar_reminders','calendar_proposals','calendar_entries','calendar_series','calendar_grants','calendars','outbox_events','audit_events','ai_executions','principal_capabilities','matter_grants','matters','memberships'])await pool.query(`DELETE FROM ${table} WHERE tenant_id=$1`,[tenant]);
 await pool.query('DELETE FROM tenants WHERE id=$1',[tenant]);if(integrationLock){await integrationLock.query("SELECT pg_advisory_unlock(604182701)");integrationLock.release();}await pool.end();
});

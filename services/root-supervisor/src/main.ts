import { createServer } from 'node:http';
import { readFileSync } from 'node:fs';
import { capabilityRequestSchema, agentResultSchema, CapabilityRegistry, findCapability, signInternal, verifyInternal } from '@lawpilot/contracts';

const registry:CapabilityRegistry=JSON.parse(readFileSync(process.env.CAPABILITY_REGISTRY_PATH||'contracts/capabilities/registry.v1.json','utf8'));
const secret=process.env.INTERNAL_HMAC_KEY||'';
if(secret.length<32) throw new Error('INTERNAL_HMAC_KEY is required and must be >=32 chars');
const port=Number(process.env.SUPERVISOR_PORT||3001);

function reply(res:any,status:number,obj:unknown){res.writeHead(status,{'content-type':'application/json','cache-control':'no-store'});res.end(JSON.stringify(obj));}
createServer(async(req,res)=>{
  if(req.method==='GET'&&req.url==='/healthz') return reply(res,200,{service:'root-supervisor',status:'up'});
  if(req.method!=='POST'||req.url!=='/internal/execute') return reply(res,404,{error:'not_found'});
  let raw='';
  for await(const chunk of req){raw+=chunk; if(Buffer.byteLength(raw)>32_768) return reply(res,413,{error:'input_too_large'});}
  if(!verifyInternal(raw,req.headers['x-lawpilot-timestamp'] as string|undefined,req.headers['x-lawpilot-signature'] as string|undefined,secret)) return reply(res,401,{error:'invalid_internal_signature'});
  let input:unknown;
  try{input=JSON.parse(raw)}catch{return reply(res,400,{error:'invalid_json'})}
  const task=capabilityRequestSchema.safeParse(input);
  if(!task.success)return reply(res,400,{error:'invalid_task'});
  const capability=findCapability(registry,task.data.capabilityId);
  if(!capability) return reply(res,422,{error:'UNSUPPORTED_CAPABILITY'});
  if(capability.requiresMatter&&!task.data.matterId) return reply(res,400,{error:'matter_required'});
  if(Buffer.byteLength(JSON.stringify(task.data.input))>capability.maxInputBytes) return reply(res,413,{error:'input_too_large'});
  // Supervisor is the *single logical coordinator* of specialized AI agents.
  // No direct Clio requests, DB reads, or writes are permitted here.
  const outbound=JSON.stringify(task.data);const ts=Date.now();
  try{
    const response=await fetch(`${process.env.AI_URL||'http://localhost:8000'}/internal/execute`,{
      method:'POST',body:outbound,headers:{'content-type':'application/json','x-lawpilot-timestamp':String(ts),'x-lawpilot-signature':signInternal(outbound,ts,secret)},signal:AbortSignal.timeout(20_000)});
    if(!response.ok)return reply(res,502,{error:'AI_worker_failed',status:response.status});
    const result=agentResultSchema.safeParse(await response.json());
    if(!result.success||result.data.executionId!==task.data.executionId||result.data.agentId!==capability.agentId||result.data.agentVersion!==capability.agentVersion){
      return reply(res,502,{error:'invalid_agent_result'});
    }
    // Reject external actions until deterministic policy/approval workflow is wired.
    if(result.data.proposedActions.length>0)return reply(res,422,{error:'proposed_actions_not_enabled_in_starter'});
    return reply(res,200,result.data);
  }catch{return reply(res,503,{error:'ai_worker_unavailable'})}
}).listen(port,'0.0.0.0',()=>console.log(`root-supervisor listening on ${port}`));

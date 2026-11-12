import { randomUUID } from 'node:crypto';
import { PoolClient } from 'pg';

export async function recordExecutionEvent(client: PoolClient, tenantId: string, principalId: string,
  executionId: string, state: 'accepted' | 'completed' | 'failed' | 'cancelled', decisionCode: string): Promise<void> {
  const event={specversion:'1.0',id:randomUUID(),source:'platform-api',type:`lawpilot.ai.${state}.v1`,
    time:new Date().toISOString(),tenantId,schemaVersion:'1.0',aggregateId:executionId,
    data:{executionId,state,decisionCode}};
  // Metadata only. Never copy request input, documents, tokens or result text here.
  await client.query('INSERT INTO audit_events(tenant_id,actor_id,action,resource_id,decision_code) VALUES($1,$2,$3,$4,$5)',
    [tenantId,principalId,`ai.${state}`,executionId,decisionCode]);
  await client.query('INSERT INTO outbox_events(tenant_id,event_type,event_version,aggregate_id,event_body) VALUES($1,$2,$3,$4,$5)',
    [tenantId,event.type,'1.0',executionId,JSON.stringify(event)]);
}

import { createHmac, timingSafeEqual } from 'node:crypto';
import { z } from 'zod';

export const capabilityRequestSchema = z.object({
  executionId: z.uuid(), tenantId: z.uuid(), principalId: z.string().min(1),
  matterId: z.uuid().optional(), capabilityId: z.string().regex(/^[a-z][a-z0-9.]*\.v[1-9]\d*$/),
  input: z.record(z.string(), z.unknown()), traceId: z.string().min(1),
}).strict();
export type CapabilityRequest = z.infer<typeof capabilityRequestSchema>;

export const evidenceSchema = z.object({
  documentId: z.string().min(1), documentVersionId: z.string().min(1),
  passageId: z.string().min(1), contentSha256: z.string().regex(/^[a-f0-9]{64}$/).optional(), pageNumber: z.number().int().positive().optional(),
}).strict();
export const agentResultSchema = z.object({
  executionId: z.string().min(1), agentId: z.string().min(1), agentVersion: z.string().min(1),
  status: z.enum(['completed','abstained','failed']),
  output: z.record(z.string(), z.unknown()), evidence: z.array(evidenceSchema),
  proposedActions: z.array(z.record(z.string(), z.unknown())), requiresAttorneyReview: z.boolean(),
}).strict();
export type AgentResult = z.infer<typeof agentResultSchema>;

export const clientRequestSchema = z.object({
  capabilityId: capabilityRequestSchema.shape.capabilityId,
  matterId: z.uuid().optional(),
  input: capabilityRequestSchema.shape.input,
  idempotencyKey: z.string().min(8).max(128),
}).strict();

export interface CapabilityDefinition {
  id: string; agentId: string; agentVersion: string;
  requiresMatter: boolean; requiresAttorneyReview: boolean; maxInputBytes: number;
}
export interface CapabilityRegistry { registryVersion: string; capabilities: CapabilityDefinition[] }
export function findCapability(registry: CapabilityRegistry, id: string): CapabilityDefinition | undefined {
  return registry.capabilities.find((x) => x.id === id);
}

// LOCAL DEV transport only. Production: service identity + mTLS/OIDC, replay cache.
export function signInternal(body: string, timestamp: number, secret: string): string {
  if (secret.length < 32) throw new Error('INTERNAL_HMAC_KEY must be >= 32 characters');
  return createHmac('sha256', secret).update(`${timestamp}.`).update(body).digest('hex');
}
export function verifyInternal(body: string, timestampHeader: string|undefined, signature: string|undefined, secret: string, now=Date.now()): boolean {
  if (!timestampHeader || !signature || !/^\d{13}$/.test(timestampHeader) || !/^[a-f0-9]{64}$/i.test(signature)) return false;
  const ts = Number(timestampHeader);
  if (Math.abs(now - ts) > 60_000) return false;
  const expected = Buffer.from(signInternal(body, ts, secret), 'hex');
  const received = Buffer.from(signature, 'hex');
  return expected.length === received.length && timingSafeEqual(expected, received);
}

export const capabilityDefinitionSchema = z.object({
  id: capabilityRequestSchema.shape.capabilityId,
  agentId: z.string().min(1), agentVersion: z.string().regex(/^\d+\.\d+\.\d+$/),
  requiresMatter: z.boolean(), requiresAttorneyReview: z.boolean(),
  maxInputBytes: z.number().int().positive().max(32768),
}).strict();
export const capabilityRegistrySchema = z.object({
  registryVersion: z.literal('1.0'),
  capabilities: z.array(capabilityDefinitionSchema).min(1),
}).strict().superRefine((registry, ctx) => {
  const ids = new Set<string>();
  for (const capability of registry.capabilities) {
    if (ids.has(capability.id)) ctx.addIssue({code: 'custom', message: 'Duplicate capability ID'});
    ids.add(capability.id);
  }
});

export function validateAgentResult(raw: unknown, task: CapabilityRequest, capability: CapabilityDefinition): AgentResult {
  const result = agentResultSchema.parse(raw);
  if (result.executionId !== task.executionId || result.agentId !== capability.agentId || result.agentVersion !== capability.agentVersion) {
    throw new Error('Agent result identity mismatch');
  }
  if (capability.requiresAttorneyReview && !result.requiresAttorneyReview) throw new Error('Attorney review cannot be bypassed');
  if (capability.requiresMatter && result.status === 'completed' && !result.evidence.length) throw new Error('Completed matter results require evidence');
  if (result.proposedActions.length) throw new Error('External actions are disabled in the starter');
  return result;
}

import { Injectable, HttpException } from '@nestjs/common';
import { PoolClient } from 'pg';
import { DbService } from './db.service';

function uuid(value: string): boolean {
  return /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value);
}

@Injectable()
export class AccessService {
  constructor(private db: DbService) {}

  async authorize(tenantId: string, principalId: string, capabilityId?: string, matterId?: string): Promise<void> {
    await this.withAccess(tenantId, principalId, capabilityId, matterId, async () => undefined);
  }

  async withAccess<T>(tenantId: string, principalId: string, capabilityId: string | undefined,
    matterId: string | undefined, action: (client: PoolClient) => Promise<T>): Promise<T> {
    if (!uuid(tenantId) || (matterId && !uuid(matterId))) throw new HttpException({error: 'Invalid resource identifier'}, 400);
    return this.db.withTenant(tenantId.toLowerCase(), async client => {
      // Evaluate permissions and perform the operation in the same tenant transaction.
      const member = await client.query("SELECT 1 FROM memberships WHERE tenant_id=$1 AND principal_id=$2 AND status='active'", [tenantId, principalId]);
      if (member.rowCount !== 1) throw new HttpException({error: 'Not a member of this tenant'}, 403);
      if (capabilityId) {
        const cap = await client.query('SELECT 1 FROM principal_capabilities WHERE tenant_id=$1 AND principal_id=$2 AND capability_id=$3', [tenantId, principalId, capabilityId]);
        if (cap.rowCount !== 1) throw new HttpException({error: 'Capability not granted'}, 403);
      }
      if (matterId) {
        const grant = await client.query("SELECT 1 FROM matter_grants WHERE tenant_id=$1 AND principal_id=$2 AND matter_id=$3 AND permission='matter.read'", [tenantId, principalId, matterId]);
        if (grant.rowCount !== 1) throw new HttpException({error: 'Matter access denied'}, 403);
      }
      return action(client);
    });
  }
}

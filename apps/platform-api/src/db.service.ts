import { Injectable, OnModuleDestroy } from '@nestjs/common';
import { Pool, PoolClient } from 'pg';

@Injectable()
export class DbService implements OnModuleDestroy {
  private pool = new Pool({ connectionString: process.env.DATABASE_URL, max: 10, application_name: 'lawpilot-platform-api' });
  async onModuleDestroy(): Promise<void> { await this.pool.end(); }
  async withTenant<T>(tenantId: string, fn: (c:PoolClient)=>Promise<T>): Promise<T> {
    const client=await this.pool.connect();
    try {
      await client.query('BEGIN');
      // Transaction-local: prevents leaking tenant context between pooled connections.
      await client.query("SELECT set_config('app.tenant_id', $1, true)",[tenantId]);
      const result=await fn(client);
      await client.query('COMMIT'); return result;
    } catch(e){ await client.query('ROLLBACK'); throw e; }
    finally { client.release(); }
  }
}

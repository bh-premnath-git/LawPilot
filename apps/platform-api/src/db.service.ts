import { Injectable, OnModuleDestroy } from "@nestjs/common";
import { Pool, PoolClient } from "pg";
import { readFileSync } from "node:fs";
function connectionString() {
  const value = process.env.DATABASE_URL_FILE
    ? readFileSync(process.env.DATABASE_URL_FILE, "utf8").trim()
    : process.env.DATABASE_URL;
  if (
    (process.env.NODE_ENV === "production" ||
      process.env.APP_ENV === "production") &&
    (!value || new URL(value).searchParams.get("sslmode") !== "verify-full")
  )
    throw new Error("Production database TLS verify-full required");
  return value;
}

@Injectable()
export class DbService implements OnModuleDestroy {
  private pool = new Pool({
    connectionString: connectionString(),
    max: 10,
    connectionTimeoutMillis: 5000,
    idleTimeoutMillis: 30000,
    application_name: "lawpilot-platform-api",
  });
  async ready(): Promise<void> {
    const role = await this.pool.query(
      "SELECT rolsuper,rolbypassrls,EXISTS(SELECT 1 FROM pg_class c WHERE c.relname IN('memberships','document_assets','ai_executions') AND pg_has_role(current_user,c.relowner,'MEMBER')) AS owns_tables FROM pg_roles WHERE rolname=current_user",
    );
    if (role.rows[0]?.rolsuper || role.rows[0]?.rolbypassrls || role.rows[0]?.owns_tables)
      throw new Error(
        "Privileged database roles are not application identities",
      );
    await this.pool.query("SELECT revision FROM matters LIMIT 0");
    await this.pool.query("SELECT workflow_id FROM ai_executions LIMIT 0");
    await this.pool.query("SELECT lease_token FROM outbox_events LIMIT 0");
  }
  async onModuleDestroy(): Promise<void> {
    await this.pool.end();
  }
  async withTenant<T>(
    tenantId: string,
    fn: (c: PoolClient) => Promise<T>,
  ): Promise<T> {
    if (
      !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(
        tenantId,
      )
    )
      throw new Error("Invalid tenant context");
    const client = await this.pool.connect();
    try {
      await client.query("BEGIN");
      // Transaction-local: prevents leaking tenant context between pooled connections.
      await client.query("SELECT set_config('app.tenant_id', $1, true)", [
        tenantId.toLowerCase(),
      ]);
      await client.query("SET LOCAL statement_timeout = '10s'");
      const result = await fn(client);
      await client.query("COMMIT");
      return result;
    } catch (e) {
      try {
        await client.query("ROLLBACK");
      } catch {
        /* Preserve the operation error. */
      }
      throw e;
    } finally {
      client.release();
    }
  }
}

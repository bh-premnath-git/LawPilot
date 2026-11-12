import 'reflect-metadata';
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { AccessService } from './access.service';
import { DbService } from './db.service';
const tenant = '11111111-1111-4111-8111-111111111111';
const matter = '22222222-2222-4222-8222-222222222222';

test('authorization requires membership, capability and explicit matter grant', async () => {
  for (const rows of [[0], [1, 0], [1, 1, 0], [1, 1, 1]]) {
    const seen: unknown[][] = [];
    const db = { withTenant: async (id: string, fn: Function) => {
      assert.equal(id, tenant);
      return fn({ query: async (sql: string, params: unknown[]) => {
        seen.push(params);
        if (seen.length === 1) assert.match(sql, /status='active'/);
        return { rowCount: rows[seen.length - 1] };
      }});
    }} as unknown as DbService;
    const run = new AccessService(db).authorize(tenant, 'user-1', 'document.summarize.v1', matter);
    if (rows.every(Boolean)) await run;
    else await assert.rejects(run, { status: 403 });
    assert.equal(seen.length, rows.length);
    assert.deepEqual(seen[0], [tenant, 'user-1']);
    if (seen.length === 3) assert.deepEqual(seen[2], [tenant, 'user-1', matter]);
  }
});
test('malformed tenant and matter IDs fail before database access', async () => {
  const db = { withTenant: () => assert.fail('unexpected database call') } as unknown as DbService;
  const access = new AccessService(db);
  await assert.rejects(access.authorize('invalid', 'user-1'), { status: 400 });
  await assert.rejects(access.authorize(tenant, 'user-1', undefined, 'invalid'), { status: 400 });
});

test('authorized action runs in the checked transaction and is skipped on denial', async () => {
  let actionCalled = false;
  const client = { query: async () => ({ rowCount: 1 }) };
  const db = { withTenant: async (_id: string, fn: Function) => fn(client) } as unknown as DbService;
  const value = await new AccessService(db).withAccess(tenant, 'user-1', undefined, undefined, async c => {
    assert.equal(c, client); actionCalled = true; return 'authorized';
  });
  assert.equal(value, 'authorized'); assert.equal(actionCalled, true);
  client.query = async () => ({ rowCount: 0 });
  await assert.rejects(new AccessService(db).withAccess(tenant, 'user-1', undefined, undefined,
    async () => assert.fail('unauthorized action executed')), { status: 403 });
});

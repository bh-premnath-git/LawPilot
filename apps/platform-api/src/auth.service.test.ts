import 'reflect-metadata';
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { generateKeyPair, exportJWK, SignJWT } from 'jose';
import { AuthService } from './auth.service';

test('Keycloak access-token verification rejects invalid identity credentials', async () => {
  const { publicKey, privateKey } = await generateKeyPair('RS256');
  const jwk = { ...await exportJWK(publicKey), kid: 'test', alg: 'RS256', use: 'sig' };
  const server = createServer((_req, res) => { res.setHeader('content-type', 'application/json'); res.end(JSON.stringify({ keys: [jwk] })); });
  await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve));
  const address = server.address() as { port: number };
  process.env.KEYCLOAK_ISSUER = 'http://localhost:8080/realms/lawpilot-dev';
  process.env.KEYCLOAK_AUDIENCE = 'lawpilot-api';
  process.env.KEYCLOAK_JWKS_URL = `http://127.0.0.1:${address.port}/certs`;
  const auth = new AuthService();
  const now = Math.floor(Date.now() / 1000);
  const claims = { sub: 'user-1', iss: process.env.KEYCLOAK_ISSUER, aud: 'lawpilot-api', typ: 'Bearer', iat: now, exp: now + 300 };
  const token = (changes: Record<string, unknown> = {}) => new SignJWT({ ...claims, ...changes }).setProtectedHeader({ alg: 'RS256', kid: 'test' }).sign(privateKey);
  try {
    assert.equal(await auth.subject(`bearer ${await token()}`), 'user-1');
    for (const changes of [{ iss: 'https://attacker.example' }, { aud: 'other-api' }, { exp: now - 10 }, { exp: undefined }, { iat: undefined }, { sub: '' }, { typ: 'ID' }, { typ: 'Refresh' }, { nbf: now + 300 }]) {
      await assert.rejects(auth.subject(`Bearer ${await token(changes)}`), { status: 401 });
    }
    for (const header of [undefined, 'Basic abc', 'Bearer ', 'Bearer a b', 'Bearer invalid']) {
      await assert.rejects(auth.subject(header), { status: 401 });
    }
    const forgedKey = await generateKeyPair('RS256');
    const forged = await new SignJWT(claims).setProtectedHeader({ alg: 'RS256', kid: 'test' }).sign(forgedKey.privateKey);
    await assert.rejects(auth.subject(`Bearer ${forged}`), { status: 401 });
    delete process.env.KEYCLOAK_AUDIENCE;
    assert.throws(() => new AuthService(), /required/);
  } finally { server.closeAllConnections(); await new Promise<void>(resolve => server.close(() => resolve())); }
});

import { Injectable, UnauthorizedException } from '@nestjs/common';
import { createRemoteJWKSet, jwtVerify } from 'jose';

@Injectable()
export class AuthService {
  private jwks = createRemoteJWKSet(new URL(process.env.KEYCLOAK_JWKS_URL || 'http://localhost:8080/realms/lawpilot-dev/protocol/openid-connect/certs'));
  async subject(authHeader: string | undefined): Promise<string> {
    if (!authHeader?.startsWith('Bearer ')) throw new UnauthorizedException('Missing bearer token');
    try {
      const { payload } = await jwtVerify(authHeader.slice(7), this.jwks, {
        issuer: process.env.KEYCLOAK_ISSUER,
        audience: process.env.KEYCLOAK_AUDIENCE,
        algorithms: ['RS256'],
      });
      if (!payload.sub) throw new Error('Missing JWT subject');
      return payload.sub;
    } catch { throw new UnauthorizedException('Invalid access token'); }
  }
}

import { Injectable, UnauthorizedException } from '@nestjs/common';
import { createRemoteJWKSet, jwtVerify } from 'jose';

@Injectable()
export class AuthService {
  private readonly issuer: string;
  private readonly audience: string;
  private readonly jwks: ReturnType<typeof createRemoteJWKSet>;

  constructor() {
    const issuer = process.env.KEYCLOAK_ISSUER;
    const audience = process.env.KEYCLOAK_AUDIENCE;
    if (!issuer || !audience?.trim()) throw new Error('KEYCLOAK_ISSUER and KEYCLOAK_AUDIENCE are required');
    const issuerUrl = new URL(issuer);
    const jwksUrl = new URL(process.env.KEYCLOAK_JWKS_URL || `${issuer}/protocol/openid-connect/certs`);
    for (const url of [issuerUrl, jwksUrl]) {
      if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password || url.search || url.hash) {
        throw new Error('Invalid Keycloak URL');
      }
      if (process.env.NODE_ENV === 'production' && url.protocol !== 'https:') {
        throw new Error('Production Keycloak URLs must use HTTPS');
      }
    }
    this.issuer = issuer;
    this.audience = audience;
    this.jwks = createRemoteJWKSet(jwksUrl);
  }

  async subject(authHeader: string | undefined): Promise<string> {
    const match = /^Bearer ([^\s]+)$/i.exec(authHeader || '');
    if (!match) throw new UnauthorizedException('Missing or malformed bearer token');
    try {
      const { payload } = await jwtVerify(match[1], this.jwks, {
        issuer: this.issuer,
        audience: this.audience,
        algorithms: ['RS256'],
        requiredClaims: ['sub', 'exp', 'iat'],
      });
      // Keycloak access tokens carry typ=Bearer; ID and refresh tokens are not API credentials.
      if (payload.typ !== 'Bearer' || !payload.sub?.trim()) throw new Error('Invalid access token claims');
      return payload.sub;
    } catch { throw new UnauthorizedException('Invalid access token'); }
  }
}

import { readFileSync } from "node:fs";
import {
  experimental,
  type ChannelCredentials,
  type ServerCredentials,
  status,
  type ServerUnaryCall,
  type ServiceError,
} from "@grpc/grpc-js";
function path(name: string) {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required for mTLS`);
  readFileSync(value);
  return value;
}
function provider(client: boolean) {
  return new experimental.FileWatcherCertificateProvider({
    caCertificateFile: path("GRPC_CA_FILE"),
    certificateFile: path(
      client ? "GRPC_CLIENT_CERT_FILE" : "GRPC_SERVER_CERT_FILE",
    ),
    privateKeyFile: path(
      client ? "GRPC_CLIENT_KEY_FILE" : "GRPC_SERVER_KEY_FILE",
    ),
    refreshIntervalMs: 5000,
  });
}
export function clientCredentials(): ChannelCredentials {
  const source = provider(true);
  return experimental.createCertificateProviderChannelCredentials(
    source,
    source,
  );
}
export function serverCredentials(): ServerCredentials {
  const source = provider(false);
  return experimental.createCertificateProviderServerCredentials(
    source,
    source,
    true,
  );
}
export function requireWorkload(
  call: ServerUnaryCall<any, any>,
  expected: string,
): void {
  const auth = call.getAuthContext(),
    cert = auth?.sslPeerCertificate as any;
  const domain = process.env.WORKLOAD_TRUST_DOMAIN || "lawpilot.local";
  const uri = `URI:spiffe://${domain}/${expected}`;
  const alternatives = String(cert?.subjectaltname || "").split(/,\s*/);
  const legacy =
    process.env.APP_ENV === "development" &&
    process.env.WORKLOAD_ALLOW_LEGACY_CN === "true";
  if (
    auth?.transportSecurityType !== "ssl" ||
    cert?.subject?.CN !== expected ||
    (!alternatives.includes(uri) && !legacy)
  )
    throw Object.assign(new Error("WORKLOAD_SCOPE_DENIED"), {
      code: status.PERMISSION_DENIED,
    }) as ServiceError;
}

import { readFileSync } from "node:fs";
import {
  credentials,
  ServerCredentials,
  status,
  type ServerUnaryCall,
  type ServiceError,
} from "@grpc/grpc-js";
function file(name: string): Buffer {
  const path = process.env[name];
  if (!path) throw new Error(`${name} is required for mTLS`);
  return readFileSync(path);
}
export function clientCredentials() {
  return credentials.createSsl(
    file("GRPC_CA_FILE"),
    file("GRPC_CLIENT_KEY_FILE"),
    file("GRPC_CLIENT_CERT_FILE"),
  );
}
export function serverCredentials() {
  return ServerCredentials.createSsl(
    file("GRPC_CA_FILE"),
    [
      {
        private_key: file("GRPC_SERVER_KEY_FILE"),
        cert_chain: file("GRPC_SERVER_CERT_FILE"),
      },
    ],
    true,
  );
}
export function requireWorkload(
  call: ServerUnaryCall<any, any>,
  expected: string,
): void {
  const auth = call.getAuthContext();
  if (
    auth?.transportSecurityType !== "ssl" ||
    auth.sslPeerCertificate?.subject?.CN !== expected
  ) {
    throw Object.assign(new Error("WORKLOAD_SCOPE_DENIED"), {
      code: status.PERMISSION_DENIED,
    }) as ServiceError;
  }
}

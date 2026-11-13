// Synthetic process test: actual API, Go integration/context/parser, Rust, Temporal,
// TypeScript supervisor and Python generation. External S3/ClamAV/model are local fixtures.
const assert = require("node:assert/strict"),
  fs = require("node:fs"),
  path = require("node:path"),
  http = require("node:http"),
  net = require("node:net");
const { spawn, execFile } = require("node:child_process"),
  { promisify } = require("node:util"),
  { randomUUID, createHash } = require("node:crypto");
const { Pool } = require("pg"),
  grpc = require("@grpc/grpc-js"),
  { Integrations } = require("../packages/rpc/dist");
const exec = promisify(execFile),
  sleep = (ms) => new Promise((r) => setTimeout(r, ms)),
  children = [],
  servers = [],
  channels = [];
for (const key of [
  "TEST_DATABASE_URL",
  "TEST_APP_DATABASE_URL",
  "TEST_CONTEXT_DATABASE_URL",
  "TEST_INTEGRATION_DATABASE_URL",
  "TEST_WORKFLOW_DATABASE_URL",
  "TEST_DISPATCHER_DATABASE_URL",
])
  assert(process.env[key], key + " required");
const pool = new Pool({ connectionString: process.env.TEST_DATABASE_URL }),
  tenant = randomUUID(),
  owner = randomUUID(),
  reviewer = randomUUID(),
  matter = randomUUID();
const scratch = fs.mkdtempSync(path.resolve("work/configured-grpc-")),
  tls = path.join(scratch, "tls");
let lock;
async function port() {
  const s = net.createServer();
  await new Promise((r) => s.listen(0, "127.0.0.1", r));
  const p = s.address().port;
  await new Promise((r) => s.close(r));
  return p;
}
async function listen(s) {
  servers.push(s);
  await new Promise((r) => s.listen(0, "127.0.0.1", r));
  return s.address().port;
}
async function until(action, timeout = 60000) {
  let last;
  for (const end = Date.now() + timeout; Date.now() < end;) {
    try {
      const x = await action();
      if (x) return x;
    } catch (e) {
      last = e;
    }
    await sleep(150);
  }
  throw last || Error("process integration timeout");
}
function launch(command, args, name, env = {}) {
  const fd = fs.openSync(path.join(scratch, name + ".log"), "a"),
    p = spawn(command, args, {
      env: {
        ...process.env,
        NODE_ENV: "test",
        APP_ENV: "development",
        OTEL_EXPORTER_OTLP_ENDPOINT: "",
        ENABLE_LEGACY_INTERNAL_HTTP: "false",
        LEGAL_AI_ENABLED: "false",
        FILE_STORAGE_ENABLED: "false",
        CLIO_ENABLED: "false",
        GENERIC_PROVIDER_ENABLED: "false",
        ...env,
      },
      stdio: ["ignore", fd, fd],
    });
  fs.closeSync(fd);
  children.push(p);
  return p;
}
function identity(role) {
  return {
    GRPC_CA_FILE: path.join(tls, "ca.pem"),
    GRPC_CLIENT_CERT_FILE: path.join(tls, role + ".pem"),
    GRPC_CLIENT_KEY_FILE: path.join(tls, role + ".key"),
    GRPC_SERVER_CERT_FILE: path.join(tls, role + ".pem"),
    GRPC_SERVER_KEY_FILE: path.join(tls, role + ".key"),
  };
}
(async () => {
  lock = await pool.connect();
  await lock.query("SELECT pg_advisory_lock(604182701)");
  await exec("bash", ["scripts/generate_dev_tls.sh", tls]);
  const { generateKeyPair, exportJWK, SignJWT } = await import("jose"),
    { publicKey, privateKey } = await generateKeyPair("RS256");
  const key = {
    ...(await exportJWK(publicKey)),
    kid: "configured-fixture",
    alg: "RS256",
    use: "sig",
  };
  const jwks = http.createServer((req, res) => {
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({ keys: [key] }));
  });
  const issuer = `http://127.0.0.1:${await listen(jwks)}/realm`;
  const auth = {
    KEYCLOAK_ISSUER: issuer,
    KEYCLOAK_JWKS_URL: issuer + "/certs",
    KEYCLOAK_AUDIENCE: "lawpilot-api",
  };
  const jwt = (sub) =>
    new SignJWT({ typ: "Bearer" })
      .setProtectedHeader({ alg: "RS256", kid: key.kid })
      .setIssuer(issuer)
      .setAudience("lawpilot-api")
      .setSubject(sub)
      .setIssuedAt()
      .setExpirationTime("10m")
      .sign(privateKey);
  const token = await jwt(owner),
    reviewToken = await jwt(reviewer);
  await pool.query(
    "INSERT INTO tenants VALUES($1,'Synthetic configured test')",
    [tenant],
  );
  await pool.query(
    "INSERT INTO matters(id,tenant_id,title,resource_owner)VALUES($1,$2,'Synthetic lease','native')",
    [matter, tenant],
  );
  for (const who of [owner, reviewer]) {
    await pool.query("INSERT INTO memberships VALUES($1,$2,'active')", [
      tenant,
      who,
    ]);
    await pool.query(
      "INSERT INTO matter_grants VALUES($1,$2,$3,'matter.read')",
      [tenant, matter, who],
    );
    for (const cap of [
      "document.ingest",
      "document.download",
      "document.delete",
      "context.read",
      "matter.qa.v1",
      "draft.approve",
      "integration.read",
      "integration.manage",
    ])
      await pool.query("INSERT INTO principal_capabilities VALUES($1,$2,$3)", [
        tenant,
        who,
        cap,
      ]);
  }
  // Private object fixture verifies the actual AWS SDK's size/encryption/checksum contract.
  const objects = new Map(),
    kms = "arn:aws:kms:fixture:000000000000:key/synthetic";
  const s3 = http.createServer(async (req, res) => {
    const u = new URL(req.url, "http://fixture"),
      key = decodeURIComponent(u.pathname).replace(/^\/bucket\//, "");
    if (req.method === "PUT") {
      const chunks = [];
      for await (const part of req) chunks.push(part);
      const bytes = Buffer.concat(chunks);
      if (req.headers["if-none-match"] === "*" && objects.has(key)) {
        res.writeHead(412, { "Content-Type": "application/xml" });
        res.end("<Error><Code>PreconditionFailed</Code></Error>");
        return;
      }
      objects.set(key, bytes);
      res.setHeader("ETag", '"fixture"');
      res.end();
      return;
    }
    if (req.method === "DELETE") {
      objects.delete(key);
      res.writeHead(204);
      res.end();
      return;
    }
    const bytes = objects.get(key);
    if (!bytes) {
      res.writeHead(404);
      res.end();
      return;
    }
    res.setHeader("Content-Length", bytes.length);
    res.setHeader("x-amz-server-side-encryption", "aws:kms");
    res.setHeader("x-amz-server-side-encryption-aws-kms-key-id", kms);
    res.end(bytes);
  });
  const s3port = await listen(s3);
  let infected = false;
  const scanner = net.createServer((c) => {
    let buffer = Buffer.alloc(0),
      prefix = false;
    const chunks = [];
    c.on("data", (part) => {
      buffer = Buffer.concat([buffer, part]);
      if (!prefix) {
        if (buffer.length < 10) return;
        assert.equal(buffer.subarray(0, 10).toString(), "zINSTREAM\0");
        buffer = buffer.subarray(10);
        prefix = true;
      }
      while (buffer.length >= 4) {
        const length = buffer.readUInt32BE();
        if (buffer.length < 4 + length) return;
        buffer = buffer.subarray(4);
        if (!length) {
          c.end(infected ? "stream: Eicar FOUND\0" : "stream: OK\0");
          return;
        }
        chunks.push(buffer.subarray(0, length));
        buffer = buffer.subarray(length);
      }
    });
  });
  const scanport = await listen(scanner);
  let modelCalls = 0;
  const model = http.createServer(async (req, res) => {
    const chunks = [];
    for await (const chunk of req) chunks.push(chunk);
    assert.equal(req.headers.authorization, "Bearer synthetic-only");
    const input = JSON.parse(Buffer.concat(chunks)),
      payload = JSON.parse(input.messages[1].content);
    assert(!input.tools);
    assert(payload.untrustedSources[0].text.includes("Rent is due monthly."));
    modelCalls++;
    res.setHeader("Content-Type", "application/json");
    res.end(
      JSON.stringify({
        model: "fixture-model",
        choices: [
          {
            finish_reason: "stop",
            message: {
              content: JSON.stringify({
                draft: "Unapproved analysis of rent terms.",
                claims: [{ sourceId: "S1", quote: "Rent is due monthly." }],
                limitations: ["Synthetic response; attorney review required."],
              }),
            },
          },
        ],
      }),
    );
  });
  const modelport = await listen(model);
  const [
    contextPort,
    integrationPort,
    parserPort,
    apiPort,
    aiPort,
    supervisorPort,
    temporalPort,
  ] = await Promise.all(Array.from({ length: 7 }, port));
  const contextAddress = `127.0.0.1:${contextPort}`,
    integrationAddress = `127.0.0.1:${integrationPort}`,
    parserAddress = `127.0.0.1:${parserPort}`;
  launch(path.resolve("work/context-engine"), [], "context", {
    ...identity("context-engine"),
    ...auth,
    CONTEXT_DATABASE_URL: process.env.TEST_CONTEXT_DATABASE_URL,
    CONTEXT_GRPC_ADDRESS: contextAddress,
    EMBEDDING_BASE_URL: "",
    EMBEDDING_MODEL: "",
  });
  launch(path.resolve("work/document-parser"), [], "parser", {
    ...identity("document-parser"),
    PARSER_GRPC_ADDRESS: parserAddress,
    DOCUMENT_PROCESSOR_BIN: path.resolve(
      process.env.DOCUMENT_PROCESSOR_BIN ||
        "work/rust-target/debug/lawpilot-document-worker",
    ),
  });
  launch(path.resolve("work/integration-service"), [], "integration", {
    ...identity("integration-service"),
    ...auth,
    INTEGRATION_DATABASE_URL: process.env.TEST_INTEGRATION_DATABASE_URL,
    INTEGRATION_GRPC_BIND: integrationAddress,
    CONTEXT_GRPC_ADDRESS: contextAddress,
    PARSER_GRPC_ADDRESS: parserAddress,
    FILE_STORAGE_ENABLED: "true",
    S3_ENDPOINT: `http://127.0.0.1:${s3port}`,
    S3_FORCE_PATH_STYLE: "true",
    S3_BUCKET: "bucket",
    S3_REGION: "fixture",
    S3_KMS_KEY_ID: kms,
    S3_ACCESS_KEY_ID: "synthetic",
    S3_SECRET_ACCESS_KEY: "synthetic-only",
    S3_SKIP_BUCKET_POLICY_CHECK: "true",
    CLAMAV_ADDRESS: `127.0.0.1:${scanport}`,
  });
  launch(process.execPath, ["apps/platform-api/dist/main.js"], "api", {
    ...identity("api-gateway"),
    ...auth,
    DATABASE_URL: process.env.TEST_APP_DATABASE_URL,
    API_PORT: String(apiPort),
    CONTEXT_GRPC_ADDRESS: contextAddress,
    INTEGRATION_GRPC_ADDRESS: integrationAddress,
  });
  await until(async () => {
    const r = await fetch(`http://127.0.0.1:${apiPort}/v1/readyz`);
    return r.ok;
  });
  async function request(
    method,
    route,
    body,
    expected = 200,
    credential = token,
  ) {
    const r = await fetch(
      `http://127.0.0.1:${apiPort}/v1/tenants/${tenant}/${route}`,
      {
        method,
        headers: {
          authorization: "Bearer " + credential,
          "Content-Type": "application/json",
        },
        ...(body ? { body: JSON.stringify(body) } : {}),
      },
    );
    const value = await r.json();
    assert.equal(r.status, expected, JSON.stringify(value));
    return value;
  }
  await until(() => request("GET", "integration-status"));
  const credentials = grpc.credentials.createSsl(
    fs.readFileSync(path.join(tls, "ca.pem")),
    fs.readFileSync(path.join(tls, "execution-worker.key")),
    fs.readFileSync(path.join(tls, "execution-worker.pem")),
  );
  const worker = new Integrations.IntegrationServiceClient(
    integrationAddress,
    credentials,
    { "grpc.ssl_target_name_override": "integration-service" },
  );
  channels.push(worker);
  const run = (id) =>
    new Promise((resolve, reject) =>
      worker.runJob(
        { tenantId: tenant, id },
        new grpc.Metadata(),
        { deadline: Date.now() + 35000 },
        (e, v) => (e ? reject(e) : resolve(v)),
      ),
    );
  const python = process.env.RPC_PYTHON || "python3",
    fixture = path.join(scratch, "lease.docx");
  await exec(python, [
    "-c",
    "import sys,zipfile\nwith zipfile.ZipFile(sys.argv[1],'w') as z:\n z.writestr('[Content_Types].xml','<Types>wordprocessingml.document.main+xml</Types>')\n z.writestr('word/document.xml','<w:document xmlns:w=\"x\"><w:p><w:r><w:t>Lease agreement. Rent is due monthly.</w:t></w:r></w:p></w:document>')",
    fixture,
  ]);
  const data = fs.readFileSync(fixture),
    sha = createHash("sha256").update(data).digest("hex");
  async function upload() {
    const result = await request(
      "POST",
      `matters/${matter}/files/uploads`,
      {
        filename: "lease.docx",
        mediaType:
          "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
        sizeBytes: data.length,
        sha256: sha,
        idempotencyKey: randomUUID(),
      },
      201,
    );
    const headers = Object.fromEntries(
      result.requiredHeaders.map((x) => [x.name, x.value]),
    );
    assert.equal(
      (await fetch(result.uploadUrl, { method: "PUT", headers, body: data }))
        .status,
      200,
    );
    return result.file.id;
  }
  const file = await upload();
  await request("POST", `matters/${matter}/files/${file}/finalize`, {}, 202);
  // Actual Temporal dispatcher/worker execute the scan/parse/index job from references only.
  const temporal = process.env.TEMPORAL_BIN || "temporal";
  launch(
    temporal,
    [
      "server",
      "start-dev",
      "--ip",
      "127.0.0.1",
      "--port",
      String(temporalPort),
      "--ui-port",
      String(await port()),
      "--db-filename",
      path.join(scratch, "temporal.db"),
    ],
    "temporal",
  );
  await until(async () => {
    await exec(
      temporal,
      [
        "operator",
        "cluster",
        "health",
        "--address",
        `127.0.0.1:${temporalPort}`,
      ],
      { timeout: 3000 },
    );
    return true;
  });
  launch(python, ["-m", "app.grpc_server"], "python", {
    ...identity("ai-capabilities"),
    PYTHONPATH: path.resolve("services/ai-capabilities"),
    RPC_PYTHON_PATH: path.resolve("contracts/gen/python"),
    AI_GRPC_BIND: `127.0.0.1:${aiPort}`,
    AI_PORT: String(await port()),
    LEGAL_AI_ENABLED: "true",
    LEGAL_AI_BASE_URL: `http://127.0.0.1:${modelport}/v1`,
    LEGAL_AI_MODEL: "fixture-model",
    LEGAL_AI_REVISION: "fixture-v1",
    LEGAL_AI_API_KEY: "synthetic-only",
  });
  launch(
    process.execPath,
    ["services/root-supervisor/dist/main.js"],
    "supervisor",
    {
      ...identity("root-supervisor"),
      AI_GRPC_ADDRESS: `127.0.0.1:${aiPort}`,
      SUPERVISOR_GRPC_BIND: `127.0.0.1:${supervisorPort}`,
      SUPERVISOR_PORT: String(await port()),
    },
  );
  const common = {
    TEMPORAL_ADDRESS: `127.0.0.1:${temporalPort}`,
    TEMPORAL_NAMESPACE: "default",
    TEMPORAL_TASK_QUEUE: "configured-" + tenant,
    WORKFLOW_DATABASE_URL: process.env.TEST_WORKFLOW_DATABASE_URL,
  };
  launch(path.resolve("work/execution-worker"), [], "worker", {
    ...common,
    ...identity("execution-worker"),
    SUPERVISOR_GRPC_ADDRESS: `127.0.0.1:${supervisorPort}`,
    CONTEXT_GRPC_ADDRESS: contextAddress,
    INTEGRATION_GRPC_ADDRESS: integrationAddress,
    LEGAL_AI_ENABLED: "true",
    CALENDAR_GRPC_ADDRESS: "",
  });
  launch(path.resolve("work/outbox-dispatcher"), [], "dispatcher", {
    ...common,
    DISPATCHER_DATABASE_URL: process.env.TEST_DISPATCHER_DATABASE_URL,
    ENABLE_INTEGRATION_JOBS: "true",
    ENABLE_CALENDAR_REMINDERS: "false",
  });
  const stored = await until(async () => {
    const f = await request("GET", `matters/${matter}/files/${file}`);
    assert(!["rejected", "expired"].includes(f.file.state), JSON.stringify(f));
    return f.file.state === "completed" && f.file;
  });
  const download = await request(
    "GET",
    `matters/${matter}/files/${file}/download`,
  );
  assert.equal(download.expiresSeconds, 60);
  assert.deepEqual(
    Buffer.from(await (await fetch(download.downloadUrl)).arrayBuffer()),
    data,
  );
  assert.equal((await run(file)).state, "completed");
  const execution = await request(
    "POST",
    "executions",
    {
      capabilityId: "matter.qa.v1",
      matterId: matter,
      input: { question: "When is rent due?" },
      idempotencyKey: randomUUID(),
    },
    202,
  );
  await until(async () => {
    const e = await request("GET", `executions/${execution.executionId}`);
    assert(!["failed", "rejected"].includes(e.status), JSON.stringify(e));
    return e.status === "awaiting_review";
  });
  const review = await request(
    "GET",
    `executions/${execution.executionId}/review`,
    undefined,
    200,
    reviewToken,
  );
  assert(review.artifact.output.claims.length);
  assert(review.artifact.output.limitations.length);
  assert.equal(modelCalls, 1);
  await request(
    "POST",
    `executions/${execution.executionId}/review`,
    { expectedSha256: review.artifactSha256, decision: "approve" },
    403,
  );
  await request(
    "POST",
    `executions/${execution.executionId}/review`,
    { expectedSha256: review.artifactSha256, decision: "approve" },
    200,
    reviewToken,
  );
  const rejectedFile = await upload();
  infected = true;
  await request(
    "POST",
    `matters/${matter}/files/${rejectedFile}/finalize`,
    {},
    202,
  );
  await until(async () => {
    const f = await request("GET", `matters/${matter}/files/${rejectedFile}`);
    return f.file.state === "rejected";
  });
  const count = await pool.query(
    "SELECT count(*)::int n FROM document_assets WHERE tenant_id=$1",
    [tenant],
  );
  assert.equal(count.rows[0].n, 1);
  await pool.query(
    "DELETE FROM matter_grants WHERE tenant_id=$1 AND principal_id=$2",
    [tenant, owner],
  );
  await request(
    "GET",
    `matters/${matter}/files/${file}/download`,
    undefined,
    403,
  );
  console.log(
    "PASS: private upload → Temporal → ClamAV → mTLS Go/Rust DOCX parser → context index; configured model → full attorney review; malware/revoked access denied",
  );
})()
  .catch((error) => {
    console.error(error);
    console.error("Synthetic diagnostics:", scratch);
    process.exitCode = 1;
  })
  .finally(async () => {
    for (const c of channels) c.close();
    for (const p of children) p.kill("SIGTERM");
    await sleep(1500);
    for (const p of children) if (p.exitCode === null) p.kill("SIGKILL");
    for (const s of servers) {
      s.closeAllConnections?.();
      await new Promise((r) => s.close(r));
    }
    try {
      await pool.query(
        "DELETE FROM domain_event_deliveries WHERE event_id IN(SELECT id FROM domain_event_journal WHERE tenant_id=$1)",
        [tenant],
      );
      for (const table of [
        "domain_event_inbox",
        "domain_aggregate_heads",
        "domain_event_journal",
        "legal_review_decisions",
        "execution_context_grants",
        "context_commands",
        "context_packages",
        "document_embeddings",
        "document_passages",
      ])
        await pool.query(`DELETE FROM ${table} WHERE tenant_id=$1`, [tenant]);
      await pool.query(
        "UPDATE document_assets SET current_version_id=NULL WHERE tenant_id=$1",
        [tenant],
      );
      for (const table of [
        "document_versions",
        "document_assets",
        "file_ingestions",
        "integration_commands",
        "integration_jobs",
        "outbox_events",
        "audit_events",
        "ai_executions",
        "matter_grants",
        "principal_capabilities",
        "memberships",
        "matters",
      ])
        await pool.query(`DELETE FROM ${table} WHERE tenant_id=$1`, [tenant]);
      await pool.query("DELETE FROM tenants WHERE id=$1", [tenant]);
    } catch (error) {
      console.error("Fixture cleanup failed", error.code);
      process.exitCode = 1;
    }
    if (lock) {
      await lock.query("SELECT pg_advisory_unlock(604182701)");
      lock.release();
    }
    await pool.end();
  });

-- LOCAL DEVELOPMENT SQL. Provision roles/secrets through a secrets manager in production.
CREATE EXTENSION IF NOT EXISTS pgcrypto;
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='lawpilot_app') THEN
    CREATE ROLE lawpilot_app LOGIN PASSWORD 'local-dev-app-only' NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION;
  END IF;
END $$;

CREATE TABLE tenants (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE memberships (
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  principal_id text NOT NULL,
  status text NOT NULL CHECK(status IN ('active','suspended','revoked')),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id,principal_id)
);
CREATE TABLE principal_capabilities (
  tenant_id uuid NOT NULL,
  principal_id text NOT NULL,
  capability_id text NOT NULL,
  PRIMARY KEY(tenant_id,principal_id,capability_id),
  FOREIGN KEY(tenant_id,principal_id) REFERENCES memberships(tenant_id,principal_id)
);
CREATE TABLE matters (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  title text NOT NULL,
  status text NOT NULL DEFAULT 'active',
  resource_owner text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(tenant_id,id)
);
CREATE INDEX idx_matters_tenant ON matters(tenant_id,id);
CREATE TABLE matter_grants (
  tenant_id uuid NOT NULL,
  matter_id uuid NOT NULL,
  principal_id text NOT NULL,
  permission text NOT NULL,
  PRIMARY KEY(tenant_id,matter_id,principal_id,permission),
  FOREIGN KEY(tenant_id,matter_id) REFERENCES matters(tenant_id,id),
  FOREIGN KEY(tenant_id,principal_id) REFERENCES memberships(tenant_id,principal_id)
);
CREATE TABLE provider_connections (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  provider_id text NOT NULL,
  region text,
  credential_ref text, -- opaque vault reference, never OAuth token
  status text NOT NULL DEFAULT 'disabled',
  capabilities jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(tenant_id,id)
);
CREATE TABLE external_mappings (
  tenant_id uuid NOT NULL,
  provider_connection_id uuid NOT NULL,
  resource_type text NOT NULL,
  internal_id uuid NOT NULL,
  external_id text NOT NULL,
  external_etag text,
  PRIMARY KEY(tenant_id,provider_connection_id,resource_type,internal_id),
  UNIQUE(tenant_id,provider_connection_id,resource_type,external_id),
  FOREIGN KEY(tenant_id,provider_connection_id) REFERENCES provider_connections(tenant_id,id)
);
CREATE TABLE document_assets (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL,
  matter_id uuid NOT NULL,
  storage_ref text NOT NULL,
  content_sha256 text NOT NULL,
  version integer NOT NULL CHECK(version>0),
  scan_status text NOT NULL DEFAULT 'pending',
  created_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY(tenant_id,matter_id) REFERENCES matters(tenant_id,id),
  UNIQUE(tenant_id,id)
);
CREATE TABLE document_passages (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL,
  document_id uuid NOT NULL,
  page_number integer CHECK(page_number>0),
  source_sha256 text NOT NULL,
  text_ref text NOT NULL, -- authorized external ref; not raw sensitive text in logs
  FOREIGN KEY(tenant_id,document_id) REFERENCES document_assets(tenant_id,id)
);
CREATE TABLE template_versions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  name text NOT NULL,
  version integer NOT NULL CHECK(version>0),
  approved_by text,
  artifact_ref text NOT NULL,
  approved_at timestamptz,
  UNIQUE(tenant_id,name,version)
);
CREATE TABLE approval_requests (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL,
  matter_id uuid NOT NULL,
  draft_id uuid NOT NULL,
  draft_version integer NOT NULL CHECK(draft_version>0),
  draft_sha256 text NOT NULL,
  state text NOT NULL CHECK(state IN ('pending','approved','rejected','expired')),
  approver_id text,
  decided_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY(tenant_id,matter_id) REFERENCES matters(tenant_id,id)
);
CREATE TABLE workflow_instances (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  workflow_type text NOT NULL,
  workflow_version text NOT NULL,
  state text NOT NULL,
  external_workflow_id text,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE ai_executions (
  id uuid PRIMARY KEY,
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  principal_id text NOT NULL,
  idempotency_key text NOT NULL,
  request_sha256 text NOT NULL,
  capability_id text NOT NULL,
  matter_id uuid,
  status text NOT NULL CHECK(status IN ('accepted','completed','failed')),
  result jsonb,
  started_at timestamptz NOT NULL DEFAULT now(),
  completed_at timestamptz,
  UNIQUE(tenant_id,principal_id,idempotency_key),
  FOREIGN KEY(tenant_id,matter_id) REFERENCES matters(tenant_id,id)
);
CREATE TABLE audit_events (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  actor_id text NOT NULL,
  action text NOT NULL,
  resource_id uuid,
  decision_code text,
  occurred_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE outbox_events (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  event_type text NOT NULL,
  event_version text NOT NULL,
  aggregate_id uuid,
  event_body jsonb NOT NULL,
  publish_state text NOT NULL DEFAULT 'pending',
  created_at timestamptz NOT NULL DEFAULT now()
);

-- Always use an unprivileged app role, not postgres or a schema owner.
GRANT USAGE ON SCHEMA public TO lawpilot_app;
GRANT SELECT ON tenants,memberships,principal_capabilities,matters,matter_grants,provider_connections,
 external_mappings,document_assets,document_passages,template_versions,approval_requests,workflow_instances,
 ai_executions,audit_events,outbox_events TO lawpilot_app;
GRANT INSERT,UPDATE ON ai_executions TO lawpilot_app;
GRANT INSERT ON audit_events TO lawpilot_app;

-- Uniform firm isolation. Matter ACL is additionally required in application access queries.
DO $$ DECLARE t text; BEGIN
  FOREACH t IN ARRAY ARRAY[
    'memberships','principal_capabilities','matters','matter_grants','provider_connections',
    'external_mappings','document_assets','document_passages','template_versions',
    'approval_requests','workflow_instances','ai_executions','audit_events','outbox_events'
  ] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY',t);
    EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY',t);
    EXECUTE format('CREATE POLICY tenant_guard ON %I USING (tenant_id::text = current_setting(''app.tenant_id'',true)) WITH CHECK (tenant_id::text = current_setting(''app.tenant_id'',true))',t);
  END LOOP;
END $$;

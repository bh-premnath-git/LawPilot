BEGIN;
DO $$ BEGIN IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='lawpilot_integration') THEN CREATE ROLE lawpilot_integration LOGIN PASSWORD 'local-dev-integration-only' NOSUPERUSER NOBYPASSRLS;END IF;END $$;
CREATE TABLE file_ingestions(
 id uuid PRIMARY KEY,tenant_id uuid NOT NULL,matter_id uuid NOT NULL,principal_id text NOT NULL,filename text NOT NULL,media_type text NOT NULL,
 size_bytes bigint NOT NULL CHECK(size_bytes BETWEEN 1 AND 10485760),source_sha256 text NOT NULL CHECK(source_sha256~'^[a-f0-9]{64}$'),
 quarantine_key text NOT NULL,clean_key text NOT NULL,state text NOT NULL DEFAULT 'awaiting_upload' CHECK(state IN('awaiting_upload','queued','processing','completed','rejected','expired')),
 scan_status text NOT NULL DEFAULT 'pending' CHECK(scan_status IN('pending','clean','infected')),parsed_sha256 text,document_id uuid,version_id uuid,error_code text,
 expires_at timestamptz NOT NULL,created_at timestamptz NOT NULL DEFAULT now(),UNIQUE(tenant_id,id),FOREIGN KEY(tenant_id,matter_id) REFERENCES matters(tenant_id,id)
);
CREATE TABLE integration_jobs(
 id uuid PRIMARY KEY,tenant_id uuid NOT NULL,principal_id text NOT NULL,kind text NOT NULL CHECK(kind IN('file','provider_sync','provider_update','legal_review')),resource_id uuid NOT NULL,
 workflow_id text NOT NULL UNIQUE,state text NOT NULL DEFAULT 'queued' CHECK(state IN('queued','running','completed','failed','uncertain','rejected')),error_code text,
 cursor jsonb NOT NULL DEFAULT '{}'::jsonb,created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now(),UNIQUE(tenant_id,id)
);
CREATE TABLE integration_commands(tenant_id uuid NOT NULL,principal_id text NOT NULL,idempotency_key text NOT NULL,request_sha256 text NOT NULL,response bytea NOT NULL,PRIMARY KEY(tenant_id,principal_id,idempotency_key));
ALTER TABLE matters ADD COLUMN revision bigint NOT NULL DEFAULT 1;
ALTER TABLE provider_connections ADD COLUMN revision bigint NOT NULL DEFAULT 1;
ALTER TABLE provider_connections ADD COLUMN encrypted_tokens bytea;
ALTER TABLE provider_connections ADD COLUMN token_expires_at timestamptz;
ALTER TABLE provider_connections ADD COLUMN created_by text;
CREATE TABLE provider_connection_grants(tenant_id uuid NOT NULL,connection_id uuid NOT NULL,principal_id text NOT NULL,permission text NOT NULL CHECK(permission IN('read','manage','sync','approve')),PRIMARY KEY(tenant_id,connection_id,principal_id,permission),FOREIGN KEY(tenant_id,connection_id) REFERENCES provider_connections(tenant_id,id));
CREATE TABLE provider_oauth_states(id uuid PRIMARY KEY,tenant_id uuid NOT NULL,connection_id uuid NOT NULL,principal_id text NOT NULL,state_sha256 text NOT NULL,expires_at timestamptz NOT NULL,consumed_at timestamptz,FOREIGN KEY(tenant_id,connection_id) REFERENCES provider_connections(tenant_id,id));
CREATE TABLE provider_sync_bindings(
 tenant_id uuid NOT NULL,connection_id uuid NOT NULL,resource_type text NOT NULL CHECK(resource_type IN('matter','calendar','calendar_entry')),external_id text NOT NULL,local_id uuid NOT NULL,
 external_etag text NOT NULL,seen_job_id uuid,last_synced_at timestamptz NOT NULL DEFAULT now(),snapshot_sha256 text NOT NULL,deleted boolean NOT NULL DEFAULT false,
 PRIMARY KEY(tenant_id,connection_id,resource_type,external_id),UNIQUE(tenant_id,resource_type,local_id),FOREIGN KEY(tenant_id,connection_id) REFERENCES provider_connections(tenant_id,id)
);
CREATE TABLE provider_update_proposals(
 id uuid PRIMARY KEY,tenant_id uuid NOT NULL,connection_id uuid NOT NULL,matter_id uuid NOT NULL,external_id text NOT NULL,expected_etag text NOT NULL,expected_revision bigint NOT NULL,
 title text NOT NULL,artifact_sha256 text NOT NULL,proposed_by text NOT NULL,reviewed_by text,state text NOT NULL DEFAULT 'pending' CHECK(state IN('pending','approved','rejected','inflight','completed','conflict','uncertain')),created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(tenant_id,matter_id) REFERENCES matters(tenant_id,id),FOREIGN KEY(tenant_id,connection_id) REFERENCES provider_connections(tenant_id,id)
);
ALTER TABLE document_versions DROP CONSTRAINT document_versions_full_text_check;
ALTER TABLE document_versions ADD CONSTRAINT document_versions_full_text_check CHECK(octet_length(full_text) BETWEEN 1 AND 262144);
ALTER TABLE document_versions ADD COLUMN source_file_id uuid;
ALTER TABLE document_versions ADD COLUMN source_file_sha256 text;
ALTER TABLE document_versions ADD CONSTRAINT version_source_file FOREIGN KEY(tenant_id,source_file_id) REFERENCES file_ingestions(tenant_id,id);
-- External calendars remain provider-owned; native mutation RPCs must refuse them.
ALTER TABLE calendars DROP CONSTRAINT calendars_authority_check;
ALTER TABLE calendars ADD CONSTRAINT calendars_authority_check CHECK(authority IN('lawpilot-native','provider:clio-manage','provider:generic-rest'));
DO $$ DECLARE t text;BEGIN FOREACH t IN ARRAY ARRAY['file_ingestions','integration_jobs','integration_commands','provider_connection_grants','provider_oauth_states','provider_sync_bindings','provider_update_proposals'] LOOP
 EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY',t);EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY',t);EXECUTE format('CREATE POLICY tenant_guard ON %I USING(tenant_id::text=current_setting(''app.tenant_id'',true)) WITH CHECK(tenant_id::text=current_setting(''app.tenant_id'',true))',t);
END LOOP;END $$;
GRANT USAGE ON SCHEMA public TO lawpilot_integration;
GRANT SELECT ON memberships,principal_capabilities,matter_grants,matters,calendars,calendar_grants,calendar_entries,provider_connections,file_ingestions,integration_jobs,integration_commands,provider_connection_grants,provider_oauth_states,provider_sync_bindings,provider_update_proposals,document_assets,document_versions TO lawpilot_integration;
GRANT INSERT ON file_ingestions,integration_jobs,integration_commands,provider_connections,provider_connection_grants,provider_oauth_states,provider_sync_bindings,provider_update_proposals,matters,calendars,calendar_grants,calendar_entries,audit_events,outbox_events TO lawpilot_integration;
GRANT DELETE ON provider_connection_grants TO lawpilot_integration;
GRANT UPDATE ON file_ingestions,integration_jobs,provider_connections,provider_oauth_states,provider_sync_bindings TO lawpilot_integration;
GRANT UPDATE(state,reviewed_by) ON provider_update_proposals TO lawpilot_integration;
GRANT UPDATE(title,status,revision) ON matters TO lawpilot_integration;
GRANT UPDATE(name,revision) ON calendars TO lawpilot_integration;
GRANT UPDATE(title,description,start_at,end_at,time_zone,all_day,start_date,end_date,status,revision,updated_at) ON calendar_entries TO lawpilot_integration;
GRANT SELECT ON file_ingestions,integration_jobs TO lawpilot_context;
GRANT UPDATE(state,document_id,version_id) ON file_ingestions TO lawpilot_context;
CREATE FUNCTION claim_integration_commands(p_worker text,p_limit integer DEFAULT 10,p_seconds integer DEFAULT 30)
RETURNS TABLE(event_id uuid,tenant_id uuid,job_id uuid,kind text,workflow_id text,lease_token uuid)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$BEGIN
 IF p_worker IS NULL OR length(p_worker) NOT BETWEEN 1 AND 200 OR p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 100 OR p_seconds IS NULL OR p_seconds NOT BETWEEN 10 AND 300 THEN RAISE EXCEPTION 'invalid claim';END IF;
 RETURN QUERY WITH picked AS(SELECT o.id FROM public.outbox_events o JOIN public.integration_jobs j ON j.tenant_id=o.tenant_id AND j.id=o.aggregate_id WHERE o.event_type IN('file.process.requested.v1','provider.sync.requested.v1','provider.update.requested.v1','legal.review.requested.v1') AND((o.publish_state='pending' AND o.available_at<=now())OR(o.publish_state='leased' AND o.lease_expires_at<=now())) ORDER BY o.created_at FOR UPDATE OF o SKIP LOCKED LIMIT p_limit),claimed AS(UPDATE public.outbox_events o SET publish_state='leased',lease_owner=p_worker,lease_token=gen_random_uuid(),lease_expires_at=now()+make_interval(secs=>p_seconds),delivery_attempts=o.delivery_attempts+1 FROM picked p WHERE p.id=o.id RETURNING o.*)SELECT c.id,c.tenant_id,j.id,j.kind,j.workflow_id,c.lease_token FROM claimed c JOIN public.integration_jobs j ON j.tenant_id=c.tenant_id AND j.id=c.aggregate_id;
END $$;
REVOKE ALL ON FUNCTION claim_integration_commands(text,integer,integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_integration_commands(text,integer,integer) TO lawpilot_dispatcher;
-- General publication must not steal the new durable commands.
-- Existing functions reference the static exclusion lists; update them in 013
-- together with the event schema registry and worker rollout.
COMMIT;

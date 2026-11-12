BEGIN;
DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='lawpilot_context') THEN CREATE ROLE lawpilot_context LOGIN PASSWORD 'local-dev-context-only' NOSUPERUSER NOBYPASSRLS; END IF;
 IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='lawpilot_events') THEN CREATE ROLE lawpilot_events LOGIN PASSWORD 'local-dev-events-only' NOSUPERUSER NOBYPASSRLS; END IF;
END $$;
ALTER TABLE document_assets ADD COLUMN title text NOT NULL DEFAULT '';
ALTER TABLE document_assets ADD COLUMN revision bigint NOT NULL DEFAULT 0;
ALTER TABLE document_assets ADD COLUMN current_version_id uuid;
ALTER TABLE document_assets ADD COLUMN deleted_at timestamptz;
CREATE TABLE document_versions(
 id uuid PRIMARY KEY,tenant_id uuid NOT NULL,document_id uuid NOT NULL,revision bigint NOT NULL CHECK(revision>0),
 content_sha256 text NOT NULL CHECK(content_sha256 ~ '^[a-f0-9]{64}$'),full_text text NOT NULL CHECK(octet_length(full_text) BETWEEN 1 AND 24576),
 parser_version text NOT NULL DEFAULT 'native-text.v1',created_by text NOT NULL,created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(tenant_id,id),UNIQUE(tenant_id,document_id,revision),UNIQUE(tenant_id,document_id,id),FOREIGN KEY(tenant_id,document_id) REFERENCES document_assets(tenant_id,id)
);
ALTER TABLE document_assets ADD CONSTRAINT current_document_version FOREIGN KEY(tenant_id,id,current_version_id) REFERENCES document_versions(tenant_id,document_id,id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE document_passages ADD COLUMN version_id uuid;
ALTER TABLE document_passages ADD COLUMN body text;
ALTER TABLE document_passages ADD COLUMN byte_start integer;
ALTER TABLE document_passages ADD COLUMN byte_end integer;
ALTER TABLE document_passages ADD COLUMN passage_sha256 text;
ALTER TABLE document_passages ADD CONSTRAINT passage_version FOREIGN KEY(tenant_id,document_id,version_id) REFERENCES document_versions(tenant_id,document_id,id);
ALTER TABLE document_passages ADD CONSTRAINT passage_content CHECK(version_id IS NULL OR(body IS NOT NULL AND byte_start>=0 AND byte_end>byte_start AND byte_end-byte_start=octet_length(body) AND passage_sha256 ~ '^[a-f0-9]{64}$'));
ALTER TABLE document_passages ADD CONSTRAINT passage_tenant_key UNIQUE(tenant_id,id);
ALTER TABLE document_passages ADD CONSTRAINT passage_hash_key UNIQUE(tenant_id,id,passage_sha256);
CREATE INDEX passage_fts ON document_passages USING gin(to_tsvector('english',coalesce(body,''))) WHERE version_id IS NOT NULL;
CREATE TABLE document_embeddings(
 tenant_id uuid NOT NULL,passage_id uuid NOT NULL,passage_sha256 text NOT NULL,profile_id text NOT NULL,embedding vector(1536) NOT NULL,
 PRIMARY KEY(tenant_id,passage_id,profile_id),FOREIGN KEY(tenant_id,passage_id,passage_sha256) REFERENCES document_passages(tenant_id,id,passage_sha256)
);
CREATE TABLE context_packages(
 id uuid PRIMARY KEY,tenant_id uuid NOT NULL,matter_id uuid NOT NULL,principal_id text NOT NULL,recipe_id text NOT NULL,
 snapshot_sha256 text NOT NULL,body jsonb NOT NULL,expires_at timestamptz NOT NULL,invalidated boolean NOT NULL DEFAULT false,created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 FOREIGN KEY(tenant_id,matter_id) REFERENCES matters(tenant_id,id)
);
CREATE INDEX context_cache ON context_packages(tenant_id,principal_id,matter_id,snapshot_sha256);
CREATE TABLE context_commands(tenant_id uuid NOT NULL,principal_id text NOT NULL,idempotency_key text NOT NULL,request_sha256 text NOT NULL,response bytea NOT NULL,PRIMARY KEY(tenant_id,principal_id,idempotency_key));
CREATE TABLE domain_event_journal(
 id uuid PRIMARY KEY,position bigint GENERATED ALWAYS AS IDENTITY UNIQUE,tenant_id uuid NOT NULL REFERENCES tenants(id),event_type text NOT NULL,schema_version text NOT NULL,
 aggregate_type text NOT NULL,aggregate_id uuid NOT NULL,aggregate_revision bigint NOT NULL CHECK(aggregate_revision>=0),matter_id uuid,
 source text NOT NULL,correlation_id text NOT NULL,causation_id text NOT NULL,occurred_at timestamptz NOT NULL,published_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX domain_feed ON domain_event_journal(tenant_id,matter_id,position);
CREATE TABLE domain_event_deliveries(
 event_id uuid NOT NULL REFERENCES domain_event_journal(id),consumer_id text NOT NULL CHECK(consumer_id IN('context-invalidation.v1','aggregate-heads.v1')),
 state text NOT NULL DEFAULT 'pending' CHECK(state IN('pending','leased','delivered','dead_letter')),attempts integer NOT NULL DEFAULT 0,
 available_at timestamptz NOT NULL DEFAULT now(),lease_token uuid,lease_expires_at timestamptz,last_error_code text,PRIMARY KEY(event_id,consumer_id)
);
CREATE TABLE domain_event_inbox(tenant_id uuid NOT NULL,event_id uuid NOT NULL,consumer_id text NOT NULL,outcome text NOT NULL,processed_at timestamptz NOT NULL DEFAULT now(),PRIMARY KEY(event_id,consumer_id));
CREATE TABLE domain_aggregate_heads(tenant_id uuid NOT NULL,consumer_id text NOT NULL,aggregate_type text NOT NULL,aggregate_id uuid NOT NULL,revision bigint NOT NULL,event_position bigint NOT NULL,PRIMARY KEY(tenant_id,consumer_id,aggregate_type,aggregate_id));
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['document_versions','document_embeddings','context_packages','context_commands','domain_event_journal','domain_event_inbox','domain_aggregate_heads'] LOOP
  EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY',t);EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_guard ON %I USING(tenant_id::text=current_setting(''app.tenant_id'',true)) WITH CHECK(tenant_id::text=current_setting(''app.tenant_id'',true))',t);
 END LOOP;
END $$;
ALTER TABLE domain_event_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE domain_event_deliveries FORCE ROW LEVEL SECURITY;
-- This table is reachable only through narrow leased-delivery functions.
GRANT USAGE ON SCHEMA public TO lawpilot_context,lawpilot_events;
GRANT SELECT ON memberships,principal_capabilities,matter_grants,matters,document_assets,document_versions,document_passages,document_embeddings,context_packages,context_commands,domain_event_journal,ai_executions TO lawpilot_context;
GRANT INSERT ON document_assets,document_versions,document_passages,document_embeddings,context_packages,context_commands,audit_events,outbox_events TO lawpilot_context;
GRANT UPDATE(title,revision,current_version_id,deleted_at,content_sha256,version,scan_status) ON document_assets TO lawpilot_context;
GRANT UPDATE(invalidated) ON context_packages TO lawpilot_context;
-- Prevent legacy API SQL access from bypassing the dedicated document boundary.
REVOKE SELECT ON document_assets,document_passages FROM lawpilot_app;
CREATE TABLE domain_event_types(event_type text PRIMARY KEY,schema_version text NOT NULL CHECK(schema_version='1.0'));
INSERT INTO domain_event_types VALUES ('document.version.created.v1','1.0'),('document.deleted.v1','1.0'),('matter.updated.v1','1.0'),('calendar.created.v1','1.0'),('calendar.grants.updated.v1','1.0'),('calendar.overlap_policy.changed.v1','1.0'),('calendar.entry.created.v1','1.0'),('calendar.entry.updated.v1','1.0'),('calendar.entry.cancelled.v1','1.0'),('calendar.entry.proposed.v1','1.0'),('calendar.proposal.approved.v1','1.0'),('calendar.proposal.rejected.v1','1.0'),('calendar.reminder.due.v1','1.0'),('lawpilot.ai.completed.v1','1.0'),('lawpilot.ai.failed.v1','1.0'),('lawpilot.ai.queued.v1','1.0'),('lawpilot.ai.running.v1','1.0'),('lawpilot.ai.awaiting_review.v1','1.0'),('lawpilot.ai.rejected.v1','1.0');
CREATE FUNCTION claim_domain_events(p_worker text,p_limit integer DEFAULT 20,p_seconds integer DEFAULT 30)
RETURNS TABLE(event_id uuid,tenant_id uuid,event_type text,event_version text,aggregate_id uuid,event_body jsonb,lease_token uuid,created_at timestamptz)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$ BEGIN
 IF length(coalesce(p_worker,'')) NOT BETWEEN 1 AND 200 OR p_limit IS NULL OR p_seconds IS NULL OR p_limit NOT BETWEEN 1 AND 100 OR p_seconds NOT BETWEEN 10 AND 300 THEN RAISE EXCEPTION 'invalid claim'; END IF;
 RETURN QUERY WITH picked AS(SELECT o.id FROM public.outbox_events o WHERE o.event_type NOT IN('lawpilot.ai.accepted.v1','lawpilot.ai.cancelled.v1','calendar.reminder.scheduled.v1','calendar.reminder.cancelled.v1') AND((o.publish_state='pending' AND o.available_at<=now())OR(o.publish_state='leased' AND o.lease_expires_at<=now())) ORDER BY o.created_at FOR UPDATE SKIP LOCKED LIMIT p_limit),claimed AS(UPDATE public.outbox_events o SET publish_state='leased',lease_owner=p_worker,lease_token=gen_random_uuid(),lease_expires_at=now()+make_interval(secs=>p_seconds),delivery_attempts=o.delivery_attempts+1 FROM picked p WHERE p.id=o.id RETURNING o.*)SELECT c.id,c.tenant_id,c.event_type,c.event_version,c.aggregate_id,c.event_body,c.lease_token,c.created_at FROM claimed c;
END $$;
-- The publisher sends validated metadata, never the original event JSON. Domain
-- identity, source, revisions and matter scope are rederived from authoritative rows.
CREATE FUNCTION publish_domain_event(p_id uuid,p_token uuid,p_error text DEFAULT NULL)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE o public.outbox_events; kind text; rev bigint; matter uuid; src text;
BEGIN
 SELECT * INTO o FROM public.outbox_events WHERE id=p_id AND lease_token=p_token AND publish_state='leased' AND lease_expires_at>now() FOR UPDATE;
 IF NOT FOUND THEN RETURN false; END IF;
 IF p_error IS NULL AND NOT EXISTS(SELECT 1 FROM public.domain_event_types WHERE event_type=o.event_type AND schema_version=o.event_version)THEN p_error:='UNREGISTERED_EVENT';END IF;
 IF p_error IS NULL THEN
  IF jsonb_typeof(o.event_body) IS DISTINCT FROM 'object' OR jsonb_typeof(o.event_body->'data') IS DISTINCT FROM 'object' THEN p_error:='INVALID_EVENT_SCHEMA';
  ELSIF o.event_body->>'type' IS DISTINCT FROM o.event_type OR o.event_body->>'tenantId' IS DISTINCT FROM o.tenant_id::text OR o.event_body->>'aggregateId' IS DISTINCT FROM o.aggregate_id::text OR o.aggregate_id IS NULL OR o.event_body->>'specversion' IS DISTINCT FROM '1.0' OR o.event_body->>'schemaVersion' IS DISTINCT FROM '1.0' THEN p_error:='INVALID_EVENT_SCHEMA';
  ELSIF EXISTS(SELECT 1 FROM jsonb_object_keys(o.event_body)k WHERE k NOT IN('specversion','id','source','type','time','tenantId','schemaVersion','aggregateId','correlationId','causationId','data')) OR EXISTS(SELECT 1 FROM jsonb_object_keys(o.event_body->'data')k WHERE k NOT IN('revision','executionId','state','decisionCode')) THEN p_error:='INVALID_EVENT_SCHEMA';
  ELSIF o.event_type NOT LIKE 'lawpilot.ai.%' AND (jsonb_typeof(o.event_body->'data'->'revision') IS DISTINCT FROM 'number' OR coalesce(o.event_body->'data'->>'revision','') !~ '^[1-9][0-9]{0,17}$') THEN p_error:='INVALID_EVENT_SCHEMA';
  ELSIF o.event_type LIKE 'document.%' THEN
   SELECT a.revision INTO rev FROM public.document_assets a WHERE a.tenant_id=o.tenant_id AND a.id=o.aggregate_id;
   IF NOT FOUND OR (o.event_body->'data'->>'revision')::bigint>rev THEN p_error:='INVALID_EVENT_SCHEMA';END IF;
  END IF;
 END IF;
 IF p_error IS NOT NULL THEN
  UPDATE public.outbox_events SET publish_state=CASE WHEN p_error IN('INVALID_EVENT_SCHEMA','UNREGISTERED_EVENT') OR delivery_attempts>=10 THEN 'dead_letter' ELSE 'pending' END,available_at=now()+interval '10 seconds',last_error_code=CASE WHEN p_error IN('INVALID_EVENT_SCHEMA','UNREGISTERED_EVENT') THEN p_error ELSE 'EVENT_DELIVERY_FAILED' END,lease_token=NULL,lease_expires_at=NULL WHERE id=o.id;RETURN true;
 END IF;
 kind:=CASE WHEN o.event_type LIKE 'document.%' THEN 'document' WHEN o.event_type LIKE 'calendar.%' THEN 'calendar' WHEN o.event_type LIKE 'lawpilot.ai.%' THEN 'ai_execution' ELSE 'matter' END;
 rev:=coalesce((o.event_body->'data'->>'revision')::bigint,0);
 IF kind='document' THEN SELECT a.matter_id INTO matter FROM public.document_assets a WHERE a.id=o.aggregate_id AND a.tenant_id=o.tenant_id; END IF;
 IF kind='matter' THEN SELECT m.id INTO matter FROM public.matters m WHERE m.id=o.aggregate_id AND m.tenant_id=o.tenant_id; END IF;
 src:=CASE WHEN kind='document' THEN 'context-engine' WHEN kind='calendar' THEN 'calendar-service' ELSE 'platform' END;
 -- Serialize journal positions through commit so an SSE cursor cannot skip an
 -- earlier uncommitted position produced by a different publisher instance.
 PERFORM pg_advisory_xact_lock(604182702);
 INSERT INTO public.domain_event_journal(id,tenant_id,event_type,schema_version,aggregate_type,aggregate_id,aggregate_revision,matter_id,source,correlation_id,causation_id,occurred_at)
 VALUES(o.id,o.tenant_id,o.event_type,'1.0',kind,o.aggregate_id,rev,matter,src,o.id::text,o.id::text,o.created_at)ON CONFLICT(id)DO NOTHING;
 INSERT INTO public.domain_event_deliveries(event_id,consumer_id) SELECT o.id,c FROM unnest(ARRAY['context-invalidation.v1','aggregate-heads.v1'])c ON CONFLICT DO NOTHING;
 UPDATE public.outbox_events SET publish_state='delivered',delivered_at=now(),lease_token=NULL,lease_expires_at=NULL,last_error_code=NULL WHERE id=o.id;
 RETURN true;
END $$;
CREATE FUNCTION claim_domain_deliveries(p_limit integer DEFAULT 20,p_seconds integer DEFAULT 30)
RETURNS TABLE(event_id uuid,consumer_id text,lease_token uuid)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$BEGIN
 IF p_limit IS NULL OR p_seconds IS NULL OR p_limit NOT BETWEEN 1 AND 100 OR p_seconds NOT BETWEEN 10 AND 300 THEN RAISE EXCEPTION 'invalid claim';END IF;
 RETURN QUERY WITH picked AS(SELECT d.event_id,d.consumer_id FROM public.domain_event_deliveries d WHERE (d.state='pending' AND d.available_at<=now())OR(d.state='leased' AND d.lease_expires_at<=now()) ORDER BY d.event_id FOR UPDATE SKIP LOCKED LIMIT p_limit),claimed AS(UPDATE public.domain_event_deliveries d SET state='leased',attempts=d.attempts+1,lease_token=gen_random_uuid(),lease_expires_at=now()+make_interval(secs=>p_seconds)FROM picked p WHERE d.event_id=p.event_id AND d.consumer_id=p.consumer_id RETURNING d.*)SELECT c.event_id,c.consumer_id,c.lease_token FROM claimed c;
END $$;
CREATE FUNCTION consume_domain_delivery(p_id uuid,p_consumer text,p_token uuid)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE e public.domain_event_journal; previous bigint; last_pos bigint; result text:='applied';
BEGIN
 PERFORM 1 FROM public.domain_event_deliveries WHERE event_id=p_id AND consumer_id=p_consumer AND lease_token=p_token AND state='leased' AND lease_expires_at>now() FOR UPDATE;
 IF NOT FOUND THEN RETURN false;END IF;
 SELECT * INTO STRICT e FROM public.domain_event_journal WHERE id=p_id;
 PERFORM pg_advisory_xact_lock(hashtextextended(e.tenant_id::text||p_consumer||e.aggregate_type||e.aggregate_id::text,0));
 IF EXISTS(SELECT 1 FROM public.domain_event_inbox WHERE event_id=e.id AND consumer_id=p_consumer)THEN result:='duplicate';
 ELSE
  SELECT h.revision,h.event_position INTO previous,last_pos FROM public.domain_aggregate_heads h WHERE h.tenant_id=e.tenant_id AND h.consumer_id=p_consumer AND h.aggregate_type=e.aggregate_type AND h.aggregate_id=e.aggregate_id;
  IF previous IS NOT NULL AND ((e.aggregate_revision>0 AND e.aggregate_revision<=previous)OR(e.aggregate_revision=0 AND e.position<=last_pos))THEN result:='stale';
  ELSE
   INSERT INTO public.domain_aggregate_heads VALUES(e.tenant_id,p_consumer,e.aggregate_type,e.aggregate_id,e.aggregate_revision,e.position)ON CONFLICT(tenant_id,consumer_id,aggregate_type,aggregate_id)DO UPDATE SET revision=greatest(domain_aggregate_heads.revision,excluded.revision),event_position=greatest(domain_aggregate_heads.event_position,excluded.event_position);
   IF p_consumer='context-invalidation.v1' AND e.matter_id IS NOT NULL THEN UPDATE public.context_packages SET invalidated=true WHERE tenant_id=e.tenant_id AND matter_id=e.matter_id AND created_at<=e.occurred_at;END IF;
  END IF;
  INSERT INTO public.domain_event_inbox(tenant_id,event_id,consumer_id,outcome)VALUES(e.tenant_id,e.id,p_consumer,result);
 END IF;
 UPDATE public.domain_event_deliveries SET state='delivered',lease_token=NULL,lease_expires_at=NULL,last_error_code=NULL WHERE event_id=e.id AND consumer_id=p_consumer;
 RETURN true;
END $$;
CREATE FUNCTION fail_domain_delivery(p_id uuid,p_consumer text,p_token uuid)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$DECLARE n integer;BEGIN
 UPDATE public.domain_event_deliveries SET state=CASE WHEN attempts>=10 THEN 'dead_letter' ELSE 'pending' END,available_at=now()+interval '10 seconds',last_error_code='CONSUMER_FAILED',lease_token=NULL,lease_expires_at=NULL WHERE event_id=p_id AND consumer_id=p_consumer AND lease_token=p_token AND state='leased' AND lease_expires_at>now();GET DIAGNOSTICS n=ROW_COUNT;RETURN n=1;
END $$;
REVOKE ALL ON FUNCTION claim_domain_events(text,integer,integer),publish_domain_event(uuid,uuid,text),claim_domain_deliveries(integer,integer),consume_domain_delivery(uuid,text,uuid),fail_domain_delivery(uuid,text,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_domain_events(text,integer,integer),publish_domain_event(uuid,uuid,text),claim_domain_deliveries(integer,integer),consume_domain_delivery(uuid,text,uuid),fail_domain_delivery(uuid,text,uuid) TO lawpilot_events;
COMMIT;

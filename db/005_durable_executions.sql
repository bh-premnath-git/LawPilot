-- LOCAL DEVELOPMENT roles; production credentials must be provisioned separately.
BEGIN;
DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='lawpilot_workflow') THEN
  CREATE ROLE lawpilot_workflow LOGIN PASSWORD 'local-dev-workflow-only' NOSUPERUSER NOBYPASSRLS;
 END IF;
 IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='lawpilot_dispatcher') THEN
  CREATE ROLE lawpilot_dispatcher LOGIN PASSWORD 'local-dev-dispatcher-only' NOSUPERUSER NOBYPASSRLS;
 END IF;
END $$;
ALTER TABLE ai_executions DROP CONSTRAINT ai_executions_status_check;
ALTER TABLE ai_executions ADD CONSTRAINT ai_executions_status_check CHECK(status IN
 ('accepted','queued','running','awaiting_review','completed','rejected','cancelled','failed'));
ALTER TABLE ai_executions ADD COLUMN request_input jsonb;
ALTER TABLE ai_executions ADD COLUMN workflow_id text UNIQUE;
ALTER TABLE ai_executions ADD COLUMN error_code text;
ALTER TABLE ai_executions ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE ai_executions ADD COLUMN activity_attempts integer NOT NULL DEFAULT 0;
-- Old synchronous requests have no retained input and cannot be safely reconstructed.
WITH legacy AS (
 UPDATE ai_executions SET status='failed',error_code='LEGACY_INPUT_UNAVAILABLE',completed_at=now(),updated_at=now()
 WHERE status='accepted' RETURNING id,tenant_id,principal_id
), audited AS (
 INSERT INTO audit_events(tenant_id,actor_id,action,resource_id,decision_code)
 SELECT tenant_id,principal_id,'ai.failed',id,'LEGACY_INPUT_UNAVAILABLE' FROM legacy
)
INSERT INTO outbox_events(tenant_id,event_type,event_version,aggregate_id,event_body)
SELECT tenant_id,'lawpilot.ai.failed.v1','1.0',id,
 jsonb_build_object('specversion','1.0','id',gen_random_uuid()::text,'source','migration-005','type','lawpilot.ai.failed.v1','time',now(),'tenantId',tenant_id::text,'schemaVersion','1.0','aggregateId',id::text,'data',jsonb_build_object('executionId',id::text,'state','failed','decisionCode','LEGACY_INPUT_UNAVAILABLE'))
FROM legacy;
ALTER TABLE outbox_events ADD COLUMN delivery_attempts integer NOT NULL DEFAULT 0;
ALTER TABLE outbox_events ADD COLUMN available_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE outbox_events ADD COLUMN lease_owner text;
ALTER TABLE outbox_events ADD COLUMN lease_token uuid;
ALTER TABLE outbox_events ADD COLUMN lease_expires_at timestamptz;
ALTER TABLE outbox_events ADD COLUMN delivered_at timestamptz;
ALTER TABLE outbox_events ADD COLUMN last_error_code text;
ALTER TABLE outbox_events ADD CONSTRAINT outbox_state_check CHECK(publish_state IN ('pending','leased','delivered','dead_letter'));
CREATE INDEX outbox_dispatch_due ON outbox_events(available_at,created_at)
 WHERE publish_state IN ('pending','leased');
CREATE UNIQUE INDEX execution_command_once ON outbox_events(aggregate_id,event_type)
 WHERE event_type IN ('lawpilot.ai.accepted.v1','lawpilot.ai.cancelled.v1');
GRANT USAGE ON SCHEMA public TO lawpilot_workflow,lawpilot_dispatcher;
GRANT SELECT ON memberships,principal_capabilities,matter_grants,ai_executions TO lawpilot_workflow;
GRANT UPDATE(status,result,error_code,updated_at,completed_at,activity_attempts) ON ai_executions TO lawpilot_workflow;
GRANT INSERT ON audit_events,outbox_events TO lawpilot_workflow;
GRANT UPDATE(publish_state,delivery_attempts,available_at,last_error_code,lease_owner,lease_token,lease_expires_at) ON outbox_events TO lawpilot_app;

-- Dispatcher sees command metadata only, never input/result text. Lease tokens fence
-- stale workers; SKIP LOCKED permits multiple dispatchers without duplicate claims.
CREATE FUNCTION claim_execution_commands(p_worker text,p_limit integer DEFAULT 20,p_lease_seconds integer DEFAULT 30)
RETURNS TABLE(event_id uuid,tenant_id uuid,execution_id uuid,event_type text,workflow_id text,lease_token uuid,attempt integer)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
BEGIN
 IF p_worker IS NULL OR length(trim(p_worker)) NOT BETWEEN 1 AND 200
 OR p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 100
 OR p_lease_seconds IS NULL OR p_lease_seconds NOT BETWEEN 10 AND 300 THEN
  RAISE EXCEPTION 'invalid claim parameters';
 END IF;
 RETURN QUERY
 WITH candidates AS (
  SELECT o.id FROM public.outbox_events o JOIN public.ai_executions e ON e.id=o.aggregate_id AND e.tenant_id=o.tenant_id
  WHERE e.workflow_id IS NOT NULL
    AND o.event_type IN ('lawpilot.ai.accepted.v1','lawpilot.ai.cancelled.v1')
    AND ((o.publish_state='pending' AND o.available_at<=now()) OR (o.publish_state='leased' AND o.lease_expires_at<=now()))
  ORDER BY o.created_at FOR UPDATE OF o SKIP LOCKED LIMIT p_limit
 ), claimed AS (
  UPDATE public.outbox_events o SET publish_state='leased',lease_owner=p_worker,lease_token=gen_random_uuid(),
   lease_expires_at=now()+make_interval(secs=>p_lease_seconds),delivery_attempts=o.delivery_attempts+1
  FROM candidates c WHERE o.id=c.id RETURNING o.*
 ), queued AS (
  UPDATE public.ai_executions e SET status='queued',updated_at=now()
  FROM claimed c WHERE c.aggregate_id=e.id AND c.event_type='lawpilot.ai.accepted.v1' AND e.status='accepted'
 )
 SELECT c.id,c.tenant_id,c.aggregate_id,c.event_type,e.workflow_id,c.lease_token,c.delivery_attempts
 FROM claimed c JOIN public.ai_executions e ON e.id=c.aggregate_id AND e.tenant_id=c.tenant_id;
END $$;
CREATE FUNCTION settle_execution_command(p_event uuid,p_token uuid,p_error text DEFAULT NULL)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE changed integer;
BEGIN
 UPDATE public.outbox_events SET
  publish_state=CASE WHEN p_error IS NULL THEN 'delivered' WHEN delivery_attempts>=10 THEN 'dead_letter' ELSE 'pending' END,
  delivered_at=CASE WHEN p_error IS NULL THEN now() ELSE NULL END,
  available_at=now()+make_interval(secs=>LEAST(300,power(2,LEAST(delivery_attempts,8))::integer)),
  last_error_code=CASE WHEN p_error IS NULL THEN NULL ELSE 'TEMPORAL_UNAVAILABLE' END,
  lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL
 WHERE id=p_event AND lease_token=p_token AND publish_state='leased' AND lease_expires_at>now();
 GET DIAGNOSTICS changed=ROW_COUNT;
 RETURN changed=1;
END $$;
CREATE FUNCTION stale_execution_commands(p_limit integer DEFAULT 100)
RETURNS TABLE(tenant_id uuid,execution_id uuid,workflow_id text)
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
 SELECT e.tenant_id,e.id,e.workflow_id FROM public.ai_executions e
 WHERE e.workflow_id IS NOT NULL AND e.status IN ('queued','running') AND e.updated_at<now()-interval '2 minutes'
 ORDER BY e.updated_at LIMIT LEAST(GREATEST(p_limit,1),100)
$$;
REVOKE ALL ON FUNCTION claim_execution_commands(text,integer,integer),settle_execution_command(uuid,uuid,text),stale_execution_commands(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_execution_commands(text,integer,integer),settle_execution_command(uuid,uuid,text),stale_execution_commands(integer) TO lawpilot_dispatcher;
COMMIT;

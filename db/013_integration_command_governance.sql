BEGIN;
CREATE OR REPLACE FUNCTION claim_domain_events(p_worker text,p_limit integer DEFAULT 20,p_seconds integer DEFAULT 30)
RETURNS TABLE(event_id uuid,tenant_id uuid,event_type text,event_version text,aggregate_id uuid,event_body jsonb,lease_token uuid,created_at timestamptz)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$ BEGIN
 IF length(coalesce(p_worker,'')) NOT BETWEEN 1 AND 200 OR p_limit IS NULL OR p_seconds IS NULL OR p_limit NOT BETWEEN 1 AND 100 OR p_seconds NOT BETWEEN 10 AND 300 THEN RAISE EXCEPTION 'invalid claim'; END IF;
 RETURN QUERY WITH picked AS(SELECT o.id FROM public.outbox_events o WHERE o.event_type NOT IN('lawpilot.ai.accepted.v1','lawpilot.ai.cancelled.v1','calendar.reminder.scheduled.v1','calendar.reminder.cancelled.v1','file.process.requested.v1','provider.sync.requested.v1','provider.update.requested.v1','legal.review.requested.v1') AND((o.publish_state='pending' AND o.available_at<=now())OR(o.publish_state='leased' AND o.lease_expires_at<=now())) ORDER BY o.created_at FOR UPDATE SKIP LOCKED LIMIT p_limit),claimed AS(UPDATE public.outbox_events o SET publish_state='leased',lease_owner=p_worker,lease_token=gen_random_uuid(),lease_expires_at=now()+make_interval(secs=>p_seconds),delivery_attempts=o.delivery_attempts+1 FROM picked p WHERE p.id=o.id RETURNING o.*)SELECT c.id,c.tenant_id,c.event_type,c.event_version,c.aggregate_id,c.event_body,c.lease_token,c.created_at FROM claimed c;
END $$;
-- The publisher sends validated metadata, never the original event JSON. Domain
-- identity, source, revisions and matter scope are rederived from authoritative rows.
CREATE OR REPLACE FUNCTION publish_domain_event(p_id uuid,p_token uuid,p_error text DEFAULT NULL)
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
 src:=CASE WHEN o.event_body->>'source'='integration-service' THEN 'integration-service' WHEN kind='document' THEN 'context-engine' WHEN kind='calendar' THEN 'calendar-service' ELSE 'platform' END;
 -- Serialize journal positions through commit so an SSE cursor cannot skip an
 -- earlier uncommitted position produced by a different publisher instance.
 PERFORM pg_advisory_xact_lock(604182702);
 INSERT INTO public.domain_event_journal(id,tenant_id,event_type,schema_version,aggregate_type,aggregate_id,aggregate_revision,matter_id,source,correlation_id,causation_id,occurred_at)
 VALUES(o.id,o.tenant_id,o.event_type,'1.0',kind,o.aggregate_id,rev,matter,src,o.id::text,o.id::text,o.created_at)ON CONFLICT(id)DO NOTHING;
 INSERT INTO public.domain_event_deliveries(event_id,consumer_id) SELECT o.id,c FROM unnest(ARRAY['context-invalidation.v1','aggregate-heads.v1'])c ON CONFLICT DO NOTHING;
 UPDATE public.outbox_events SET publish_state='delivered',delivered_at=now(),lease_token=NULL,lease_expires_at=NULL,last_error_code=NULL WHERE id=o.id;
 RETURN true;
END $$;
CREATE OR REPLACE FUNCTION claim_domain_deliveries(p_limit integer DEFAULT 20,p_seconds integer DEFAULT 30)
RETURNS TABLE(event_id uuid,consumer_id text,lease_token uuid)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$BEGIN
 IF p_limit IS NULL OR p_seconds IS NULL OR p_limit NOT BETWEEN 1 AND 100 OR p_seconds NOT BETWEEN 10 AND 300 THEN RAISE EXCEPTION 'invalid claim';END IF;
 RETURN QUERY WITH picked AS(SELECT d.event_id,d.consumer_id FROM public.domain_event_deliveries d WHERE (d.state='pending' AND d.available_at<=now())OR(d.state='leased' AND d.lease_expires_at<=now()) ORDER BY d.event_id FOR UPDATE SKIP LOCKED LIMIT p_limit),claimed AS(UPDATE public.domain_event_deliveries d SET state='leased',attempts=d.attempts+1,lease_token=gen_random_uuid(),lease_expires_at=now()+make_interval(secs=>p_seconds)FROM picked p WHERE d.event_id=p.event_id AND d.consumer_id=p.consumer_id RETURNING d.*)SELECT c.event_id,c.consumer_id,c.lease_token FROM claimed c;
END $$;

CREATE UNIQUE INDEX integration_command_once ON outbox_events(tenant_id,aggregate_id,event_type) WHERE event_type IN('file.process.requested.v1','provider.sync.requested.v1','provider.update.requested.v1','legal.review.requested.v1');
INSERT INTO domain_event_types VALUES('lawpilot.ai.review_decided.v1','1.0');
CREATE OR REPLACE FUNCTION domain_event_failures(p_tenant uuid)
RETURNS TABLE(event_id uuid,consumer_id text,event_type text,error_code text,attempts integer)
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
 SELECT o.id,'publisher'::text,CASE WHEN EXISTS(SELECT 1 FROM public.domain_event_types r WHERE r.event_type=o.event_type)THEN o.event_type ELSE 'unregistered' END,o.last_error_code,o.delivery_attempts FROM public.outbox_events o WHERE o.tenant_id=p_tenant AND o.publish_state='dead_letter' AND o.event_type NOT IN('lawpilot.ai.accepted.v1','lawpilot.ai.cancelled.v1','calendar.reminder.scheduled.v1','calendar.reminder.cancelled.v1','file.process.requested.v1','provider.sync.requested.v1','provider.update.requested.v1','legal.review.requested.v1')
 UNION ALL SELECT j.id,d.consumer_id,j.event_type,d.last_error_code,d.attempts FROM public.domain_event_deliveries d JOIN public.domain_event_journal j ON j.id=d.event_id WHERE j.tenant_id=p_tenant AND d.state='dead_letter';
$$;
CREATE OR REPLACE FUNCTION replay_domain_event(p_id uuid,p_consumer text DEFAULT 'publisher')
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$DECLARE n integer;BEGIN
 IF p_consumer='publisher' THEN UPDATE public.outbox_events SET publish_state='pending',delivery_attempts=0,available_at=now(),last_error_code=NULL WHERE id=p_id AND publish_state='dead_letter' AND event_type NOT IN('lawpilot.ai.accepted.v1','lawpilot.ai.cancelled.v1','calendar.reminder.scheduled.v1','calendar.reminder.cancelled.v1','file.process.requested.v1','provider.sync.requested.v1','provider.update.requested.v1','legal.review.requested.v1');
 ELSE UPDATE public.domain_event_deliveries SET state='pending',attempts=0,available_at=now(),last_error_code=NULL WHERE event_id=p_id AND consumer_id=p_consumer AND state='dead_letter';END IF;
 GET DIAGNOSTICS n=ROW_COUNT;RETURN n=1;
END $$;

COMMIT;

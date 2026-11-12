BEGIN;
-- Exact-quote-only grounded results. This function verifies inside the caller's
-- result-save/read transaction, preventing a source-update race after an RPC.
CREATE FUNCTION verify_execution_result(p_tenant uuid,p_execution uuid,p_result jsonb)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE e public.ai_executions; claim jsonb; c jsonb; a public.document_assets; v public.document_versions; p public.document_passages; n integer; seen text[]:='{}';
BEGIN
 IF current_setting('app.tenant_id',true) IS DISTINCT FROM p_tenant::text THEN RETURN false;END IF;
 SELECT * INTO e FROM public.ai_executions WHERE tenant_id=p_tenant AND id=p_execution FOR SHARE;
 IF NOT FOUND OR e.matter_id IS NULL OR e.status NOT IN('running','completed','awaiting_review') THEN RETURN false;END IF;
 PERFORM 1 FROM public.memberships m JOIN public.principal_capabilities c ON c.tenant_id=m.tenant_id AND c.principal_id=m.principal_id AND c.capability_id=e.capability_id JOIN public.principal_capabilities x ON x.tenant_id=m.tenant_id AND x.principal_id=m.principal_id AND x.capability_id='context.read' JOIN public.matter_grants g ON g.tenant_id=m.tenant_id AND g.principal_id=m.principal_id AND g.matter_id=e.matter_id AND g.permission='matter.read' WHERE m.tenant_id=p_tenant AND m.principal_id=e.principal_id AND m.status='active' FOR SHARE OF m,c,x,g;
 IF NOT FOUND THEN RETURN false;END IF;
 IF p_result->>'executionId' IS DISTINCT FROM p_execution::text OR p_result->>'status' IS DISTINCT FROM 'completed' OR p_result->'output'->>'kind' IS DISTINCT FROM 'extractive_quotes.v1' OR jsonb_typeof(p_result->'output'->'claims') IS DISTINCT FROM 'array' OR jsonb_typeof(p_result->'evidence') IS DISTINCT FROM 'array' THEN RETURN false;END IF;
 IF EXISTS(SELECT 1 FROM jsonb_object_keys(p_result->'output')k WHERE k NOT IN('kind','claims'))THEN RETURN false;END IF;
 n:=jsonb_array_length(p_result->'output'->'claims');IF n NOT BETWEEN 1 AND 16 OR jsonb_array_length(p_result->'evidence')<>n THEN RETURN false;END IF;
 FOR claim IN SELECT value FROM jsonb_array_elements(p_result->'output'->'claims') LOOP
  IF jsonb_typeof(claim) IS DISTINCT FROM 'object' OR EXISTS(SELECT 1 FROM jsonb_object_keys(claim)k WHERE k NOT IN('quote','citation')) OR jsonb_typeof(claim->'quote') IS DISTINCT FROM 'string' OR octet_length(coalesce(claim->>'quote','')) NOT BETWEEN 1 AND 4096 THEN RETURN false;END IF;
  c:=claim->'citation';IF jsonb_typeof(c) IS DISTINCT FROM 'object' THEN RETURN false;END IF;
  IF EXISTS(SELECT 1 FROM jsonb_object_keys(c)k WHERE k NOT IN('documentId','versionId','passageId','contentSha256','byteStart','byteEnd')) OR coalesce(c->>'contentSha256','')!~'^[a-f0-9]{64}$' OR (c->>'passageId')=ANY(seen) THEN RETURN false;END IF;
  seen:=array_append(seen,c->>'passageId');
  SELECT * INTO a FROM public.document_assets WHERE tenant_id=p_tenant AND matter_id=e.matter_id AND id=(c->>'documentId')::uuid AND deleted_at IS NULL FOR SHARE;
  IF NOT FOUND OR a.current_version_id IS DISTINCT FROM (c->>'versionId')::uuid THEN RETURN false;END IF;
  SELECT * INTO v FROM public.document_versions WHERE tenant_id=p_tenant AND document_id=a.id AND id=a.current_version_id;
  IF NOT FOUND OR encode(public.digest(convert_to(v.full_text,'UTF8'),'sha256'),'hex')<>v.content_sha256 THEN RETURN false;END IF;
  SELECT * INTO p FROM public.document_passages WHERE tenant_id=p_tenant AND document_id=a.id AND version_id=v.id AND id=(c->>'passageId')::uuid;
  IF NOT FOUND OR p.passage_sha256 IS DISTINCT FROM c->>'contentSha256' OR encode(public.digest(convert_to(p.body,'UTF8'),'sha256'),'hex')<>p.passage_sha256 OR p.byte_start IS DISTINCT FROM (c->>'byteStart')::integer OR p.byte_end IS DISTINCT FROM (c->>'byteEnd')::integer OR substring(convert_to(v.full_text,'UTF8') FROM p.byte_start+1 FOR p.byte_end-p.byte_start)<>convert_to(p.body,'UTF8') OR strpos(p.body,claim->>'quote')=0 THEN RETURN false;END IF;
  IF NOT EXISTS(SELECT 1 FROM jsonb_array_elements(p_result->'evidence')r WHERE r->>'documentId'=a.id::text AND r->>'documentVersionId'=v.id::text AND r->>'passageId'=p.id::text AND r->>'contentSha256'=p.passage_sha256 AND ((NOT r ? 'pageNumber' AND p.page_number IS NULL) OR r->>'pageNumber'=p.page_number::text))THEN RETURN false;END IF;
 END LOOP;
 RETURN true;
EXCEPTION WHEN invalid_text_representation OR numeric_value_out_of_range THEN RETURN false;
END $$;
REVOKE ALL ON FUNCTION verify_execution_result(uuid,uuid,jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION verify_execution_result(uuid,uuid,jsonb) TO lawpilot_workflow,lawpilot_app,lawpilot_context;
-- Metadata-only operations. Replay remains an administrator action, never a
-- public/user RPC. Bad schemas must be corrected before replaying the original.
CREATE FUNCTION domain_event_failures(p_tenant uuid)
RETURNS TABLE(event_id uuid,consumer_id text,event_type text,error_code text,attempts integer)
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
 SELECT o.id,'publisher'::text,CASE WHEN EXISTS(SELECT 1 FROM public.domain_event_types r WHERE r.event_type=o.event_type)THEN o.event_type ELSE 'unregistered' END,o.last_error_code,o.delivery_attempts FROM public.outbox_events o WHERE o.tenant_id=p_tenant AND o.publish_state='dead_letter' AND o.event_type NOT IN('lawpilot.ai.accepted.v1','lawpilot.ai.cancelled.v1','calendar.reminder.scheduled.v1','calendar.reminder.cancelled.v1')
 UNION ALL SELECT j.id,d.consumer_id,j.event_type,d.last_error_code,d.attempts FROM public.domain_event_deliveries d JOIN public.domain_event_journal j ON j.id=d.event_id WHERE j.tenant_id=p_tenant AND d.state='dead_letter';
$$;
CREATE FUNCTION replay_domain_event(p_id uuid,p_consumer text DEFAULT 'publisher')
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$DECLARE n integer;BEGIN
 IF p_consumer='publisher' THEN UPDATE public.outbox_events SET publish_state='pending',delivery_attempts=0,available_at=now(),last_error_code=NULL WHERE id=p_id AND publish_state='dead_letter' AND event_type NOT IN('lawpilot.ai.accepted.v1','lawpilot.ai.cancelled.v1','calendar.reminder.scheduled.v1','calendar.reminder.cancelled.v1');
 ELSE UPDATE public.domain_event_deliveries SET state='pending',attempts=0,available_at=now(),last_error_code=NULL WHERE event_id=p_id AND consumer_id=p_consumer AND state='dead_letter';END IF;
 GET DIAGNOSTICS n=ROW_COUNT;RETURN n=1;
END $$;
REVOKE ALL ON FUNCTION domain_event_failures(uuid),replay_domain_event(uuid,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION domain_event_failures(uuid) TO lawpilot_events;
COMMIT;

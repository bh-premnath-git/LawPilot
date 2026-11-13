BEGIN;
ALTER TABLE ai_executions ADD CONSTRAINT ai_execution_tenant_key UNIQUE(tenant_id,id);
CREATE TABLE legal_template_versions(id uuid PRIMARY KEY,tenant_id uuid NOT NULL REFERENCES tenants(id),name text NOT NULL,version integer NOT NULL CHECK(version>0),body text NOT NULL CHECK(octet_length(body) BETWEEN 1 AND 16384),body_sha256 text NOT NULL,created_by text NOT NULL,approved_by text,state text NOT NULL DEFAULT 'pending' CHECK(state IN('pending','approved','revoked')),created_at timestamptz NOT NULL DEFAULT now(),UNIQUE(tenant_id,id),UNIQUE(tenant_id,name,version));
CREATE TABLE execution_context_grants(id uuid PRIMARY KEY,tenant_id uuid NOT NULL,execution_id uuid NOT NULL,package_id uuid NOT NULL REFERENCES context_packages(id),snapshot_sha256 text NOT NULL,input_sha256 text NOT NULL,template_id uuid,template_sha256 text,generation_expires_at timestamptz NOT NULL,review_expires_at timestamptz NOT NULL,created_at timestamptz NOT NULL DEFAULT now(),UNIQUE(tenant_id,id),FOREIGN KEY(tenant_id,template_id)REFERENCES legal_template_versions(tenant_id,id),FOREIGN KEY(tenant_id,execution_id)REFERENCES ai_executions(tenant_id,id));
CREATE TABLE legal_review_decisions(tenant_id uuid NOT NULL,execution_id uuid NOT NULL,reviewed_by text NOT NULL,artifact_sha256 text NOT NULL,decision text NOT NULL CHECK(decision IN('approve','reject')),decided_at timestamptz NOT NULL DEFAULT now(),PRIMARY KEY(tenant_id,execution_id),FOREIGN KEY(tenant_id,execution_id)REFERENCES ai_executions(tenant_id,id));
DO $$ DECLARE t text;BEGIN FOREACH t IN ARRAY ARRAY['legal_template_versions','execution_context_grants','legal_review_decisions'] LOOP EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY',t);EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY',t);EXECUTE format('CREATE POLICY tenant_guard ON %I USING(tenant_id::text=current_setting(''app.tenant_id'',true)) WITH CHECK(tenant_id::text=current_setting(''app.tenant_id'',true))',t);END LOOP;END $$;
GRANT SELECT,INSERT ON legal_template_versions,execution_context_grants TO lawpilot_context;
GRANT UPDATE(state,approved_by) ON legal_template_versions TO lawpilot_context;
GRANT SELECT ON legal_review_decisions TO lawpilot_context;
GRANT INSERT,SELECT(id) ON integration_jobs TO lawpilot_workflow;
CREATE FUNCTION verify_ai_execution_result(p_tenant uuid,p_execution uuid,p_result jsonb)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE e public.ai_executions;g public.execution_context_grants;pkg public.context_packages;t public.legal_template_versions;review public.legal_review_decisions;claim jsonb;normalized jsonb;
BEGIN
 IF p_result->'output'->>'kind' IS DISTINCT FROM 'legal_draft.v1' THEN RETURN public.verify_execution_result(p_tenant,p_execution,p_result);END IF;
 IF current_setting('app.tenant_id',true) IS DISTINCT FROM p_tenant::text THEN RETURN false;END IF;
 SELECT * INTO e FROM public.ai_executions WHERE tenant_id=p_tenant AND id=p_execution FOR SHARE;
 IF NOT FOUND OR e.status NOT IN('running','awaiting_review','completed') OR p_result->>'requiresAttorneyReview' IS DISTINCT FROM 'true' THEN RETURN false;END IF;
 SELECT * INTO g FROM public.execution_context_grants WHERE tenant_id=p_tenant AND execution_id=p_execution AND id=(p_result->'output'->>'grantId')::uuid;
 IF NOT FOUND OR g.input_sha256<>encode(public.digest(convert_to(e.request_input::jsonb::text,'UTF8'),'sha256'),'hex') THEN RETURN false;END IF;
 IF e.status='running' AND g.generation_expires_at<=now() OR e.status='awaiting_review' AND g.review_expires_at<=now() THEN RETURN false;END IF;
 SELECT * INTO pkg FROM public.context_packages WHERE tenant_id=p_tenant AND id=g.package_id AND principal_id=e.principal_id AND matter_id=e.matter_id;
 IF NOT FOUND OR pkg.invalidated OR pkg.snapshot_sha256<>g.snapshot_sha256 OR pkg.body->>'snapshotSha256' IS DISTINCT FROM g.snapshot_sha256 THEN RETURN false;END IF;
 IF jsonb_typeof(p_result->'output'->'draft') IS DISTINCT FROM 'string' OR octet_length(p_result->'output'->>'draft') NOT BETWEEN 1 AND 24576 OR jsonb_typeof(p_result->'output'->'claims') IS DISTINCT FROM 'array' OR EXISTS(SELECT 1 FROM jsonb_object_keys(p_result->'output')k WHERE k NOT IN('kind','draft','claims','limitations','grantId','modelProfile','templateId','templateSha256')) THEN RETURN false;END IF;
 IF e.capability_id='legal.draft.v1' THEN
  SELECT * INTO t FROM public.legal_template_versions WHERE tenant_id=p_tenant AND id=g.template_id AND state='approved' FOR SHARE;
  IF NOT FOUND OR t.body_sha256 IS DISTINCT FROM g.template_sha256 OR t.body_sha256<>encode(public.digest(convert_to(t.body,'UTF8'),'sha256'),'hex') OR p_result->'output'->>'templateId' IS DISTINCT FROM t.id::text OR p_result->'output'->>'templateSha256' IS DISTINCT FROM t.body_sha256 THEN RETURN false;END IF;
  IF NOT EXISTS(SELECT 1 FROM public.memberships m JOIN public.principal_capabilities c ON c.tenant_id=m.tenant_id AND c.principal_id=m.principal_id WHERE m.tenant_id=p_tenant AND m.principal_id=t.approved_by AND m.status='active' AND c.capability_id='template.approve') THEN RETURN false;END IF;
 END IF;
 FOR claim IN SELECT value FROM jsonb_array_elements(p_result->'output'->'claims') LOOP
  IF NOT EXISTS(SELECT 1 FROM jsonb_array_elements(pkg.body->'sources')s WHERE s->'citation'->>'documentId'=claim->'citation'->>'documentId' AND s->'citation'->>'versionId'=claim->'citation'->>'versionId' AND s->'citation'->>'passageId'=claim->'citation'->>'passageId' AND s->'citation'->>'contentSha256'=claim->'citation'->>'contentSha256') THEN RETURN false;END IF;
 END LOOP;
 IF e.status='completed' THEN
  SELECT * INTO review FROM public.legal_review_decisions WHERE tenant_id=p_tenant AND execution_id=p_execution AND decision='approve';
  IF NOT FOUND OR review.artifact_sha256<>encode(public.digest(convert_to(p_result::text,'UTF8'),'sha256'),'hex') OR NOT EXISTS(SELECT 1 FROM public.memberships m JOIN public.principal_capabilities c ON c.tenant_id=m.tenant_id AND c.principal_id=m.principal_id JOIN public.matter_grants a ON a.tenant_id=m.tenant_id AND a.principal_id=m.principal_id WHERE m.tenant_id=p_tenant AND m.principal_id=review.reviewed_by AND m.status='active' AND c.capability_id='draft.approve' AND a.matter_id=e.matter_id AND a.permission='matter.read') THEN RETURN false;END IF;
 END IF;
 normalized:=jsonb_set(p_result,'{output}',jsonb_build_object('kind','extractive_quotes.v1','claims',p_result->'output'->'claims'));
 RETURN public.verify_execution_result(p_tenant,p_execution,normalized);
EXCEPTION WHEN invalid_text_representation OR numeric_value_out_of_range THEN RETURN false;
END $$;
CREATE FUNCTION decide_legal_review(p_tenant uuid,p_execution uuid,p_actor text,p_hash text,p_decision text)
RETURNS text LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE e public.ai_executions;g public.execution_context_grants;actual text;previous public.legal_review_decisions;next_state text;
BEGIN
 IF current_setting('app.tenant_id',true) IS DISTINCT FROM p_tenant::text THEN RAISE EXCEPTION 'tenant required';END IF;
 SELECT * INTO STRICT e FROM public.ai_executions WHERE tenant_id=p_tenant AND id=p_execution FOR UPDATE;
 IF p_actor=e.principal_id OR p_decision NOT IN('approve','reject') OR NOT EXISTS(SELECT 1 FROM public.memberships m JOIN public.principal_capabilities c ON c.tenant_id=m.tenant_id AND c.principal_id=m.principal_id JOIN public.matter_grants a ON a.tenant_id=m.tenant_id AND a.principal_id=m.principal_id WHERE m.tenant_id=p_tenant AND m.principal_id=p_actor AND m.status='active' AND c.capability_id='draft.approve' AND a.matter_id=e.matter_id AND a.permission='matter.read') THEN RETURN 'denied';END IF;
 actual:=encode(public.digest(convert_to(e.result::text,'UTF8'),'sha256'),'hex');IF actual IS DISTINCT FROM p_hash THEN RETURN 'artifact_changed';END IF;
 SELECT * INTO previous FROM public.legal_review_decisions WHERE tenant_id=p_tenant AND execution_id=p_execution;
 IF FOUND THEN IF previous.reviewed_by=p_actor AND previous.artifact_sha256=p_hash AND previous.decision=p_decision THEN RETURN CASE WHEN p_decision='approve' THEN 'completed' ELSE 'rejected' END;END IF;RETURN 'already_decided';END IF;
 IF e.status<>'awaiting_review' OR e.result->'output'->>'kind' IS DISTINCT FROM 'legal_draft.v1' THEN RETURN 'not_pending';END IF;
 SELECT * INTO g FROM public.execution_context_grants WHERE tenant_id=p_tenant AND execution_id=p_execution AND id=(e.result->'output'->>'grantId')::uuid;
 IF NOT FOUND OR g.review_expires_at<=now() THEN RETURN 'expired';END IF;
 IF p_decision='approve' AND NOT public.verify_ai_execution_result(p_tenant,p_execution,e.result) THEN RETURN 'source_stale';END IF;
 INSERT INTO public.legal_review_decisions VALUES(p_tenant,p_execution,p_actor,p_hash,p_decision,now());next_state:=CASE WHEN p_decision='approve' THEN 'completed' ELSE 'rejected' END;
 UPDATE public.ai_executions SET status=next_state,completed_at=now(),updated_at=now(),error_code=CASE WHEN next_state='rejected' THEN 'LEGAL_REVIEW_REJECTED' ELSE NULL END WHERE tenant_id=p_tenant AND id=p_execution;
 UPDATE public.integration_jobs SET state=CASE WHEN next_state='completed' THEN 'completed' ELSE 'rejected' END,updated_at=now() WHERE tenant_id=p_tenant AND id=p_execution AND kind='legal_review';
 INSERT INTO public.audit_events(tenant_id,actor_id,action,resource_id,decision_code)VALUES(p_tenant,p_actor,'legal.review.decided',p_execution,next_state);
 INSERT INTO public.outbox_events(tenant_id,event_type,event_version,aggregate_id,event_body)VALUES(p_tenant,'lawpilot.ai.review_decided.v1','1.0',p_execution,jsonb_build_object('specversion','1.0','id',gen_random_uuid()::text,'source','platform-api','type','lawpilot.ai.review_decided.v1','time',now(),'tenantId',p_tenant::text,'schemaVersion','1.0','aggregateId',p_execution::text,'data',jsonb_build_object('executionId',p_execution::text,'state',next_state,'decisionCode','attorney_review')));
 RETURN next_state;
END $$;
REVOKE ALL ON FUNCTION verify_ai_execution_result(uuid,uuid,jsonb),decide_legal_review(uuid,uuid,text,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION verify_ai_execution_result(uuid,uuid,jsonb) TO lawpilot_workflow,lawpilot_app,lawpilot_context;
GRANT EXECUTE ON FUNCTION decide_legal_review(uuid,uuid,text,text,text) TO lawpilot_context;
GRANT UPDATE(status,error_code,completed_at,updated_at) ON ai_executions TO lawpilot_context;
GRANT INSERT,UPDATE(state,error_code,updated_at) ON integration_jobs TO lawpilot_context;
COMMIT;

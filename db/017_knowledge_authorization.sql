BEGIN;
-- Existing evidence verification remains authoritative; library access is an
-- additional permission, including when results are read after grant revocation.
ALTER FUNCTION verify_execution_result(uuid,uuid,jsonb) RENAME TO verify_execution_result_v015;
CREATE FUNCTION verify_execution_result(p_tenant uuid,p_execution uuid,p_result jsonb)RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE e public.ai_executions;BEGIN
 IF current_setting('app.tenant_id',true)IS DISTINCT FROM p_tenant::text THEN RETURN false;END IF;
 SELECT * INTO e FROM public.ai_executions WHERE tenant_id=p_tenant AND id=p_execution;
 IF EXISTS(SELECT 1 FROM public.knowledge_libraries WHERE tenant_id=p_tenant AND matter_id=e.matter_id)AND NOT EXISTS(SELECT 1 FROM public.principal_capabilities WHERE tenant_id=p_tenant AND principal_id=e.principal_id AND capability_id='knowledge.read')THEN RETURN false;END IF;
 RETURN public.verify_execution_result_v015(p_tenant,p_execution,p_result);
END$$;
ALTER FUNCTION decide_legal_review(uuid,uuid,text,text,text)RENAME TO decide_legal_review_v015;
CREATE FUNCTION decide_legal_review(p_tenant uuid,p_execution uuid,p_actor text,p_hash text,p_decision text)RETURNS text LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$DECLARE matter uuid;BEGIN
 IF current_setting('app.tenant_id',true)IS DISTINCT FROM p_tenant::text THEN RETURN 'denied';END IF;
 SELECT matter_id INTO matter FROM public.ai_executions WHERE tenant_id=p_tenant AND id=p_execution;
 IF NOT EXISTS(SELECT 1 FROM public.principal_capabilities WHERE tenant_id=p_tenant AND principal_id=p_actor AND capability_id='context.read')THEN RETURN 'denied';END IF;
 IF EXISTS(SELECT 1 FROM public.knowledge_libraries WHERE tenant_id=p_tenant AND matter_id=matter)AND NOT EXISTS(SELECT 1 FROM public.principal_capabilities WHERE tenant_id=p_tenant AND principal_id=p_actor AND capability_id='knowledge.read')THEN RETURN 'denied';END IF;
 RETURN public.decide_legal_review_v015(p_tenant,p_execution,p_actor,p_hash,p_decision);
END$$;
REVOKE ALL ON FUNCTION verify_execution_result_v015(uuid,uuid,jsonb) FROM PUBLIC,lawpilot_workflow,lawpilot_app,lawpilot_context;
REVOKE ALL ON FUNCTION decide_legal_review_v015(uuid,uuid,text,text,text) FROM PUBLIC,lawpilot_context;
REVOKE ALL ON FUNCTION verify_execution_result(uuid,uuid,jsonb),decide_legal_review(uuid,uuid,text,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION verify_execution_result(uuid,uuid,jsonb)TO lawpilot_workflow,lawpilot_app,lawpilot_context;
GRANT EXECUTE ON FUNCTION decide_legal_review(uuid,uuid,text,text,text)TO lawpilot_context;
COMMIT;

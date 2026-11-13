BEGIN;
ALTER FUNCTION verify_ai_execution_result(uuid,uuid,jsonb)RENAME TO verify_ai_execution_result_v015;
CREATE FUNCTION verify_ai_execution_result(p_tenant uuid,p_execution uuid,p_result jsonb)RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$DECLARE e public.ai_executions;reviewer text;BEGIN
 IF current_setting('app.tenant_id',true)IS DISTINCT FROM p_tenant::text THEN RETURN false;END IF;
 SELECT * INTO e FROM public.ai_executions WHERE tenant_id=p_tenant AND id=p_execution;
 IF e.status='completed' AND p_result->'output'->>'kind'='legal_draft.v1' THEN
 SELECT reviewed_by INTO reviewer FROM public.legal_review_decisions WHERE tenant_id=p_tenant AND execution_id=p_execution AND decision='approve';
 IF NOT EXISTS(SELECT 1 FROM public.principal_capabilities WHERE tenant_id=p_tenant AND principal_id=reviewer AND capability_id='context.read')THEN RETURN false;END IF;
 IF EXISTS(SELECT 1 FROM public.knowledge_libraries WHERE tenant_id=p_tenant AND matter_id=e.matter_id)AND NOT EXISTS(SELECT 1 FROM public.principal_capabilities WHERE tenant_id=p_tenant AND principal_id=reviewer AND capability_id='knowledge.read')THEN RETURN false;END IF;
 END IF;
 RETURN public.verify_ai_execution_result_v015(p_tenant,p_execution,p_result);
END$$;
REVOKE ALL ON FUNCTION verify_ai_execution_result_v015(uuid,uuid,jsonb)FROM PUBLIC,lawpilot_workflow,lawpilot_context,lawpilot_app;
REVOKE ALL ON FUNCTION verify_ai_execution_result(uuid,uuid,jsonb)FROM PUBLIC;
GRANT EXECUTE ON FUNCTION verify_ai_execution_result(uuid,uuid,jsonb)TO lawpilot_workflow,lawpilot_context,lawpilot_app;
CREATE OR REPLACE FUNCTION system_queue_metrics()RETURNS TABLE(metric text,value bigint)LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
 SELECT 'outbox_'||publish_state,count(*) FROM public.outbox_events GROUP BY publish_state UNION ALL SELECT 'consumer_'||state,count(*) FROM public.domain_event_deliveries GROUP BY state UNION ALL SELECT 'integration_'||state,count(*) FROM public.integration_jobs GROUP BY state UNION ALL SELECT 'ai_'||status,count(*) FROM public.ai_executions GROUP BY status UNION ALL SELECT 'webhook_'||state,count(*) FROM public.provider_webhooks GROUP BY state UNION ALL SELECT 'sync_dirty',count(*) FROM public.provider_sync_signals WHERE generation>consumed;
$$;
COMMIT;

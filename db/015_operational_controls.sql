BEGIN;
ALTER TABLE file_ingestions ADD COLUMN purge_lease_token uuid;
ALTER TABLE file_ingestions ADD COLUMN purge_lease_expires_at timestamptz;
ALTER TABLE file_ingestions ADD COLUMN purged_at timestamptz;
CREATE FUNCTION claim_expired_uploads(p_limit integer DEFAULT 20)
RETURNS TABLE(tenant_id uuid,file_id uuid,object_key text,lease_token uuid)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$BEGIN
 IF p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 100 THEN RAISE EXCEPTION 'invalid cleanup limit';END IF;
 RETURN QUERY WITH picked AS(SELECT f.id FROM public.file_ingestions f WHERE f.state IN('awaiting_upload','expired','completed','rejected') AND f.expires_at<now() AND f.purged_at IS NULL AND(f.purge_lease_expires_at IS NULL OR f.purge_lease_expires_at<now()) ORDER BY f.expires_at FOR UPDATE SKIP LOCKED LIMIT p_limit),updated AS(UPDATE public.file_ingestions f SET state=CASE WHEN f.state='awaiting_upload' THEN 'expired' ELSE f.state END,purge_lease_token=gen_random_uuid(),purge_lease_expires_at=now()+interval '1 minute' FROM picked p WHERE p.id=f.id RETURNING f.*)SELECT u.tenant_id,u.id,u.quarantine_key,u.purge_lease_token FROM updated u;
END $$;
CREATE FUNCTION settle_expired_upload(p_id uuid,p_token uuid,p_ok boolean)
RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$DECLARE n integer;BEGIN
 UPDATE public.file_ingestions SET purged_at=CASE WHEN p_ok THEN now() ELSE NULL END,purge_lease_token=NULL,purge_lease_expires_at=CASE WHEN p_ok THEN NULL ELSE now()+interval '5 minutes' END WHERE id=p_id AND purge_lease_token=p_token AND purge_lease_expires_at>now();GET DIAGNOSTICS n=ROW_COUNT;RETURN n=1;
END $$;
CREATE FUNCTION system_queue_metrics()RETURNS TABLE(metric text,value bigint)
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
 SELECT 'outbox_'||publish_state,count(*) FROM public.outbox_events GROUP BY publish_state UNION ALL SELECT 'consumer_'||state,count(*) FROM public.domain_event_deliveries GROUP BY state UNION ALL SELECT 'integration_'||state,count(*) FROM public.integration_jobs GROUP BY state UNION ALL SELECT 'ai_'||status,count(*) FROM public.ai_executions GROUP BY status;
$$;
REVOKE ALL ON FUNCTION claim_expired_uploads(integer),settle_expired_upload(uuid,uuid,boolean),system_queue_metrics() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_expired_uploads(integer),settle_expired_upload(uuid,uuid,boolean) TO lawpilot_integration;
GRANT EXECUTE ON FUNCTION system_queue_metrics() TO lawpilot_integration,lawpilot_context,lawpilot_events,lawpilot_workflow,lawpilot_dispatcher;
COMMIT;

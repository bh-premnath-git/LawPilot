-- Run with psql -v ON_ERROR_STOP=1 as the database migration/admin role.
-- All synthetic fixtures are rolled back.
BEGIN;
INSERT INTO tenants(id,name) VALUES
 ('99999999-1111-4111-8111-111111111111','RLS test A'),
 ('99999999-2222-4222-8222-222222222222','RLS test B');
INSERT INTO memberships(tenant_id,principal_id,status) VALUES
 ('99999999-1111-4111-8111-111111111111','rls-test-user','active'),
 ('99999999-2222-4222-8222-222222222222','rls-test-user','active');
SET LOCAL ROLE lawpilot_app;
SELECT set_config('app.tenant_id','',true);
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM tenants) OR EXISTS(SELECT 1 FROM memberships) THEN
  RAISE EXCEPTION 'Missing tenant context exposed records';
 END IF;
 PERFORM set_config('app.tenant_id','99999999-1111-4111-8111-111111111111',true);
 IF (SELECT count(*) FROM tenants) <> 1 OR (SELECT count(*) FROM memberships) <> 1 THEN
  RAISE EXCEPTION 'Tenant isolation failed';
 END IF;
 BEGIN
  INSERT INTO outbox_events(tenant_id,event_type,event_version,event_body)
   VALUES('99999999-2222-4222-8222-222222222222','test.v1','1.0','{}');
  RAISE EXCEPTION 'Cross-tenant outbox write succeeded';
 EXCEPTION WHEN insufficient_privilege THEN NULL;
 END;
END $$;
ROLLBACK;

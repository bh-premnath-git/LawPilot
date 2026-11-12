-- Apply once to existing databases as the migration/admin role.
-- Fresh installs already include this policy in 001_init.sql.
BEGIN;
ALTER TABLE tenants ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenants FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_directory_guard ON tenants;
CREATE POLICY tenant_directory_guard ON tenants
 USING (id::text = current_setting('app.tenant_id',true))
 WITH CHECK (id::text = current_setting('app.tenant_id',true));
COMMIT;

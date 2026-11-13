-- Optional LOCAL example, must be run by postgres migration/admin role after replacing principal UUID.
-- psql -U postgres -d lawpilot -f scripts/seed_local_tenant.sql
-- This creates synthetic records ONLY. It does not create Keycloak users.
INSERT INTO tenants(id,name) VALUES('11111111-1111-4111-8111-111111111111','Synthetic Test Firm') ON CONFLICT DO NOTHING;
INSERT INTO memberships(tenant_id,principal_id,status)
VALUES('11111111-1111-4111-8111-111111111111','REPLACE_WITH_ACTUAL_KEYCLOAK_SUB','active') ON CONFLICT DO NOTHING;
INSERT INTO principal_capabilities(tenant_id,principal_id,capability_id)
SELECT '11111111-1111-4111-8111-111111111111','REPLACE_WITH_ACTUAL_KEYCLOAK_SUB',cap
FROM unnest(ARRAY['intake.summarize.v1','document.summarize.v1','legal.draft.v1','matter.qa.v1','calendar.manage','calendar.read','calendar.write','calendar.approve','calendar.availability','context.read','document.ingest','document.delete','document.download','integration.manage','integration.read','provider.manage','provider.read','provider.sync','provider.write','provider.approve','template.manage','template.approve','draft.approve','knowledge.manage','knowledge.read']) AS cap
ON CONFLICT DO NOTHING;
INSERT INTO matters(id,tenant_id,title,status,resource_owner) VALUES
('22222222-2222-4222-8222-222222222222','11111111-1111-4111-8111-111111111111','Synthetic Matter Only','active','mock') ON CONFLICT DO NOTHING;
INSERT INTO matter_grants(tenant_id,matter_id,principal_id,permission) VALUES
('11111111-1111-4111-8111-111111111111','22222222-2222-4222-8222-222222222222','REPLACE_WITH_ACTUAL_KEYCLOAK_SUB','matter.read') ON CONFLICT DO NOTHING;

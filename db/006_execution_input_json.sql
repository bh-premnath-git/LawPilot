-- Preserve future accepted JSON representations instead of JSONB reformatting.
-- Existing JSONB values retain their current representation on conversion.
BEGIN;
ALTER TABLE ai_executions ALTER COLUMN request_input TYPE json USING request_input::json;
COMMIT;

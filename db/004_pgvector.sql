-- Requires a server image with pgvector 0.8.7 installed; run as migration/admin role.
BEGIN;
CREATE EXTENSION IF NOT EXISTS vector;
ALTER EXTENSION vector UPDATE TO '0.8.7';
COMMIT;

-- Verify the supported major/minor baseline, extension version and vector operations.
-- All fixtures are temporary and rolled back; no embedding model is assumed.
BEGIN;
DO $$ BEGIN
 IF current_setting('server_version_num')::integer < 180006
    OR current_setting('server_version_num')::integer >= 190000 THEN
  RAISE EXCEPTION 'Expected PostgreSQL 18.6 or newer PostgreSQL 18 minor release';
 END IF;
 IF (SELECT extversion FROM pg_extension WHERE extname='vector') IS DISTINCT FROM '0.8.7' THEN
  RAISE EXCEPTION 'Expected pgvector 0.8.7';
 END IF;
END $$;
CREATE TEMP TABLE vector_smoke(id integer PRIMARY KEY, embedding vector(3));
INSERT INTO vector_smoke VALUES (1,'[1,0,0]'),(2,'[0,1,0]');
CREATE INDEX ON vector_smoke USING hnsw (embedding vector_cosine_ops);
DO $$ BEGIN
 IF (SELECT id FROM vector_smoke ORDER BY embedding <=> '[1,0,0]' LIMIT 1) <> 1 THEN
  RAISE EXCEPTION 'Vector similarity query returned the wrong result';
 END IF;
 IF ('[1,2,3]'::vector <-> '[1,2,3]'::vector) <> 0 THEN
  RAISE EXCEPTION 'Vector distance check failed';
 END IF;
END $$;
ROLLBACK;

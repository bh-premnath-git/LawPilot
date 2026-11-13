#!/usr/bin/env bash
set -euo pipefail
# Fresh docker-entrypoint bootstrap only. SQL files already completed successfully.
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<'SQL'
CREATE TABLE schema_migrations(name text PRIMARY KEY,sha256 text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now());
REVOKE ALL ON schema_migrations FROM PUBLIC;
SQL
for migration in /docker-entrypoint-initdb.d/[0-9][0-9][0-9]_*.sql; do
  name="$(basename "$migration")"
  checksum="$(sha256sum "$migration" | cut -d ' ' -f 1)"
  psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" -v name="$name" -v checksum="$checksum" <<'SQL'
INSERT INTO schema_migrations(name,sha256)VALUES(:'name',:'checksum');
SQL
done

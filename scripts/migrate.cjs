// Run as a dedicated migration owner, never with a runtime database identity.
const fs = require("node:fs"),
  path = require("node:path"),
  { createHash } = require("node:crypto"),
  { Client } = require("pg");
(async () => {
  const value = process.env.MIGRATION_DATABASE_URL_FILE
    ? fs.readFileSync(process.env.MIGRATION_DATABASE_URL_FILE, "utf8").trim()
    : process.env.MIGRATION_DATABASE_URL;
  if (!value) throw Error("MIGRATION_DATABASE_URL(_FILE) required");
  const client = new Client({ connectionString: value });
  await client.connect();
  try {
    await client.query("SELECT pg_advisory_lock(604182703)");
    await client.query(
      "CREATE TABLE IF NOT EXISTS public.schema_migrations(name text PRIMARY KEY,sha256 text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now())",
    );
    await client.query("REVOKE ALL ON public.schema_migrations FROM PUBLIC");
    const files = fs
      .readdirSync("db")
      .filter((x) => /^\d{3}_.*\.sql$/.test(x))
      .sort();
    const baseline = process.argv
      .find((x) => x.startsWith("--baseline-through="))
      ?.split("=")[1];
    if (baseline && !/^\d{3}$/.test(baseline)) throw Error("invalid baseline");
    for (const name of files) {
      const raw = fs.readFileSync(path.join("db", name), "utf8"),
        hash = createHash("sha256").update(raw).digest("hex");
      const previous = await client.query(
        "SELECT sha256 FROM public.schema_migrations WHERE name=$1",
        [name],
      );
      if (previous.rowCount) {
        if (previous.rows[0].sha256 !== hash)
          throw Error("Migration checksum mismatch: " + name);
        continue;
      }
      await client.query("BEGIN");
      try {
        // Explicit baseline records operator-verified existing migrations without rerunning them.
        if (!baseline || name.slice(0, 3) > baseline) {
          const sql = raw
            .replace(/^\s*BEGIN;\s*$/im, "")
            .replace(/^\s*COMMIT;\s*$/im, "");
          await client.query(sql);
        }
        await client.query(
          "INSERT INTO public.schema_migrations(name,sha256)VALUES($1,$2)",
          [name, hash],
        );
        await client.query("COMMIT");
        console.log("Recorded " + name);
      } catch (error) {
        await client.query("ROLLBACK");
        throw error;
      }
    }
  } finally {
    await client.end();
  }
})().catch(() => {
  console.error(
    "Migration failed; verify schema/checksums with the migration owner. No credentials or SQL values logged.",
  );
  process.exitCode = 1;
});

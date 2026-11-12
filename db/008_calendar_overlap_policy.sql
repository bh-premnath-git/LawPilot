BEGIN;
-- Preserve existing calendars' single-resource behavior; new calendars default shared.
ALTER TABLE calendars ADD COLUMN overlap_policy text NOT NULL DEFAULT 'exclusive' CHECK(overlap_policy IN ('shared','exclusive'));
ALTER TABLE calendars ALTER COLUMN overlap_policy SET DEFAULT 'shared';
ALTER TABLE calendars ADD CONSTRAINT calendar_policy_key UNIQUE(tenant_id,id,overlap_policy);
ALTER TABLE calendar_entries ADD COLUMN overlap_policy text NOT NULL DEFAULT 'shared' CHECK(overlap_policy IN ('shared','exclusive'));
UPDATE calendar_entries e SET overlap_policy=c.overlap_policy FROM calendars c WHERE c.tenant_id=e.tenant_id AND c.id=e.calendar_id;
-- A write cannot opt out of an exclusive calendar's constraint by changing its entry flag.
ALTER TABLE calendar_entries ADD CONSTRAINT calendar_entry_policy FOREIGN KEY(tenant_id,calendar_id,overlap_policy) REFERENCES calendars(tenant_id,id,overlap_policy) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE calendar_entries DROP CONSTRAINT calendar_no_overlap;
ALTER TABLE calendar_entries ADD CONSTRAINT calendar_no_overlap EXCLUDE USING gist(tenant_id WITH =,calendar_id WITH =,tstzrange(start_at,end_at,'[)') WITH &&) WHERE(status='active' AND kind<>5 AND overlap_policy='exclusive') DEFERRABLE INITIALLY DEFERRED;
COMMIT;

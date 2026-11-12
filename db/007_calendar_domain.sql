BEGIN;
CREATE EXTENSION IF NOT EXISTS btree_gist;
DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='lawpilot_calendar') THEN
  CREATE ROLE lawpilot_calendar LOGIN PASSWORD 'local-dev-calendar-only' NOSUPERUSER NOBYPASSRLS;
 END IF;
END $$;
CREATE TABLE calendars(
 id uuid PRIMARY KEY,tenant_id uuid NOT NULL REFERENCES tenants(id),name text NOT NULL,
 default_time_zone text NOT NULL,approval_required boolean NOT NULL DEFAULT true,
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),created_by text NOT NULL,authority text NOT NULL DEFAULT 'lawpilot-native' CHECK(authority='lawpilot-native'),
 UNIQUE(tenant_id,id)
);
CREATE TABLE calendar_grants(
 tenant_id uuid NOT NULL,calendar_id uuid NOT NULL,principal_id text NOT NULL,
 permission text NOT NULL CHECK(permission IN ('read','write','approve','manage')),
 PRIMARY KEY(tenant_id,calendar_id,principal_id,permission),
 FOREIGN KEY(tenant_id,calendar_id) REFERENCES calendars(tenant_id,id),
 FOREIGN KEY(tenant_id,principal_id) REFERENCES memberships(tenant_id,principal_id)
);
CREATE TABLE calendar_series(
 id uuid PRIMARY KEY,tenant_id uuid NOT NULL,calendar_id uuid NOT NULL,revision bigint NOT NULL DEFAULT 1,
 frequency integer NOT NULL CHECK(frequency IN (1,2)),interval_count integer NOT NULL CHECK(interval_count BETWEEN 1 AND 52),occurrence_count integer NOT NULL CHECK(occurrence_count BETWEEN 1 AND 366),
 FOREIGN KEY(tenant_id,calendar_id) REFERENCES calendars(tenant_id,id),UNIQUE(tenant_id,id)
);
CREATE TABLE calendar_entries(
 id uuid PRIMARY KEY,tenant_id uuid NOT NULL,calendar_id uuid NOT NULL,series_id uuid,occurrence_index integer NOT NULL DEFAULT 0,
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),title text NOT NULL,description text NOT NULL DEFAULT '',
 kind integer NOT NULL CHECK(kind BETWEEN 1 AND 5),visibility integer NOT NULL CHECK(visibility BETWEEN 1 AND 3),matter_id uuid,
 start_at timestamptz NOT NULL,end_at timestamptz NOT NULL,time_zone text NOT NULL,all_day boolean NOT NULL,
 start_date date,end_date date,status text NOT NULL DEFAULT 'active' CHECK(status IN ('active','cancelled')),
 participants text[] NOT NULL DEFAULT '{}',created_by text NOT NULL,updated_at timestamptz NOT NULL DEFAULT now(),
 CHECK(end_at>start_at),CHECK((all_day AND start_date IS NOT NULL AND end_date>start_date) OR (NOT all_day AND start_date IS NULL AND end_date IS NULL)),
 FOREIGN KEY(tenant_id,calendar_id) REFERENCES calendars(tenant_id,id),FOREIGN KEY(tenant_id,series_id) REFERENCES calendar_series(tenant_id,id),FOREIGN KEY(tenant_id,matter_id) REFERENCES matters(tenant_id,id),UNIQUE(tenant_id,id),UNIQUE(series_id,occurrence_index)
);
ALTER TABLE calendar_entries ADD CONSTRAINT calendar_no_overlap EXCLUDE USING gist(tenant_id WITH =,calendar_id WITH =,tstzrange(start_at,end_at,'[)') WITH &&) WHERE(status='active' AND kind<>5) DEFERRABLE INITIALLY DEFERRED;
CREATE INDEX calendar_entry_range ON calendar_entries(tenant_id,calendar_id,start_at,end_at) WHERE status='active';
CREATE INDEX calendar_entry_matter ON calendar_entries(tenant_id,matter_id);
CREATE TABLE calendar_proposals(
 id uuid PRIMARY KEY,tenant_id uuid NOT NULL,calendar_id uuid NOT NULL,command bytea NOT NULL,sha256 text NOT NULL CHECK(length(sha256)=64),
 affected_count integer NOT NULL,affected_ids uuid[] NOT NULL DEFAULT '{}',proposed_by text NOT NULL,status text NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','approved','rejected')),
 reviewed_by text,reviewed_at timestamptz,created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(tenant_id,calendar_id) REFERENCES calendars(tenant_id,id),UNIQUE(tenant_id,id)
);
CREATE TABLE calendar_commands(
 tenant_id uuid NOT NULL,principal_id text NOT NULL,idempotency_key text NOT NULL,request_sha256 text NOT NULL,response bytea NOT NULL,created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,principal_id,idempotency_key)
);
CREATE TABLE calendar_reminders(
 id uuid PRIMARY KEY,tenant_id uuid NOT NULL,calendar_id uuid NOT NULL,entry_id uuid NOT NULL,entry_revision bigint NOT NULL,
 requestor_id text NOT NULL,due_at timestamptz NOT NULL,workflow_id text NOT NULL UNIQUE,
 status text NOT NULL DEFAULT 'scheduled' CHECK(status IN ('scheduled','due','cancelled')),reason text,updated_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(tenant_id,calendar_id) REFERENCES calendars(tenant_id,id),FOREIGN KEY(tenant_id,entry_id) REFERENCES calendar_entries(tenant_id,id),UNIQUE(tenant_id,id)
);
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['calendars','calendar_grants','calendar_series','calendar_entries','calendar_proposals','calendar_commands','calendar_reminders'] LOOP
  EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY tenant_guard ON %I USING(tenant_id::text=current_setting(''app.tenant_id'',true)) WITH CHECK(tenant_id::text=current_setting(''app.tenant_id'',true))',t);
 END LOOP;
END $$;
GRANT USAGE ON SCHEMA public TO lawpilot_calendar;
GRANT SELECT ON memberships,principal_capabilities,matter_grants,calendars,calendar_grants,calendar_series,calendar_entries,calendar_proposals,calendar_commands,calendar_reminders TO lawpilot_calendar;
GRANT INSERT ON calendars,calendar_grants,calendar_series,calendar_entries,calendar_proposals,calendar_commands,calendar_reminders,audit_events,outbox_events TO lawpilot_calendar;
GRANT UPDATE ON calendars,calendar_grants,calendar_series,calendar_entries,calendar_reminders TO lawpilot_calendar;
GRANT DELETE ON calendar_grants TO lawpilot_calendar;
GRANT UPDATE(status,reviewed_by,reviewed_at) ON calendar_proposals TO lawpilot_calendar;
CREATE FUNCTION claim_calendar_reminder_commands(p_worker text,p_limit integer DEFAULT 1,p_lease_seconds integer DEFAULT 30)
RETURNS TABLE(event_id uuid,tenant_id uuid,reminder_id uuid,event_type text,workflow_id text,lease_token uuid)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
BEGIN
 IF p_worker IS NULL OR length(trim(p_worker)) NOT BETWEEN 1 AND 200 OR p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 100 OR p_lease_seconds IS NULL OR p_lease_seconds NOT BETWEEN 10 AND 300 THEN RAISE EXCEPTION 'invalid claim parameters'; END IF;
 RETURN QUERY WITH candidates AS (
  SELECT o.id FROM public.outbox_events o JOIN public.calendar_reminders r ON r.id=o.aggregate_id AND r.tenant_id=o.tenant_id
  WHERE o.event_type IN ('calendar.reminder.scheduled.v1','calendar.reminder.cancelled.v1')
   AND ((o.publish_state='pending' AND o.available_at<=now()) OR(o.publish_state='leased' AND o.lease_expires_at<=now()))
  ORDER BY o.created_at FOR UPDATE OF o SKIP LOCKED LIMIT p_limit
 ),claimed AS(
  UPDATE public.outbox_events o SET publish_state='leased',lease_owner=p_worker,lease_token=gen_random_uuid(),lease_expires_at=now()+make_interval(secs=>p_lease_seconds),delivery_attempts=o.delivery_attempts+1
  FROM candidates c WHERE o.id=c.id RETURNING o.*
 ) SELECT c.id,c.tenant_id,c.aggregate_id,c.event_type,r.workflow_id,c.lease_token FROM claimed c JOIN public.calendar_reminders r ON r.id=c.aggregate_id AND r.tenant_id=c.tenant_id;
END $$;
REVOKE ALL ON FUNCTION claim_calendar_reminder_commands(text,integer,integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_calendar_reminder_commands(text,integer,integer) TO lawpilot_dispatcher;
COMMIT;

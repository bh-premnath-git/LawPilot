package calendar

import (
	"context"
	"errors"
	"strings"
	"time"

	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/calendar/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const columns = `e.id::text,e.calendar_id::text,COALESCE(e.series_id::text,''),e.occurrence_index,e.revision,COALESCE(s.revision,0),e.title,e.description,e.kind,e.visibility,COALESCE(e.matter_id::text,''),e.start_at,e.end_at,e.time_zone,e.all_day,e.start_date,e.end_date,e.status,e.participants,e.created_by,COALESCE(s.frequency,0),COALESCE(s.interval_count,0),COALESCE(s.occurrence_count,0)`
const join = ` FROM calendar_entries e LEFT JOIN calendar_series s ON s.tenant_id=e.tenant_id AND s.id=e.series_id `

func scan(row pgx.Row) (*pb.CalendarEntry, interval, error) {
	value := new(pb.CalendarEntry)
	var when interval
	var kind, visibility, freq, step, count int32
	err := row.Scan(&value.Id, &value.CalendarId, &value.SeriesId, &value.OccurrenceIndex, &value.Revision, &value.SeriesRevision, &value.Title, &value.Description, &kind, &visibility, &value.MatterId, &when.Start, &when.End, &when.Zone, &when.AllDay, &when.StartDate, &when.EndDate, &value.Status, &value.ParticipantPrincipalIds, &value.CreatedBy, &freq, &step, &count)
	if err != nil {
		return nil, when, err
	}
	value.Kind = pb.EntryKind(kind)
	value.Visibility = pb.Visibility(visibility)
	if freq != 0 {
		value.Recurrence = &pb.RecurrenceRule{Frequency: pb.Frequency(freq), Interval: step, Count: count}
	}
	if when.AllDay {
		value.Schedule = &pb.CalendarEntry_AllDay{AllDay: &pb.AllDayInterval{StartDate: civil(*when.StartDate), EndDateExclusive: civil(*when.EndDate), TimeZone: when.Zone}}
	} else {
		value.Schedule = &pb.CalendarEntry_Timed{Timed: &pb.TimedInterval{Start: timestamppb.New(when.Start), End: timestamppb.New(when.End), TimeZone: when.Zone}}
	}
	return value, when, nil
}
func loadEntry(ctx context.Context, tx pgx.Tx, tenant, calendar, id string) (*pb.CalendarEntry, interval, error) {
	if !identifier(id) {
		return nil, interval{}, bad("INVALID_ENTRY_ID")
	}
	e, when, err := scan(tx.QueryRow(ctx, "SELECT "+columns+join+"WHERE e.tenant_id=$1 AND e.calendar_id=$2 AND e.id=$3", tenant, calendar, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, when, status.Error(codes.NotFound, "NOT_FOUND")
	}
	return e, when, err
}
func visible(ctx context.Context, tx pgx.Tx, tenant, who string, e *pb.CalendarEntry, client bool) bool {
	if e.Visibility == pb.Visibility_VISIBILITY_PRIVATE && e.CreatedBy != who {
		return false
	}
	if client {
		if e.Visibility != pb.Visibility_VISIBILITY_SHARED || (e.Kind != pb.EntryKind_ENTRY_KIND_APPOINTMENT && e.Kind != pb.EntryKind_ENTRY_KIND_CONSULTATION) {
			return false
		}
		found := false
		for _, id := range e.ParticipantPrincipalIds {
			if id == who {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return matterAccess(ctx, tx, tenant, who, e.MatterId) == nil
}
func redact(e *pb.CalendarEntry, who string, client bool) {
	if client {
		e.Description = ""
		e.CreatedBy = ""
		e.ParticipantPrincipalIds = []string{who}
	}
}
func validateDraft(ctx context.Context, tx pgx.Tx, tenant, who string, draft *pb.EntryDraft) error {
	if draft == nil || len(strings.TrimSpace(draft.Title)) == 0 || len(draft.Title) > 256 || len(draft.Description) > 4096 {
		return bad("INVALID_ENTRY")
	}
	if draft.Kind == pb.EntryKind_ENTRY_KIND_VERIFIED_DEADLINE {
		return status.Error(codes.Unimplemented, "DEADLINE_VERIFICATION_NOT_AVAILABLE")
	}
	if draft.Kind < 1 || draft.Kind > 5 || draft.Visibility < 1 || draft.Visibility > 3 || len(draft.ParticipantPrincipalIds) > 50 || len(draft.Reminders) > 3 {
		return bad("INVALID_ENTRY")
	}
	if err := matterAccess(ctx, tx, tenant, who, draft.MatterId); err != nil {
		return err
	}
	if _, err := schedule(draft, 0); err != nil {
		return err
	}
	if _, _, err := recurrence(draft.Recurrence); err != nil {
		return err
	}
	if draft.ParticipantPrincipalIds == nil {
		draft.ParticipantPrincipalIds = []string{}
	}
	seen := map[string]bool{}
	for _, id := range draft.ParticipantPrincipalIds {
		if seen[id] {
			return bad("DUPLICATE_PARTICIPANT")
		}
		seen[id] = true
		if err := member(ctx, tx, tenant, id); err != nil {
			return status.Error(codes.PermissionDenied, "PARTICIPANT_OUTSIDE_TENANT")
		}
	}
	offsets := map[int32]bool{}
	for _, r := range draft.Reminders {
		if r.MinutesBefore < 0 || r.MinutesBefore > 40320 || offsets[r.MinutesBefore] {
			return bad("INVALID_REMINDER")
		}
		offsets[r.MinutesBefore] = true
	}
	return nil
}
func conflict(ctx context.Context, tx pgx.Tx, tenant, calendar string, when interval, kind pb.EntryKind, excluded []string) error {
	if kind == pb.EntryKind_ENTRY_KIND_DEADLINE_CANDIDATE {
		return nil
	}
	var exists bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM calendar_entries WHERE tenant_id=$1 AND calendar_id=$2 AND overlap_policy='exclusive' AND status='active' AND kind<>5 AND start_at<$4 AND end_at>$3 AND NOT(id::text=ANY($5::text[])))`, tenant, calendar, when.Start, when.End, excluded).Scan(&exists)
	if err != nil {
		return err
	}
	if exists {
		return status.Error(codes.AlreadyExists, "CALENDAR_CONFLICT")
	}
	return nil
}
func cancelReminders(ctx context.Context, tx pgx.Tx, tenant, entry, who string) error {
	rows, err := tx.Query(ctx, "UPDATE calendar_reminders SET status='cancelled',reason='ENTRY_CHANGED',updated_at=now() WHERE tenant_id=$1 AND entry_id=$2 AND status IN ('scheduled','due') RETURNING id::text,entry_revision", tenant, entry)
	if err != nil {
		return err
	}
	type record struct {
		id       string
		revision int64
	}
	records := []record{}
	for rows.Next() {
		var r record
		if err = rows.Scan(&r.id, &r.revision); err != nil {
			rows.Close()
			return err
		}
		records = append(records, r)
	}
	rows.Close()
	if rows.Err() != nil {
		return rows.Err()
	}
	for _, r := range records {
		if err = event(ctx, tx, tenant, who, r.id, "calendar.reminder.cancelled.v1", r.revision, "entry_changed"); err != nil {
			return err
		}
	}
	return nil
}
func (s *Service) reminders(ctx context.Context, tx pgx.Tx, tenant, calendar, id, who string, revision int64, when interval, specs []*pb.ReminderSpec) error {
	for _, spec := range specs {
		due := when.Start.Add(-time.Duration(spec.MinutesBefore) * time.Minute)
		if !due.After(s.now()) {
			return bad("REMINDER_MUST_BE_FUTURE")
		}
		reminder := uuid.NewString()
		workflow := "lawpilot:" + tenant + ":calendar-reminder:" + reminder
		_, err := tx.Exec(ctx, "INSERT INTO calendar_reminders(id,tenant_id,calendar_id,entry_id,entry_revision,requestor_id,due_at,workflow_id)VALUES($1,$2,$3,$4,$5,$6,$7,$8)", reminder, tenant, calendar, id, revision, who, due, workflow)
		if err != nil {
			return err
		}
		if err = event(ctx, tx, tenant, who, reminder, "calendar.reminder.scheduled.v1", revision, "scheduled"); err != nil {
			return err
		}
	}
	return nil
}
func (s *Service) command(ctx context.Context, tx pgx.Tx, tenant, calendar, originator string, command *pb.CalendarCommand, dry bool) (*pb.MutationResult, error) {
	if command == nil {
		return nil, bad("COMMAND_REQUIRED")
	}
	calendarInfo, err := calendarRecord(ctx, tx, tenant, calendar)
	if err != nil {
		return nil, err
	}
	exclusive := calendarInfo.OverlapPolicy == "exclusive"
	result := &pb.MutationResult{Status: "applied"}
	if create := command.GetCreate(); create != nil {
		draft := create.Draft
		if err := validateDraft(ctx, tx, tenant, originator, draft); err != nil {
			return nil, err
		}
		count, step, err := recurrence(draft.Recurrence)
		if err != nil {
			return nil, err
		}
		series := ""
		if draft.Recurrence != nil {
			series = uuid.NewString()
			if !dry {
				if _, err = tx.Exec(ctx, "INSERT INTO calendar_series(id,tenant_id,calendar_id,frequency,interval_count,occurrence_count)VALUES($1,$2,$3,$4,$5,$6)", series, tenant, calendar, draft.Recurrence.Frequency, draft.Recurrence.Interval, draft.Recurrence.Count); err != nil {
					return nil, err
				}
			}
			result.SeriesId = series
			result.SeriesRevision = 1
		}
		ranges := []interval{}
		for i := 0; i < count; i++ {
			when, err := schedule(draft, i*step)
			if err != nil {
				return nil, err
			}
			if err = conflict(ctx, tx, tenant, calendar, when, draft.Kind, []string{}); err != nil {
				return nil, err
			}
			if exclusive && draft.Kind != pb.EntryKind_ENTRY_KIND_DEADLINE_CANDIDATE {
				for _, previous := range ranges {
					if when.Start.Before(previous.End) && when.End.After(previous.Start) {
						return nil, status.Error(codes.AlreadyExists, "RECURRENCE_CONFLICT")
					}
				}
			}
			ranges = append(ranges, when)
			id := uuid.NewString()
			result.EntryIds = append(result.EntryIds, id)
			result.Revisions = append(result.Revisions, 1)
			if dry {
				continue
			}
			_, err = tx.Exec(ctx, `INSERT INTO calendar_entries(id,tenant_id,calendar_id,series_id,occurrence_index,title,description,kind,visibility,matter_id,start_at,end_at,time_zone,all_day,start_date,end_date,participants,created_by,overlap_policy)
    VALUES($1,$2,$3,NULLIF($4,'')::uuid,$5,$6,$7,$8,$9,NULLIF($10,'')::uuid,$11,$12,$13,$14,$15,$16,$17,$18,$19)`, id, tenant, calendar, series, i, draft.Title, draft.Description, draft.Kind, draft.Visibility, draft.MatterId, when.Start, when.End, when.Zone, when.AllDay, when.StartDate, when.EndDate, draft.ParticipantPrincipalIds, originator, calendarInfo.OverlapPolicy)
			if err != nil {
				return nil, err
			}
			if err = s.reminders(ctx, tx, tenant, calendar, id, originator, 1, when, draft.Reminders); err != nil {
				return nil, err
			}
			if err = event(ctx, tx, tenant, originator, id, "calendar.entry.created.v1", 1, "native_only"); err != nil {
				return nil, err
			}
		}
		return result, nil
	}
	var id string
	var expected, seriesExpected int64
	var change pb.ChangeScope
	var draft *pb.EntryDraft
	cancel := false
	if update := command.GetUpdate(); update != nil {
		id = update.EntryId
		expected = update.ExpectedRevision
		seriesExpected = update.ExpectedSeriesRevision
		change = update.Scope
		draft = update.Draft
		if err := validateDraft(ctx, tx, tenant, originator, draft); err != nil {
			return nil, err
		}
		if draft.Recurrence != nil {
			return nil, status.Error(codes.Unimplemented, "RECURRENCE_RULE_CHANGE_UNSUPPORTED")
		}
	} else if removal := command.GetCancel(); removal != nil {
		id = removal.EntryId
		expected = removal.ExpectedRevision
		seriesExpected = removal.ExpectedSeriesRevision
		change = removal.Scope
		cancel = true
	} else {
		return nil, bad("COMMAND_REQUIRED")
	}
	entry, _, err := loadEntry(ctx, tx, tenant, calendar, id)
	if err != nil {
		return nil, err
	}
	if !visible(ctx, tx, tenant, originator, entry, false) {
		return nil, status.Error(codes.NotFound, "NOT_FOUND")
	}
	if entry.Status != "active" || expected < 1 || expected != entry.Revision {
		return nil, status.Error(codes.Aborted, "STALE_REVISION")
	}
	if change < 1 || change > 3 {
		return nil, bad("CHANGE_SCOPE_REQUIRED")
	}
	targets := []*pb.CalendarEntry{entry}
	step := 0
	if change != pb.ChangeScope_CHANGE_SCOPE_OCCURRENCE {
		if entry.SeriesId == "" {
			return nil, status.Error(codes.Unimplemented, "NOT_A_SERIES")
		}
		if seriesExpected < 1 || seriesExpected != entry.SeriesRevision {
			return nil, status.Error(codes.Aborted, "STALE_SERIES_REVISION")
		}
		var frequency, count int
		err = tx.QueryRow(ctx, "SELECT frequency,interval_count FROM calendar_series WHERE tenant_id=$1 AND id=$2", tenant, entry.SeriesId).Scan(&frequency, &count)
		if err != nil {
			return nil, err
		}
		step = count
		if frequency == 2 {
			step *= 7
		}
		rows, err := tx.Query(ctx, "SELECT "+columns+join+"WHERE e.tenant_id=$1 AND e.series_id=$2 AND e.status='active' AND ($3=3 OR e.occurrence_index>=$4) ORDER BY e.occurrence_index", tenant, entry.SeriesId, int32(change), entry.OccurrenceIndex)
		if err != nil {
			return nil, err
		}
		targets = nil
		for rows.Next() {
			value, _, err := scan(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			targets = append(targets, value)
		}
		rows.Close()
		if rows.Err() != nil {
			return nil, rows.Err()
		}
	}
	ids := []string{}
	for _, e := range targets {
		if !visible(ctx, tx, tenant, originator, e, false) {
			return nil, status.Error(codes.PermissionDenied, "SCOPE_ACCESS_DENIED")
		}
		ids = append(ids, e.Id)
	}
	ranges := []interval{}
	for _, e := range targets {
		var when interval
		if !cancel {
			when, err = schedule(draft, (int(e.OccurrenceIndex)-int(entry.OccurrenceIndex))*step)
			if err != nil {
				return nil, err
			}
			if err = conflict(ctx, tx, tenant, calendar, when, draft.Kind, ids); err != nil {
				return nil, err
			}
			if exclusive && draft.Kind != pb.EntryKind_ENTRY_KIND_DEADLINE_CANDIDATE {
				for _, r := range ranges {
					if when.Start.Before(r.End) && when.End.After(r.Start) {
						return nil, status.Error(codes.AlreadyExists, "RECURRENCE_CONFLICT")
					}
				}
			}
			ranges = append(ranges, when)
		}
		result.EntryIds = append(result.EntryIds, e.Id)
		result.Revisions = append(result.Revisions, e.Revision+1)
		if dry {
			continue
		}
		if err = cancelReminders(ctx, tx, tenant, e.Id, originator); err != nil {
			return nil, err
		}
		if cancel {
			_, err = tx.Exec(ctx, "UPDATE calendar_entries SET status='cancelled',revision=revision+1,updated_at=now() WHERE tenant_id=$1 AND id=$2", tenant, e.Id)
		} else {
			_, err = tx.Exec(ctx, `UPDATE calendar_entries SET title=$3,description=$4,kind=$5,visibility=$6,matter_id=NULLIF($7,'')::uuid,start_at=$8,end_at=$9,time_zone=$10,all_day=$11,start_date=$12,end_date=$13,participants=$14,revision=revision+1,updated_at=now() WHERE tenant_id=$1 AND id=$2`, tenant, e.Id, draft.Title, draft.Description, draft.Kind, draft.Visibility, draft.MatterId, when.Start, when.End, when.Zone, when.AllDay, when.StartDate, when.EndDate, draft.ParticipantPrincipalIds)
			if err == nil {
				err = s.reminders(ctx, tx, tenant, calendar, e.Id, originator, e.Revision+1, when, draft.Reminders)
			}
		}
		if err != nil {
			return nil, err
		}
		kind := "calendar.entry.updated.v1"
		if cancel {
			kind = "calendar.entry.cancelled.v1"
		}
		if err = event(ctx, tx, tenant, originator, e.Id, kind, e.Revision+1, "native_only"); err != nil {
			return nil, err
		}
	}
	if entry.SeriesId != "" {
		result.SeriesId = entry.SeriesId
		result.SeriesRevision = entry.SeriesRevision + 1
		if !dry {
			_, err = tx.Exec(ctx, "UPDATE calendar_series SET revision=revision+1 WHERE tenant_id=$1 AND id=$2", tenant, entry.SeriesId)
			if err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}

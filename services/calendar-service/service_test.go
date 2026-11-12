package calendar

import (
	"context"
	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/calendar/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type fixture struct {
	s                                  *Service
	admin                              *pgxpool.Pool
	tenant, alice, bob, client, matter string
	calendar                           *pb.Calendar
}

func fixtureFor(t *testing.T, approval bool) *fixture {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not configured")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, os.Getenv("TEST_CALENDAR_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{s: &Service{Pool: pool, Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }}, admin: admin, tenant: uuid.NewString(), alice: uuid.NewString(), bob: uuid.NewString(), client: uuid.NewString(), matter: uuid.NewString()}
	t.Cleanup(func() {
		for _, table := range []string{"calendar_commands", "calendar_reminders", "calendar_proposals", "calendar_entries", "calendar_series", "calendar_grants", "calendars", "outbox_events", "audit_events", "matter_grants", "matters", "principal_capabilities", "memberships"} {
			if _, err := admin.Exec(ctx, "DELETE FROM "+table+" WHERE tenant_id=$1", f.tenant); err != nil {
				t.Error(err)
			}
		}
		admin.Exec(ctx, "DELETE FROM tenants WHERE id=$1", f.tenant)
		pool.Close()
		admin.Close()
	})
	if _, err = admin.Exec(ctx, "INSERT INTO tenants(id,name)VALUES($1,'synthetic calendar')", f.tenant); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{f.alice, f.bob, f.client} {
		if _, err = admin.Exec(ctx, "INSERT INTO memberships(tenant_id,principal_id,status)VALUES($1,$2,'active')", f.tenant, who); err != nil {
			t.Fatal(err)
		}
		caps := []string{"calendar.read", "calendar.write", "calendar.approve", "calendar.availability"}
		if who == f.alice {
			caps = append(caps, "calendar.manage")
		}
		if who == f.client {
			caps = []string{"calendar.client_read"}
		}
		for _, cap := range caps {
			if _, err = admin.Exec(ctx, "INSERT INTO principal_capabilities(tenant_id,principal_id,capability_id)VALUES($1,$2,$3)", f.tenant, who, cap); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err = admin.Exec(ctx, "INSERT INTO matters(id,tenant_id,title,resource_owner)VALUES($1,$2,'synthetic matter','mock')", f.matter, f.tenant); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{f.alice, f.bob} {
		admin.Exec(ctx, "INSERT INTO matter_grants(tenant_id,matter_id,principal_id,permission)VALUES($1,$2,$3,'matter.read')", f.tenant, f.matter, who)
	}
	created, err := f.s.CreateCalendar(f.ctx(f.alice), &pb.CreateCalendarRequest{Scope: f.scope(), Name: "Native", DefaultTimeZone: "America/New_York", ApprovalRequired: &approval, OverlapPolicy: "exclusive", IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	f.calendar = created.Calendar
	for _, who := range []string{f.bob, f.client} {
		permissions := []string{"read", "write", "approve"}
		if who == f.client {
			permissions = []string{"read"}
		}
		granted, err := f.s.GrantCalendar(f.ctx(f.alice), &pb.GrantCalendarRequest{Scope: f.scope(), CalendarId: f.calendar.Id, PrincipalId: who, Permissions: permissions, ExpectedRevision: f.calendar.Revision, IdempotencyKey: uuid.NewString()})
		if err != nil {
			t.Fatal(err)
		}
		f.calendar = granted.Calendar
	}
	return f
}
func (f *fixture) ctx(who string) context.Context {
	return context.WithValue(context.Background(), principalKey{}, who)
}
func (f *fixture) scope() *pb.RequestScope {
	return &pb.RequestScope{TenantId: f.tenant, RequestId: uuid.NewString()}
}
func draftAt(day int) *pb.EntryDraft {
	start := time.Date(2026, 6, day, 14, 0, 0, 0, time.UTC)
	return &pb.EntryDraft{Title: "Synthetic appointment", Description: "PRIVATE_STAFF_NOTES", Kind: pb.EntryKind_ENTRY_KIND_APPOINTMENT, Visibility: pb.Visibility_VISIBILITY_INTERNAL, Schedule: &pb.EntryDraft_Timed{Timed: &pb.TimedInterval{Start: timestamppb.New(start), End: timestamppb.New(start.Add(time.Hour)), TimeZone: "America/New_York"}}}
}
func createCommand(draft *pb.EntryDraft) *pb.CalendarCommand {
	return &pb.CalendarCommand{Action: &pb.CalendarCommand_Create{Create: &pb.CreateEntry{Draft: draft}}}
}
func (f *fixture) create(t *testing.T, who string, draft *pb.EntryDraft) *pb.MutationResult {
	t.Helper()
	result, err := f.s.MutateEntry(f.ctx(who), &pb.MutateEntryRequest{Scope: f.scope(), CalendarId: f.calendar.Id, Command: createCommand(draft), IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	return result.Result
}
func TestNativeCalendarVisibilityAndClientRedaction(t *testing.T) {
	f := fixtureFor(t, false)
	private := draftAt(1)
	private.Visibility = pb.Visibility_VISIBILITY_PRIVATE
	first := f.create(t, f.alice, private)
	if _, err := f.s.GetEntry(f.ctx(f.bob), &pb.GetEntryRequest{Scope: f.scope(), CalendarId: f.calendar.Id, EntryId: first.EntryIds[0]}); status.Code(err) != codes.NotFound {
		t.Fatal("private event exposed", err)
	}
	shared := draftAt(2)
	shared.Visibility = pb.Visibility_VISIBILITY_SHARED
	shared.ParticipantPrincipalIds = []string{f.client, f.alice}
	second := f.create(t, f.alice, shared)
	visible, err := f.s.GetEntry(f.ctx(f.client), &pb.GetEntryRequest{Scope: f.scope(), CalendarId: f.calendar.Id, EntryId: second.EntryIds[0]})
	if err != nil {
		t.Fatal(err)
	}
	if visible.Entry.Description != "" || len(visible.Entry.ParticipantPrincipalIds) != 1 || visible.Entry.ParticipantPrincipalIds[0] != f.client {
		t.Fatal("attendee or staff notes leaked")
	}
	linked := draftAt(3)
	linked.Visibility = pb.Visibility_VISIBILITY_SHARED
	linked.ParticipantPrincipalIds = []string{f.client}
	linked.MatterId = f.matter
	hidden := f.create(t, f.alice, linked)
	if _, err = f.s.GetEntry(f.ctx(f.client), &pb.GetEntryRequest{Scope: f.scope(), CalendarId: f.calendar.Id, EntryId: hidden.EntryIds[0]}); status.Code(err) != codes.NotFound {
		t.Fatal("matter ACL bypassed")
	}
	list, err := f.s.ListEntries(f.ctx(f.client), &pb.ListEntriesRequest{Scope: f.scope(), CalendarId: f.calendar.Id, From: timestamppb.New(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)), To: timestamppb.New(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))})
	if err != nil || len(list.Entries) != 1 {
		t.Fatalf("client entries=%d err=%v", len(list.GetEntries()), err)
	}
	f.admin.Exec(context.Background(), "UPDATE memberships SET status='revoked' WHERE tenant_id=$1 AND principal_id=$2", f.tenant, f.client)
	if _, err = f.s.GetEntry(f.ctx(f.client), &pb.GetEntryRequest{Scope: f.scope(), CalendarId: f.calendar.Id, EntryId: second.EntryIds[0]}); status.Code(err) != codes.PermissionDenied {
		t.Fatal("revoked client retained access")
	}
}
func TestConcurrentOverlapAndIdempotentReceipt(t *testing.T) {
	f := fixtureFor(t, false)
	draft := draftAt(4)
	key := uuid.NewString()
	request := &pb.MutateEntryRequest{Scope: f.scope(), CalendarId: f.calendar.Id, Command: createCommand(draft), IdempotencyKey: key}
	first, err := f.s.MutateEntry(f.ctx(f.alice), request)
	if err != nil {
		t.Fatal(err)
	}
	request.Scope = f.scope()
	repeat, err := f.s.MutateEntry(f.ctx(f.alice), request)
	if err != nil || repeat.Result.EntryIds[0] != first.Result.EntryIds[0] {
		t.Fatal("idempotent retry changed entry")
	}
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for i := 0; i < 2; i++ {
		wait.Add(1)
		go func(upper bool) {
			defer wait.Done()
			scope := f.scope()
			calendar := f.calendar.Id
			if upper {
				scope.TenantId = strings.ToUpper(scope.TenantId)
				calendar = strings.ToUpper(calendar)
			}
			_, err := f.s.MutateEntry(f.ctx(f.alice), &pb.MutateEntryRequest{Scope: scope, CalendarId: calendar, Command: createCommand(draftAt(5)), IdempotencyKey: uuid.NewString()})
			results <- err
		}(i == 1)
	}
	wait.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if status.Code(err) == codes.AlreadyExists {
			conflict++
		} else {
			t.Error(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
}
func TestApprovalPinsCommandAndRejectsStaleRevision(t *testing.T) {
	f := fixtureFor(t, true)
	command := createCommand(draftAt(6))
	if _, err := f.s.MutateEntry(f.ctx(f.alice), &pb.MutateEntryRequest{Scope: f.scope(), CalendarId: f.calendar.Id, Command: command, IdempotencyKey: uuid.NewString()}); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("approval bypassed")
	}
	proposal, err := f.s.ProposeEntry(f.ctx(f.alice), &pb.ProposeEntryRequest{Scope: f.scope(), CalendarId: f.calendar.Id, Command: command, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := f.s.GetProposal(f.ctx(f.bob), &pb.GetProposalRequest{Scope: f.scope(), CalendarId: f.calendar.Id, ProposalId: proposal.Result.ProposalId})
	if err != nil {
		t.Fatal(err)
	}
	if stored.AffectedCount != 1 {
		t.Fatal("missing preview count")
	}
	decision := &pb.DecideProposalRequest{Scope: f.scope(), CalendarId: f.calendar.Id, ProposalId: stored.ProposalId, ExpectedSha256: stored.Sha256, Decision: "approve", IdempotencyKey: uuid.NewString()}
	result, err := f.s.DecideProposal(f.ctx(f.bob), decision)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.s.DecideProposal(f.ctx(f.bob), decision)
	if err != nil || again.Result.EntryIds[0] != result.Result.EntryIds[0] {
		t.Fatal("approval retry duplicated entry")
	}
	if _, err = f.admin.Exec(context.Background(), "UPDATE calendar_proposals SET command='tampered' WHERE id=$1", stored.ProposalId); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.GetProposal(f.ctx(f.alice), &pb.GetProposalRequest{Scope: f.scope(), CalendarId: f.calendar.Id, ProposalId: stored.ProposalId}); status.Code(err) != codes.DataLoss {
		t.Fatal("tampered proposal accepted")
	}
}
func TestScopedRecurrenceUpdatesAndCancellation(t *testing.T) {
	f := fixtureFor(t, false)
	draft := draftAt(10)
	draft.Recurrence = &pb.RecurrenceRule{Frequency: pb.Frequency_FREQUENCY_DAILY, Interval: 1, Count: 3}
	created := f.create(t, f.alice, draft)
	cancelled, err := f.s.MutateEntry(f.ctx(f.alice), &pb.MutateEntryRequest{Scope: f.scope(), CalendarId: f.calendar.Id, IdempotencyKey: uuid.NewString(), Command: &pb.CalendarCommand{Action: &pb.CalendarCommand_Cancel{Cancel: &pb.CancelEntry{EntryId: created.EntryIds[1], ExpectedRevision: 1, Scope: pb.ChangeScope_CHANGE_SCOPE_OCCURRENCE}}}})
	if err != nil {
		t.Fatal(err)
	}
	next := draftAt(10)
	next.Title = "Changed series"
	update := &pb.UpdateEntry{EntryId: created.EntryIds[0], ExpectedRevision: 1, ExpectedSeriesRevision: cancelled.Result.SeriesRevision, Scope: pb.ChangeScope_CHANGE_SCOPE_SERIES, Draft: next}
	reply, err := f.s.MutateEntry(f.ctx(f.alice), &pb.MutateEntryRequest{Scope: f.scope(), CalendarId: f.calendar.Id, IdempotencyKey: uuid.NewString(), Command: &pb.CalendarCommand{Action: &pb.CalendarCommand_Update{Update: update}}})
	if err != nil || len(reply.GetResult().GetEntryIds()) != 2 {
		t.Fatalf("scoped update failed: %v", err)
	}
	original, err := f.s.GetEntry(f.ctx(f.alice), &pb.GetEntryRequest{Scope: f.scope(), CalendarId: f.calendar.Id, EntryId: created.EntryIds[1]})
	if err != nil || original.Entry.Status != "cancelled" {
		t.Fatal("series edit resurrected cancelled occurrence")
	}
	update.ExpectedRevision = 1
	if _, err = f.s.MutateEntry(f.ctx(f.alice), &pb.MutateEntryRequest{Scope: f.scope(), CalendarId: f.calendar.Id, IdempotencyKey: uuid.NewString(), Command: &pb.CalendarCommand{Action: &pb.CalendarCommand_Update{Update: update}}}); status.Code(err) != codes.Aborted {
		t.Fatal("stale revision accepted")
	}
}
func TestReminderDueIsIdempotentAndChangedEntryCancelsIt(t *testing.T) {
	f := fixtureFor(t, false)
	draft := draftAt(20)
	draft.Reminders = []*pb.ReminderSpec{{MinutesBefore: 60}}
	created := f.create(t, f.alice, draft)
	items, err := f.s.ListReminders(f.ctx(f.alice), &pb.ListRemindersRequest{Scope: f.scope(), CalendarId: f.calendar.Id, EntryId: created.EntryIds[0]})
	if err != nil || len(items.GetReminders()) != 1 {
		t.Fatal(err)
	}
	reminder := items.Reminders[0]
	ref := &pb.ReminderWorkRequest{TenantId: f.tenant, ReminderId: reminder.Id}
	when := reminder.DueAt.AsTime().Add(time.Second)
	f.s.Now = func() time.Time { return when }
	for i := 0; i < 2; i++ {
		value, err := f.s.DeliverReminder(context.Background(), ref)
		if err != nil || value.Status != "due" {
			t.Fatalf("due=%v err=%v", value, err)
		}
	}
	var count int
	if err = f.admin.QueryRow(context.Background(), "SELECT count(*) FROM outbox_events WHERE tenant_id=$1 AND aggregate_id=$2 AND event_type='calendar.reminder.due.v1'", f.tenant, reminder.Id).Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate reminder effect", count, err)
	}
	update := draftAt(20)
	update.Title = "Changed appointment"
	_, err = f.s.MutateEntry(f.ctx(f.alice), &pb.MutateEntryRequest{Scope: f.scope(), CalendarId: f.calendar.Id, IdempotencyKey: uuid.NewString(), Command: &pb.CalendarCommand{Action: &pb.CalendarCommand_Update{Update: &pb.UpdateEntry{EntryId: created.EntryIds[0], ExpectedRevision: 1, Scope: pb.ChangeScope_CHANGE_SCOPE_OCCURRENCE, Draft: update}}}})
	if err != nil {
		t.Fatal(err)
	}
	value, err := f.s.DeliverReminder(context.Background(), ref)
	if err != nil || value.Status != "cancelled" {
		t.Fatal("changed entry retained stale reminder", err)
	}
}
func TestReminderReauthorizesOwnerBeforeFiring(t *testing.T) {
	f := fixtureFor(t, false)
	draft := draftAt(21)
	draft.Reminders = []*pb.ReminderSpec{{MinutesBefore: 5}}
	entry := f.create(t, f.alice, draft)
	items, err := f.s.ListReminders(f.ctx(f.alice), &pb.ListRemindersRequest{Scope: f.scope(), CalendarId: f.calendar.Id, EntryId: entry.EntryIds[0]})
	if err != nil {
		t.Fatal(err)
	}
	reminder := items.Reminders[0]
	f.admin.Exec(context.Background(), "DELETE FROM calendar_grants WHERE tenant_id=$1 AND calendar_id=$2 AND principal_id=$3 AND permission='read'", f.tenant, f.calendar.Id, f.alice)
	value, err := f.s.LoadReminderWork(context.Background(), &pb.ReminderWorkRequest{TenantId: f.tenant, ReminderId: reminder.Id})
	if err != nil || value.Status != "cancelled" {
		t.Fatal("revoked grant retained reminder", err)
	}
}

func TestSeriesApprovalRequiresEveryAffectedMatterAndNoWriteGrant(t *testing.T) {
	f := fixtureFor(t, false)
	draft := draftAt(10)
	draft.Recurrence = &pb.RecurrenceRule{Frequency: pb.Frequency_FREQUENCY_DAILY, Interval: 1, Count: 2}
	created := f.create(t, f.alice, draft)
	// A single occurrence may acquire a separate matter scope.
	changed := draftAt(11)
	changed.MatterId = f.matter
	_, err := f.s.MutateEntry(f.ctx(f.alice), &pb.MutateEntryRequest{Scope: f.scope(), CalendarId: f.calendar.Id, IdempotencyKey: uuid.NewString(), Command: &pb.CalendarCommand{Action: &pb.CalendarCommand_Update{Update: &pb.UpdateEntry{EntryId: created.EntryIds[1], ExpectedRevision: 1, Scope: pb.ChangeScope_CHANGE_SCOPE_OCCURRENCE, Draft: changed}}}})
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := f.s.ProposeEntry(f.ctx(f.alice), &pb.ProposeEntryRequest{Scope: f.scope(), CalendarId: f.calendar.Id, IdempotencyKey: uuid.NewString(), Command: &pb.CalendarCommand{Action: &pb.CalendarCommand_Cancel{Cancel: &pb.CancelEntry{EntryId: created.EntryIds[0], ExpectedRevision: 1, ExpectedSeriesRevision: 2, Scope: pb.ChangeScope_CHANGE_SCOPE_SERIES}}}})
	if err != nil {
		t.Fatal(err)
	}
	// An approver can inspect/decide without being granted entry mutation authority.
	ctx := context.Background()
	if _, err = f.admin.Exec(ctx, "DELETE FROM principal_capabilities WHERE tenant_id=$1 AND principal_id=$2 AND capability_id='calendar.write'", f.tenant, f.bob); err != nil {
		t.Fatal(err)
	}
	if _, err = f.admin.Exec(ctx, "DELETE FROM calendar_grants WHERE tenant_id=$1 AND principal_id=$2 AND permission='write'", f.tenant, f.bob); err != nil {
		t.Fatal(err)
	}
	stored, err := f.s.GetProposal(f.ctx(f.bob), &pb.GetProposalRequest{Scope: f.scope(), CalendarId: f.calendar.Id, ProposalId: proposal.Result.ProposalId})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.admin.Exec(ctx, "DELETE FROM matter_grants WHERE tenant_id=$1 AND principal_id=$2", f.tenant, f.bob); err != nil {
		t.Fatal(err)
	}
	decision := &pb.DecideProposalRequest{Scope: f.scope(), CalendarId: f.calendar.Id, ProposalId: stored.ProposalId, ExpectedSha256: stored.Sha256, Decision: "approve", IdempotencyKey: uuid.NewString()}
	if _, err = f.s.DecideProposal(f.ctx(f.bob), decision); status.Code(err) != codes.PermissionDenied {
		t.Fatal("reviewer bypassed other occurrence matter scope", err)
	}
	if _, err = f.s.GetProposal(f.ctx(f.bob), &pb.GetProposalRequest{Scope: f.scope(), CalendarId: f.calendar.Id, ProposalId: stored.ProposalId}); status.Code(err) != codes.PermissionDenied {
		t.Fatal("preview disclosed unauthorized series scope", err)
	}
	if _, err = f.admin.Exec(ctx, "INSERT INTO matter_grants(tenant_id,matter_id,principal_id,permission)VALUES($1,$2,$3,'matter.read')", f.tenant, f.matter, f.bob); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.DecideProposal(f.ctx(f.bob), decision); err != nil {
		t.Fatal(err)
	}
}

func TestSharedCalendarAllowsOverlappingAttorneysAndPolicyTransitions(t *testing.T) {
	f := fixtureFor(t, false)
	ctx := context.Background()
	// Default is a shared firm calendar, not a single resource reservation.
	noApproval := false
	created, err := f.s.CreateCalendar(f.ctx(f.alice), &pb.CreateCalendarRequest{Scope: f.scope(), Name: "Firm", DefaultTimeZone: "UTC", ApprovalRequired: &noApproval, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	if created.Calendar.OverlapPolicy != "shared" {
		t.Fatal("shared must be default")
	}
	f.calendar = created.Calendar
	for _, who := range []string{f.alice, f.bob} {
		if who == f.bob {
			granted, err := f.s.GrantCalendar(f.ctx(f.alice), &pb.GrantCalendarRequest{Scope: f.scope(), CalendarId: f.calendar.Id, PrincipalId: who, Permissions: []string{"read", "write"}, ExpectedRevision: f.calendar.Revision, IdempotencyKey: uuid.NewString()})
			if err != nil {
				t.Fatal(err)
			}
			f.calendar = granted.Calendar
		}
		draft := draftAt(8)
		draft.ParticipantPrincipalIds = []string{who}
		f.create(t, who, draft)
	}
	concurrent := make(chan error, 2)
	for _, who := range []string{f.alice, f.bob} {
		go func(who string) {
			draft := draftAt(9)
			draft.ParticipantPrincipalIds = []string{who}
			_, err := f.s.MutateEntry(f.ctx(who), &pb.MutateEntryRequest{Scope: f.scope(), CalendarId: f.calendar.Id, Command: createCommand(draft), IdempotencyKey: uuid.NewString()})
			concurrent <- err
		}(who)
	}
	for i := 0; i < 2; i++ {
		if err := <-concurrent; err != nil {
			t.Fatal("shared calendar rejected concurrent attorneys", err)
		}
	}
	// Shared calendars do not infer an attendee lock either: participants are
	// visibility/attendance metadata, not a canonical resource reservation model.
	third := draftAt(8)
	third.ParticipantPrincipalIds = []string{f.alice}
	f.create(t, f.alice, third)
	request := &pb.SetOverlapPolicyRequest{Scope: f.scope(), CalendarId: f.calendar.Id, OverlapPolicy: "exclusive", ExpectedRevision: f.calendar.Revision, IdempotencyKey: uuid.NewString()}
	if _, err = f.s.SetOverlapPolicy(f.ctx(f.bob), request); status.Code(err) != codes.PermissionDenied {
		t.Fatal("non-manager changed policy", err)
	}
	if _, err = f.s.SetOverlapPolicy(f.ctx(f.alice), request); status.Code(err) != codes.AlreadyExists {
		t.Fatal("exclusive policy allowed existing overlaps", err)
	}
	var policy string
	if err = f.admin.QueryRow(ctx, "SELECT overlap_policy FROM calendars WHERE id=$1", f.calendar.Id).Scan(&policy); err != nil || policy != "shared" {
		t.Fatal("failed transition did not roll back", policy, err)
	}
	// A separate empty resource calendar can opt into exclusive mode explicitly.
	empty, err := f.s.CreateCalendar(f.ctx(f.alice), &pb.CreateCalendarRequest{Scope: f.scope(), Name: "Room A", DefaultTimeZone: "UTC", ApprovalRequired: &noApproval, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	request.CalendarId = empty.Calendar.Id
	request.ExpectedRevision = empty.Calendar.Revision
	request.IdempotencyKey = uuid.NewString()
	changed, err := f.s.SetOverlapPolicy(f.ctx(f.alice), request)
	if err != nil || changed.Calendar.OverlapPolicy != "exclusive" {
		t.Fatal(err)
	}
	repeated, err := f.s.SetOverlapPolicy(f.ctx(f.alice), request)
	if err != nil || repeated.Calendar.Revision != changed.Calendar.Revision {
		t.Fatal("policy retry not idempotent", err)
	}
	f.calendar = changed.Calendar
	f.create(t, f.alice, draftAt(8))
	if _, err = f.s.MutateEntry(f.ctx(f.alice), &pb.MutateEntryRequest{Scope: f.scope(), CalendarId: f.calendar.Id, Command: createCommand(draftAt(8)), IdempotencyKey: uuid.NewString()}); status.Code(err) != codes.AlreadyExists {
		t.Fatal("exclusive calendar accepted overlap", err)
	}
	// Managers can explicitly convert existing exclusive firm calendars to shared.
	request.ExpectedRevision = f.calendar.Revision
	request.OverlapPolicy = "shared"
	request.IdempotencyKey = uuid.NewString()
	changed, err = f.s.SetOverlapPolicy(f.ctx(f.alice), request)
	if err != nil {
		t.Fatal(err)
	}
	f.calendar = changed.Calendar
	f.create(t, f.alice, draftAt(8))
}

func TestDatabasePolicyCannotBeBypassedByEntryFlag(t *testing.T) {
	f := fixtureFor(t, false)
	created := f.create(t, f.alice, draftAt(9))
	err := f.s.tx(context.Background(), f.tenant, f.calendar.Id, true, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), "UPDATE calendar_entries SET overlap_policy='shared' WHERE tenant_id=$1 AND id=$2", f.tenant, created.EntryIds[0])
		return err
	})
	if err == nil {
		t.Fatal("entry downgraded exclusive constraint independently")
	}
}

package contextengine

import (
	"context"
	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/context/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"testing"
	"time"
)

func TestCuratedKnowledgeLibraryRequiresBothKnowledgeAndWorkspaceGrants(t *testing.T) {
	f := fixtureFor(t)
	f.ingest(t, "Firm knowledge: synthetic lease checklist.")
	ctx := context.Background()
	f.admin.Exec(ctx, "INSERT INTO principal_capabilities VALUES($1,$2,'knowledge.manage')", f.tenant, f.who)
	if _, err := f.s.RegisterKnowledgeLibrary(f.ctx(), &pb.KnowledgeLibraryRequest{Scope: f.scope(), MatterId: f.matter, Name: "Firm precedents"}); err != nil {
		t.Fatal(err)
	}
	request := &pb.BuildContextRequest{Scope: f.scope(), MatterId: f.matter, RecipeId: "firm.knowledge.v1", Query: "lease"}
	if _, err := f.s.BuildContext(f.ctx(), request); status.Code(err) != codes.PermissionDenied {
		t.Fatal("knowledge capability missing", err)
	}
	f.admin.Exec(ctx, "INSERT INTO principal_capabilities VALUES($1,$2,'knowledge.read')", f.tenant, f.who)
	built, err := f.s.BuildContext(f.ctx(), request)
	if err != nil || len(built.Package.Sources) != 1 {
		t.Fatal(built, err)
	}
	f.admin.Exec(ctx, "DELETE FROM principal_capabilities WHERE tenant_id=$1 AND principal_id=$2 AND capability_id='knowledge.read'", f.tenant, f.who)
	if _, err = f.s.GetContext(f.ctx(), &pb.GetContextRequest{Scope: f.scope(), MatterId: f.matter, PackageId: built.Package.Id}); err == nil {
		t.Fatal("cached library context survived revocation")
	}
	request.RecipeId = "matter.qa.v1"
	if _, err = f.s.BuildContext(f.ctx(), request); err == nil {
		t.Fatal("generic recipe bypassed library permission")
	}
}
func TestCalendarContextRespectsPrivateVisibilityAndCurrentGrants(t *testing.T) {
	f := fixtureFor(t)
	ctx := context.Background()
	cal, entry, private := uuid.NewString(), uuid.NewString(), uuid.NewString()
	start := time.Now().UTC().Truncate(time.Second)
	end := start.Add(time.Hour)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := f.admin.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		f.admin.Exec(ctx, "DELETE FROM calendar_entries WHERE tenant_id=$1", f.tenant)
		f.admin.Exec(ctx, "DELETE FROM calendar_grants WHERE tenant_id=$1", f.tenant)
		f.admin.Exec(ctx, "DELETE FROM calendars WHERE tenant_id=$1", f.tenant)
	})
	exec("INSERT INTO principal_capabilities VALUES($1,$2,'calendar.read')", f.tenant, f.who)
	exec("INSERT INTO calendars(id,tenant_id,name,default_time_zone,created_by)VALUES($1,$2,'Firm','UTC',$3)", cal, f.tenant, f.who)
	exec("INSERT INTO calendar_grants VALUES($1,$2,$3,'read')", f.tenant, cal, f.who)
	for _, row := range []struct {
		ID, Creator string
		Visibility  int
	}{{entry, f.who, 2}, {private, "another-actor", 1}} {
		exec("INSERT INTO calendar_entries(id,tenant_id,calendar_id,matter_id,title,kind,visibility,start_at,end_at,time_zone,all_day,created_by,overlap_policy)VALUES($1,$2,$3,$4,'Synthetic',1,$5,$6,$7,'UTC',false,$8,'shared')", row.ID, f.tenant, cal, f.matter, row.Visibility, start, end, row.Creator)
	}
	request := &pb.CalendarContextRequest{Scope: f.scope(), MatterId: f.matter, From: start.Add(-time.Hour).Format(time.RFC3339), To: end.Add(time.Hour).Format(time.RFC3339)}
	reply, err := f.s.BuildCalendarContext(f.ctx(), request)
	if err != nil || len(reply.Entries) != 1 || reply.Entries[0].EntryId != entry {
		t.Fatal("private event leaked", reply, err)
	}
	exec("UPDATE calendar_entries SET revision=revision+1 WHERE id=$1", entry)
	changed, err := f.s.BuildCalendarContext(f.ctx(), request)
	if err != nil || changed.SnapshotSha256 == reply.SnapshotSha256 {
		t.Fatal("calendar version hash not updated")
	}
	exec("DELETE FROM calendar_grants WHERE tenant_id=$1", f.tenant)
	reply, err = f.s.BuildCalendarContext(f.ctx(), request)
	if err != nil || len(reply.Entries) != 0 {
		t.Fatal("revoked calendar data leaked", reply, err)
	}
}

package integrations

import (
	"context"
	"errors"
	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/integrations/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"net/url"
	"strings"
)

// Absence in a list is never a deletion proof. Probe the stored external ID;
// 404/410 produce a reversible unavailable tombstone, not physical deletion.
func (p *Provider) Probe(ctx context.Context, kind, id, token string) (bool, error) {
	target := ""
	if p.ID == "clio-manage" {
		if kind == "calendar" {
			parts := strings.SplitN(id, ":", 2)
			if len(parts) != 2 {
				return false, errors.New("invalid calendar mapping")
			}
			id = parts[1]
		}
		root := strings.TrimSuffix(p.resourcePath(kind), ".json")
		target = p.Base + root + "/" + url.PathEscape(id) + ".json?fields=id"
	} else {
		target = p.Base + p.resourcePath(kind) + "/" + url.PathEscape(id)
	}
	response, err := p.request(ctx, "GET", target, token, "", nil)
	if err != nil {
		return false, unavailable("PROVIDER_PROBE_UNAVAILABLE")
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case 200:
		return true, nil
	case 404, 410:
		return false, nil
	default:
		return false, unavailable("PROVIDER_PROBE_UNAVAILABLE")
	}
}
func (s *Service) reconcileMissing(ctx context.Context, tenant, jobID, connectionID, who string, p *Provider, token string) (*pb.JobResponse, error) {
	var kind, external, id string
	err := s.tx(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT resource_type,external_id,local_id::text FROM provider_sync_bindings WHERE tenant_id=$1 AND connection_id=$2 AND NOT deleted AND seen_job_id IS DISTINCT FROM $3::uuid ORDER BY resource_type,external_id LIMIT 1", tenant, connectionID, jobID).Scan(&kind, &external, &id)
	})
	if status.Code(err) == codes.NotFound {
		err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
			if _, e := connectionAccess(ctx, tx, tenant, connectionID, who, "sync"); e != nil {
				return e
			}
			_, e := tx.Exec(ctx, "UPDATE integration_jobs SET state='completed',updated_at=now() WHERE tenant_id=$1 AND id=$2", tenant, jobID)
			return e
		})
		return &pb.JobResponse{Id: jobID, State: "completed"}, err
	}
	if err != nil {
		return nil, err
	}
	exists, err := p.Probe(ctx, kind, external, token)
	if err != nil {
		return nil, err
	}
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if e := lock(ctx, tx, tenant, connectionID); e != nil {
			return e
		}
		if _, e := connectionAccess(ctx, tx, tenant, connectionID, who, "sync"); e != nil {
			return e
		}
		if _, e := tx.Exec(ctx, "UPDATE provider_sync_bindings SET seen_job_id=$4,deleted=$5,last_synced_at=now() WHERE tenant_id=$1 AND connection_id=$2 AND resource_type=$3 AND local_id=$6", tenant, connectionID, kind, jobID, !exists, id); e != nil {
			return e
		}
		if exists {
			return nil
		}
		switch kind {
		case "calendar_entry":
			if _, e := tx.Exec(ctx, "UPDATE calendar_entries SET status='cancelled',revision=revision+1,updated_at=now() WHERE tenant_id=$1 AND id=$2", tenant, id); e != nil {
				return e
			}
			if e := changed(ctx, tx, tenant, id, "calendar_entries", "calendar.entry.cancelled.v1"); e != nil {
				return e
			}
		case "matter":
			if _, e := tx.Exec(ctx, "UPDATE matters SET status='provider_unavailable',revision=revision+1 WHERE tenant_id=$1 AND id=$2", tenant, id); e != nil {
				return e
			}
			if e := changed(ctx, tx, tenant, id, "matters", "matter.updated.v1"); e != nil {
				return e
			}
		case "calendar":
			if _, e := tx.Exec(ctx, "UPDATE calendar_entries SET status='cancelled',revision=revision+1,updated_at=now() WHERE tenant_id=$1 AND calendar_id=$2 AND status='active'", tenant, id); e != nil {
				return e
			}
		}
		return audit(ctx, tx, tenant, who, id, "provider.resource.unavailable", "missing_or_inaccessible")
	})
	return &pb.JobResponse{Id: jobID, State: "running"}, err
}

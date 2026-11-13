package integrations

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/integrations/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"net/url"
	"strings"
	"time"
)

type connection struct {
	ID, Provider, State, Owner string
	Revision                   int64
}

func connectionAccess(ctx context.Context, tx pgx.Tx, tenant, id, who, permission string) (connection, error) {
	var c connection
	if !validID(id) {
		return c, bad("INVALID_CONNECTION")
	}
	cap := "provider." + permission
	if err := access(ctx, tx, tenant, who, cap, ""); err != nil {
		return c, err
	}
	var allowed bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM provider_connection_grants WHERE tenant_id=$1 AND connection_id=$2 AND principal_id=$3 AND permission=$4)", tenant, id, who, permission).Scan(&allowed); err != nil {
		return c, err
	}
	if !allowed {
		return c, denied()
	}
	err := tx.QueryRow(ctx, "SELECT id::text,provider_id,status,coalesce(created_by,''),revision FROM provider_connections WHERE tenant_id=$1 AND id=$2", tenant, id).Scan(&c.ID, &c.Provider, &c.State, &c.Owner, &c.Revision)
	return c, err
}
func (c connection) public() *pb.Connection {
	return &pb.Connection{Id: c.ID, ProviderId: c.Provider, State: c.State, Revision: c.Revision, Capabilities: []string{"matter.read.v1", "calendar.read.v1", "sync.poll.v1"}}
}
func (s *Service) CreateConnection(ctx context.Context, r *pb.CreateConnectionRequest) (*pb.ConnectionResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	if s.Providers[r.ProviderId] == nil || s.Keys == nil {
		return nil, disabled()
	}
	who := actor(ctx)
	reply := new(pb.ConnectionResponse)
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if err := access(ctx, tx, tenant, who, "provider.manage", ""); err != nil {
			return err
		}
		if err := lock(ctx, tx, tenant, who); err != nil {
			return err
		}
		hash := fingerprint("create-connection", r)
		hit, err := cached(ctx, tx, tenant, who, r.IdempotencyKey, hash, reply)
		if err != nil || hit {
			return err
		}
		id := uuid.NewString()
		if _, err = tx.Exec(ctx, "INSERT INTO provider_connections(id,tenant_id,provider_id,status,created_by,capabilities)VALUES($1,$2,$3,'authorization_required',$4,'{}')", id, tenant, r.ProviderId, who); err != nil {
			return err
		}
		for _, permission := range []string{"read", "manage", "sync", "approve"} {
			if _, err = tx.Exec(ctx, "INSERT INTO provider_connection_grants VALUES($1,$2,$3,$4)", tenant, id, who, permission); err != nil {
				return err
			}
		}
		reply.Connection = (connection{ID: id, Provider: r.ProviderId, State: "authorization_required", Owner: who, Revision: 1}).public()
		if err = audit(ctx, tx, tenant, who, id, "provider.connection.created", "authorization_required"); err != nil {
			return err
		}
		return save(ctx, tx, tenant, who, r.IdempotencyKey, hash, reply)
	})
	return reply, err
}
func (s *Service) AuthorizeConnection(ctx context.Context, r *pb.ConnectionRequest) (*pb.AuthorizeResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	reply := new(pb.AuthorizeResponse)
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		c, err := connectionAccess(ctx, tx, tenant, r.ConnectionId, actor(ctx), "manage")
		if err != nil {
			return err
		}
		p := s.Providers[c.Provider]
		if p == nil || s.Keys == nil {
			return disabled()
		}
		state := make([]byte, 32)
		if _, err = rand.Read(state); err != nil {
			return err
		}
		opaque := base64.RawURLEncoding.EncodeToString(state)
		if _, err = tx.Exec(ctx, "INSERT INTO provider_oauth_states(id,tenant_id,connection_id,principal_id,state_sha256,expires_at)VALUES($1,$2,$3,$4,$5,$6)", uuid.NewString(), tenant, c.ID, actor(ctx), sha([]byte(opaque)), time.Now().Add(10*time.Minute)); err != nil {
			return err
		}
		u, _ := url.Parse(p.AuthorizeURL)
		q := u.Query()
		q.Set("client_id", p.ClientID)
		q.Set("response_type", "code")
		q.Set("redirect_uri", p.Redirect)
		q.Set("state", opaque)
		u.RawQuery = q.Encode()
		reply.AuthorizeUrl = u.String()
		reply.ExpiresSeconds = 600
		return nil
	})
	return reply, err
}
func (s *Service) CompleteOAuth(ctx context.Context, r *pb.OAuthCompleteRequest) (*pb.ConnectionResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	if len(r.Code) < 1 || len(r.Code) > 4096 || len(r.State) < 32 || len(r.State) > 128 {
		return nil, bad("INVALID_OAUTH_CALLBACK")
	}
	var c connection
	// Consume state before external exchange; an uncertain code exchange requires a
	// new authorization flow instead of replaying a one-time code blindly.
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		var err error
		c, err = connectionAccess(ctx, tx, tenant, r.ConnectionId, actor(ctx), "manage")
		if err != nil {
			return err
		}
		if s.Providers[c.Provider] == nil || s.Keys == nil {
			return disabled()
		}
		changed, err := tx.Exec(ctx, "UPDATE provider_oauth_states SET consumed_at=now() WHERE tenant_id=$1 AND connection_id=$2 AND principal_id=$3 AND state_sha256=$4 AND consumed_at IS NULL AND expires_at>now()", tenant, c.ID, actor(ctx), sha([]byte(r.State)))
		if err != nil {
			return err
		}
		if changed.RowsAffected() != 1 {
			return status.Error(codes.FailedPrecondition, "OAUTH_STATE_INVALID_OR_REPLAYED")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	tokens, err := s.Providers[c.Provider].Exchange(ctx, r.Code, "")
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "OAUTH_RESTART_REQUIRED")
	}
	raw, _ := json.Marshal(tokens)
	sealed, err := s.Keys.Seal(raw, tenant+"|"+c.ID+"|"+c.Provider)
	if err != nil {
		return nil, unavailable("CREDENTIAL_ENCRYPTION_FAILED")
	}
	reply := new(pb.ConnectionResponse)
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		current, err := connectionAccess(ctx, tx, tenant, c.ID, actor(ctx), "manage")
		if err != nil {
			return err
		}
		if current.Revision != c.Revision {
			return status.Error(codes.Aborted, "CONNECTION_CHANGED_DURING_OAUTH")
		}
		if _, err = tx.Exec(ctx, "UPDATE provider_connections SET encrypted_tokens=$3,token_expires_at=$4,status='active',revision=revision+1 WHERE tenant_id=$1 AND id=$2", tenant, c.ID, sealed, time.Now().Add(time.Duration(tokens.ExpiresIn)*time.Second)); err != nil {
			return err
		}
		c.State = "active"
		c.Revision++
		reply.Connection = c.public()
		return audit(ctx, tx, tenant, actor(ctx), c.ID, "provider.connection.authorized", "active")
	})
	return reply, err
}
func (s *Service) token(ctx context.Context, tenant, id, who, permission string) (*Provider, string, error) {
	var provider *Provider
	var accessToken string
	err := s.tx(ctx, tenant, func(tx pgx.Tx) error {
		c, err := connectionAccess(ctx, tx, tenant, id, who, permission)
		if err != nil {
			return err
		}
		if c.State != "active" {
			return status.Error(codes.FailedPrecondition, "CONNECTION_NOT_ACTIVE")
		}
		provider = s.Providers[c.Provider]
		if provider == nil || s.Keys == nil {
			return disabled()
		}
		var ciphertext []byte
		var expiry time.Time
		if err = tx.QueryRow(ctx, "SELECT encrypted_tokens,token_expires_at FROM provider_connections WHERE tenant_id=$1 AND id=$2 FOR UPDATE", tenant, id).Scan(&ciphertext, &expiry); err != nil {
			return err
		}
		plain, err := s.Keys.Open(ciphertext, tenant+"|"+c.ID+"|"+c.Provider)
		if err != nil {
			return unavailable("CREDENTIAL_INTEGRITY_FAILURE")
		}
		var tokens Tokens
		if json.Unmarshal(plain, &tokens) != nil {
			return unavailable("CREDENTIAL_INTEGRITY_FAILURE")
		}
		if !expiry.After(time.Now().Add(time.Minute)) {
			if tokens.Refresh == "" {
				return status.Error(codes.FailedPrecondition, "PROVIDER_REAUTHORIZATION_REQUIRED")
			}
			tokens, err = provider.Exchange(ctx, "", tokens.Refresh)
			if err != nil {
				return unavailable("TOKEN_REFRESH_UNAVAILABLE")
			}
			raw, _ := json.Marshal(tokens)
			ciphertext, err = s.Keys.Seal(raw, tenant+"|"+c.ID+"|"+c.Provider)
			if err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, "UPDATE provider_connections SET encrypted_tokens=$3,token_expires_at=$4 WHERE tenant_id=$1 AND id=$2", tenant, id, ciphertext, time.Now().Add(time.Duration(tokens.ExpiresIn)*time.Second)); err != nil {
				return err
			}
		}
		accessToken = tokens.Access
		return nil
	})
	return provider, accessToken, err
}
func (s *Service) Disconnect(ctx context.Context, r *pb.ConnectionRequest) (*pb.ConnectionResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	reply := new(pb.ConnectionResponse)
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		c, err := connectionAccess(ctx, tx, tenant, r.ConnectionId, actor(ctx), "manage")
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "UPDATE provider_connections SET status='disabled',encrypted_tokens=NULL,token_expires_at=NULL,revision=revision+1 WHERE tenant_id=$1 AND id=$2", tenant, c.ID); err != nil {
			return err
		}
		c.State = "disabled"
		c.Revision++
		reply.Connection = c.public()
		return audit(ctx, tx, tenant, actor(ctx), c.ID, "provider.connection.disconnected", "local_credentials_destroyed")
	})
	return reply, err
}
func (s *Service) RequestSync(ctx context.Context, r *pb.SyncRequest) (*pb.JobResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	reply := new(pb.JobResponse)
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		c, err := connectionAccess(ctx, tx, tenant, r.ConnectionId, actor(ctx), "sync")
		if err != nil {
			return err
		}
		if c.State != "active" || s.Providers[c.Provider] == nil {
			return disabled()
		}
		if err = lock(ctx, tx, tenant, c.ID); err != nil {
			return err
		}
		hash := fingerprint("request-sync", r)
		hit, err := cached(ctx, tx, tenant, actor(ctx), r.IdempotencyKey, hash, reply)
		if err != nil || hit {
			return err
		}
		var busy bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM integration_jobs WHERE tenant_id=$1 AND resource_id=$2 AND kind='provider_sync' AND state IN('queued','running'))", tenant, c.ID).Scan(&busy); err != nil {
			return err
		}
		if busy {
			return status.Error(codes.AlreadyExists, "SYNC_ALREADY_RUNNING")
		}
		id := uuid.NewString()
		if err = job(ctx, tx, tenant, actor(ctx), id, "provider_sync", c.ID); err != nil {
			return err
		}
		reply.Id = id
		reply.State = "queued"
		return save(ctx, tx, tenant, actor(ctx), r.IdempotencyKey, hash, reply)
	})
	return reply, err
}

var _ = strings.TrimSpace

func (s *Service) GrantConnection(ctx context.Context, r *pb.GrantConnectionRequest) (*pb.ConnectionResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	reply := new(pb.ConnectionResponse)
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if err := lock(ctx, tx, tenant, r.ConnectionId); err != nil {
			return err
		}
		c, err := connectionAccess(ctx, tx, tenant, r.ConnectionId, actor(ctx), "manage")
		if err != nil {
			return err
		}
		var member bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM memberships WHERE tenant_id=$1 AND principal_id=$2 AND status='active')", tenant, r.PrincipalId).Scan(&member); err != nil {
			return err
		}
		if !member {
			return denied()
		}
		seen := map[string]bool{}
		for _, permission := range r.Permissions {
			if seen[permission] || (permission != "read" && permission != "manage" && permission != "sync" && permission != "approve") {
				return bad("INVALID_CONNECTION_GRANT")
			}
			seen[permission] = true
		}
		hash := fingerprint("grant-connection", r)
		hit, err := cached(ctx, tx, tenant, actor(ctx), r.IdempotencyKey, hash, reply)
		if err != nil || hit {
			return err
		}
		if c.Revision != r.ExpectedRevision {
			return status.Error(codes.Aborted, "CONNECTION_REVISION_CHANGED")
		}
		if _, err = tx.Exec(ctx, "DELETE FROM provider_connection_grants WHERE tenant_id=$1 AND connection_id=$2 AND principal_id=$3", tenant, r.ConnectionId, r.PrincipalId); err != nil {
			return err
		}
		for _, permission := range r.Permissions {
			if _, err = tx.Exec(ctx, "INSERT INTO provider_connection_grants VALUES($1,$2,$3,$4)", tenant, r.ConnectionId, r.PrincipalId, permission); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, "UPDATE provider_connections SET revision=revision+1 WHERE tenant_id=$1 AND id=$2", tenant, r.ConnectionId); err != nil {
			return err
		}
		c.Revision++
		reply.Connection = c.public()
		if err = audit(ctx, tx, tenant, actor(ctx), r.ConnectionId, "provider.grants.updated", "explicit"); err != nil {
			return err
		}
		return save(ctx, tx, tenant, actor(ctx), r.IdempotencyKey, hash, reply)
	})
	return reply, err
}

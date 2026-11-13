package integrations

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/integrations/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Clio Manage's delayed activation binds the handshake to the ID returned by the
// authenticated creation response. A callback alone can never activate a hook.
func (s *Service) RegisterWebhook(ctx context.Context, r *pb.WebhookRequest) (*pb.WebhookResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	enabled, e := requiredFeature("PROVIDER_WEBHOOKS_ENABLED")
	if e != nil || !enabled {
		return nil, disabled()
	}
	base := strings.TrimRight(os.Getenv("PROVIDER_WEBHOOK_PUBLIC_URL"), "/")
	if endpoint(base) != nil || (!strings.HasPrefix(base, "https://") && os.Getenv("APP_ENV") != "development") {
		return nil, bad("INVALID_WEBHOOK_ENDPOINT")
	}
	if r.Model != "matter" && r.Model != "calendar_entry" {
		return nil, bad("INVALID_WEBHOOK_MODEL")
	}
	reply := new(pb.WebhookResponse)
	tokenBytes := make([]byte, 32)
	rand.Read(tokenBytes)
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	created := false
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		c, e := connectionAccess(ctx, tx, tenant, r.ConnectionId, actor(ctx), "manage")
		if e != nil {
			return e
		}
		if c.Provider != "clio-manage" || c.State != "active" {
			return disabled()
		}
		if _, e = connectionAccess(ctx, tx, tenant, c.ID, actor(ctx), "sync"); e != nil {
			return e
		}
		if e = lock(ctx, tx, tenant, c.ID); e != nil {
			return e
		}
		hit, e := cached(ctx, tx, tenant, actor(ctx), r.IdempotencyKey, fingerprint("register-webhook", r), reply)
		if e != nil || hit {
			return e
		}
		reply.SubscriptionId = uuid.NewString()
		reply.State = "creating"
		if _, e = tx.Exec(ctx, "INSERT INTO provider_webhooks(id,tenant_id,connection_id,principal_id,model,callback_sha256)VALUES($1,$2,$3,$4,$5,$6)", reply.SubscriptionId, tenant, c.ID, actor(ctx), r.Model, sha([]byte(token))); e != nil {
			return e
		}
		created = true
		return save(ctx, tx, tenant, actor(ctx), r.IdempotencyKey, fingerprint("register-webhook", r), reply)
	})
	if err != nil || !created {
		return reply, err
	}
	p, access, err := s.token(ctx, tenant, r.ConnectionId, actor(ctx), "manage")
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]any{"data": map[string]any{"url": base + "/" + reply.SubscriptionId + "/" + token, "model": r.Model, "fields": "id,etag", "events": []string{"created", "updated", "deleted"}}})
	response, err := p.request(ctx, "POST", p.Base+"/webhooks.json?fields=id", access, "", body)
	remote := ""
	if err == nil {
		defer response.Body.Close()
		if response.StatusCode == 201 {
			raw, _ := io.ReadAll(io.LimitReader(response.Body, 65537))
			var v struct {
				Data struct {
					ID json.RawMessage `json:"id"`
				} `json:"data"`
			}
			if len(raw) <= 65536 && json.Unmarshal(raw, &v) == nil {
				remote = scalar(v.Data.ID)
			}
		}
	}
	state := "pending"
	if remote == "" {
		state = "uncertain"
	}
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, "UPDATE provider_webhooks SET remote_id=NULLIF($3,''),state=$4 WHERE tenant_id=$1 AND id=$2", tenant, reply.SubscriptionId, remote, state)
		return e
	})
	reply.State = state
	return reply, err
}
func (s *Service) ActivateWebhook(ctx context.Context, r *pb.WebhookRequest) (*pb.WebhookResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	var connection, remote, handshake, state string
	var encrypted []byte
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if e := tx.QueryRow(ctx, "SELECT connection_id::text,coalesce(remote_id,''),coalesce(handshake_id,''),state,encrypted_secret FROM provider_webhooks WHERE tenant_id=$1 AND id=$2", tenant, r.SubscriptionId).Scan(&connection, &remote, &handshake, &state, &encrypted); e != nil {
			return e
		}
		_, e := connectionAccess(ctx, tx, tenant, connection, actor(ctx), "manage")
		return e
	})
	if err != nil {
		return nil, err
	}
	reply := &pb.WebhookResponse{SubscriptionId: r.SubscriptionId, State: state}
	if state == "active" {
		return reply, nil
	}
	if state != "pending" || remote == "" || remote != handshake || s.Keys == nil {
		return nil, status.Error(codes.FailedPrecondition, "WEBHOOK_HANDSHAKE_PENDING_OR_MISMATCH")
	}
	secret, err := s.Keys.Open(encrypted, "webhook|"+tenant+"|"+r.SubscriptionId)
	if err != nil {
		return nil, unavailable("WEBHOOK_SECRET_UNAVAILABLE")
	}
	p, access, err := s.token(ctx, tenant, connection, actor(ctx), "manage")
	if err != nil {
		return nil, err
	}
	request, _ := http.NewRequestWithContext(ctx, "PUT", p.Base+"/webhooks/"+url.PathEscape(remote)+"/activate", nil)
	request.Header.Set("Authorization", "Bearer "+access)
	request.Header.Set("X-Hook-Secret", string(secret))
	response, err := p.HTTP.Do(request)
	if err != nil {
		return nil, unavailable("WEBHOOK_ACTIVATION_UNAVAILABLE")
	}
	response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, status.Error(codes.FailedPrecondition, "WEBHOOK_ACTIVATION_REJECTED")
	}
	err = s.tx(ctx, tenant, func(tx pgx.Tx) error {
		if _, e := connectionAccess(ctx, tx, tenant, connection, actor(ctx), "manage"); e != nil {
			return e
		}
		_, e := tx.Exec(ctx, "UPDATE provider_webhooks SET state='active' WHERE tenant_id=$1 AND id=$2 AND state='pending'", tenant, r.SubscriptionId)
		return e
	})
	reply.State = "active"
	return reply, err
}
func (s *Service) ReceiveWebhook(ctx context.Context, r *pb.ReceiveWebhookRequest) (*pb.ReceiveWebhookResponse, error) {
	enabled, _ := requiredFeature("PROVIDER_WEBHOOKS_ENABLED")
	if !enabled {
		return nil, disabled()
	}
	if r.Secret == "" && len(r.Signature) != 64 {
		return nil, denied()
	}
	if !validID(r.SubscriptionId) || len(r.CallbackToken) < 32 || len(r.CallbackToken) > 128 || len(r.Body) > 65536 {
		return nil, bad("INVALID_WEBHOOK")
	}
	var tenant, expected string
	if err := s.Pool.QueryRow(ctx, "SELECT tenant_id::text,callback_sha256 FROM locate_provider_webhook($1)", r.SubscriptionId).Scan(&tenant, &expected); err != nil || !hmac.Equal([]byte(expected), []byte(sha([]byte(r.CallbackToken)))) {
		return nil, denied()
	}
	var envelope struct {
		Data struct {
			ID json.RawMessage `json:"id"`
		} `json:"data"`
		Meta struct {
			Event     string          `json:"event"`
			WebhookID json.RawMessage `json:"webhook_id"`
		} `json:"meta"`
	}
	if json.Unmarshal(r.Body, &envelope) != nil {
		return nil, bad("INVALID_WEBHOOK")
	}
	remote := scalar(envelope.Meta.WebhookID)
	if r.Secret != "" && remote == "" {
		remote = scalar(envelope.Data.ID)
	}
	err := s.tx(ctx, tenant, func(tx pgx.Tx) error {
		var connection, who, state, remoteID, handshake string
		var encrypted []byte
		if e := tx.QueryRow(ctx, "SELECT connection_id::text,principal_id,state,coalesce(remote_id,''),coalesce(handshake_id,''),encrypted_secret FROM provider_webhooks WHERE tenant_id=$1 AND id=$2 FOR UPDATE", tenant, r.SubscriptionId).Scan(&connection, &who, &state, &remoteID, &handshake, &encrypted); e != nil {
			return e
		}
		if s.Keys == nil {
			return disabled()
		}
		if r.Secret != "" {
			if len(r.Secret) < 16 || len(r.Secret) > 512 || strings.ContainsAny(r.Secret, "\r\n") || remote == "" || (state != "creating" && state != "pending") || (remoteID != "" && remoteID != remote) {
				return denied()
			}
			if encrypted != nil {
				old, e := s.Keys.Open(encrypted, "webhook|"+tenant+"|"+r.SubscriptionId)
				if e != nil || handshake != remote || !hmac.Equal(old, []byte(r.Secret)) {
					return denied()
				}
				return nil
			}
			sealed, e := s.Keys.Seal([]byte(r.Secret), "webhook|"+tenant+"|"+r.SubscriptionId)
			if e != nil {
				return e
			}
			_, e = tx.Exec(ctx, "UPDATE provider_webhooks SET handshake_id=$3,encrypted_secret=$4 WHERE tenant_id=$1 AND id=$2", tenant, r.SubscriptionId, remote, sealed)
			return e
		}
		if state != "active" || remoteID != remote {
			return denied()
		}
		secret, e := s.Keys.Open(encrypted, "webhook|"+tenant+"|"+r.SubscriptionId)
		if e != nil {
			return denied()
		}
		signature, e := hex.DecodeString(r.Signature)
		mac := hmac.New(sha256.New, secret)
		mac.Write(r.Body)
		if e != nil || !hmac.Equal(signature, mac.Sum(nil)) {
			return denied()
		}
		if envelope.Meta.Event != "created" && envelope.Meta.Event != "updated" && envelope.Meta.Event != "deleted" {
			return bad("UNSUPPORTED_WEBHOOK_EVENT")
		}
		if _, e = connectionAccess(ctx, tx, tenant, connection, who, "sync"); e != nil {
			return e
		}
		result, e := tx.Exec(ctx, "INSERT INTO provider_webhook_receipts(tenant_id,subscription_id,body_sha256)VALUES($1,$2,$3)ON CONFLICT DO NOTHING", tenant, r.SubscriptionId, sha(r.Body))
		if e != nil || result.RowsAffected() == 0 {
			return e
		}
		_, e = tx.Exec(ctx, "INSERT INTO provider_sync_signals(tenant_id,connection_id,principal_id)VALUES($1,$2,$3)ON CONFLICT(connection_id)DO UPDATE SET generation=provider_sync_signals.generation+1,principal_id=excluded.principal_id", tenant, connection, who)
		return e
	})
	return &pb.ReceiveWebhookResponse{Accepted: err == nil}, err
}
func (s *Service) DrainSyncSignals(ctx context.Context) error {
	rows, err := s.Pool.Query(ctx, "SELECT tenant_id::text,connection_id::text,principal_id,generation,lease_token::text FROM claim_provider_sync_signals()")
	if err != nil {
		return err
	}
	type signal struct {
		Tenant, ID, Who, Token string
		Generation             int64
	}
	items := []signal{}
	for rows.Next() {
		var v signal
		if err = rows.Scan(&v.Tenant, &v.ID, &v.Who, &v.Generation, &v.Token); err != nil {
			rows.Close()
			return err
		}
		items = append(items, v)
	}
	rows.Close()
	if rows.Err() != nil {
		return rows.Err()
	}
	for _, v := range items {
		actorCtx := context.WithValue(ctx, principalKey{}, v.Who)
		_, e := s.RequestSync(actorCtx, &pb.SyncRequest{Scope: &pb.Scope{TenantId: v.Tenant}, ConnectionId: v.ID, IdempotencyKey: "webhook:" + v.ID + ":" + strconv.FormatInt(v.Generation, 10)})
		var ok bool
		if err = s.Pool.QueryRow(ctx, "SELECT settle_provider_sync_signal($1,$2,$3,$4)", v.ID, v.Token, v.Generation, e == nil).Scan(&ok); err != nil {
			return err
		}
	}
	return nil
}

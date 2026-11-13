package integrations

import (
	"context"
	"time"
)

func (s *Service) CleanupExpired(ctx context.Context) error {
	if s.Storage == nil {
		return nil
	}
	rows, err := s.Pool.Query(ctx, "SELECT tenant_id::text,file_id::text,object_key,lease_token::text FROM claim_expired_uploads(20)")
	if err != nil {
		return err
	}
	type item struct{ tenant, id, key, token string }
	items := []item{}
	for rows.Next() {
		var i item
		if err = rows.Scan(&i.tenant, &i.id, &i.key, &i.token); err != nil {
			rows.Close()
			return err
		}
		items = append(items, i)
	}
	rows.Close()
	if rows.Err() != nil {
		return rows.Err()
	}
	for _, i := range items {
		call, cancel := context.WithTimeout(ctx, 15*time.Second)
		deleted := s.Storage.Delete(call, i.key) == nil
		cancel()
		var settled bool
		if err = s.Pool.QueryRow(ctx, "SELECT settle_expired_upload($1,$2,$3)", i.id, i.token, deleted).Scan(&settled); err != nil {
			return err
		}
	}
	return nil
}

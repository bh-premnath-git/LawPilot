package workload

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
)

type database interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func CheckDatabaseRole(ctx context.Context, db database) error {
	var super, bypass, owner bool
	if err := db.QueryRow(ctx, "SELECT r.rolsuper,r.rolbypassrls,EXISTS(SELECT 1 FROM pg_class c WHERE c.relname IN('memberships','document_assets','ai_executions') AND pg_has_role(current_user,c.relowner,'MEMBER')) FROM pg_roles r WHERE r.rolname=current_user").Scan(&super, &bypass, &owner); err != nil {
		return err
	}
	if super || bypass || owner {
		return errors.New("privileged database roles cannot be runtime identities")
	}
	return nil
}

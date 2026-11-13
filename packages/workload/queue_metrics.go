package workload

import (
	"context"
	"github.com/jackc/pgx/v5"
)

type queryDatabase interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// Source function exposes fixed aggregate states only. Never export tenant IDs,
// source text, credentials or unbounded application labels.
func QueueMetrics(db queryDatabase) func(context.Context) (map[string]int64, error) {
	return func(ctx context.Context) (map[string]int64, error) {
		rows, err := db.Query(ctx, "SELECT metric,value FROM system_queue_metrics()")
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := map[string]int64{}
		for rows.Next() {
			var name string
			var value int64
			if err = rows.Scan(&name, &value); err != nil {
				return nil, err
			}
			if len(name) > 80 {
				continue
			}
			safe := true
			for _, c := range name {
				if c != '_' && (c < 'a' || c > 'z') {
					safe = false
				}
			}
			if safe {
				out[name] = value
			}
		}
		return out, rows.Err()
	}
}

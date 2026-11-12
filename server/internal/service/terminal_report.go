package service

import (
	"context"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type terminalReportKey struct{}

// WithTerminalReport is used only by authenticated executor terminal callbacks.
// Control-plane cancellation and stale-task recovery do not observe process exit.
func WithTerminalReport(ctx context.Context) context.Context {
	return context.WithValue(ctx, terminalReportKey{}, true)
}

func recordTerminalReport(ctx context.Context, q *db.Queries, id pgtype.UUID) error {
	if observed, _ := ctx.Value(terminalReportKey{}).(bool); !observed {
		return nil
	}
	_, err := q.RecordTaskTerminalReport(ctx, id)
	return err
}

package main

import (
	"context"
	"log/slog"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func runExternalTokenSweeper(ctx context.Context, queries *db.Queries) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			_, err := queries.DeleteExpiredExternalAgentTokens(sweepCtx)
			cancel()
			if err != nil && ctx.Err() == nil {
				slog.Warn("external agent token sweep failed", "error", err)
			}
		}
	}
}

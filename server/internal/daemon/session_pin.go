package daemon

import (
	"context"

	"github.com/google/uuid"
)

type sessionPinGenerationKey struct{}

func withSessionPinGeneration(ctx context.Context, generation string) context.Context {
	return context.WithValue(ctx, sessionPinGenerationKey{}, generation)
}

func sessionPinGenerationFromContext(ctx context.Context) string {
	generation, _ := ctx.Value(sessionPinGenerationKey{}).(string)
	return generation
}

func (d *Daemon) invalidateSessionPin(ctx context.Context, taskID string) (context.Context, error) {
	generation := sessionPinGenerationFromContext(ctx)
	if generation == "" {
		return ctx, nil
	}
	next := uuid.NewString()
	if err := d.client.InvalidateTaskSession(ctx, taskID, generation, next); err != nil {
		return ctx, err
	}
	return withSessionPinGeneration(ctx, next), nil
}

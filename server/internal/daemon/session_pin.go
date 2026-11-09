package daemon

import (
	"context"
	"sync/atomic"

	"github.com/google/uuid"
)

type sessionPinGenerationKey struct{}
type terminalTurnGenerationKey struct{}

// Session pins keep immutable attempt contexts; terminal reports follow the
// last acknowledged rotation of this execution, including a fresh retry.
type terminalTurnGeneration struct{ value atomic.Value }

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
	if current, ok := ctx.Value(terminalTurnGenerationKey{}).(*terminalTurnGeneration); ok {
		current.value.Store(next)
	}
	return withSessionPinGeneration(ctx, next), nil
}

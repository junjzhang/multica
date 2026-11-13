package daemon

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/multica-ai/multica/server/pkg/agent"
)

// Only a provider's explicit pre-input busy refusal permits repeating a strict
// attempt. The task, address, prompt, credentials, and pin generation stay fixed.
func (d *Daemon) executeTaskInput(ctx context.Context, backend agent.Backend, prompt string, opts agent.ExecOptions, taskLog *slog.Logger, taskID, codexHome string, msgSeq *atomic.Int32) (agent.Result, int32, error) {
	waitingReported := false
	for {
		result, tools, err := d.executeAndDrain(ctx, backend, prompt, opts, taskLog, taskID, codexHome, msgSeq)
		if err != nil || !opts.RequireResume || !result.ResumeRejectedTransient || !result.StrictResumeUnmet {
			return result, tools, err
		}
		if !waitingReported {
			waitingReported = true
			taskLog.Info("original provider session is busy; waiting without changing the task", "error", result.Error)
			_ = d.client.ReportProgress(ctx, taskID, "Waiting for the original provider session", 1, 2)
		}
		if err := sleepWithContext(ctx, envRootBusyRetryInterval); err != nil {
			return result, tools, err
		}
	}
}

package backgroundwait

import (
	"context"
	"crypto/subtle"
	"os/exec"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/processtree"
)

func validSecret(got, want string) bool {
	return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func executeCommand(ctx context.Context, cmd *exec.Cmd) int {
	err := processtree.Run(ctx, cmd, time.Second)
	if err == nil {
		return 0
	}
	if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() > 0 {
		return cmd.ProcessState.ExitCode()
	}
	return 125
}

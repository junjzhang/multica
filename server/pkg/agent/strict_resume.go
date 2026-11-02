package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrResumeRequired reports that a run asked for a strict resume
// (ExecOptions.RequireResume) but the backend could not prove it would
// continue exactly the requested session before sending the task prompt.
//
// It is deliberately a single sentinel rather than a result state machine.
// Callers that requested strict resume must treat any wrapped error as
// non-retryable with a fresh session: starting over is exactly what the option
// forbids. A concrete backend may also surface the failure through the existing
// Result.ResumeRejected signal once it has a Session.
var ErrResumeRequired = errors.New("agent: strict resume requirement not met")

// requireStrictResume validates the caller-owned half of the strict contract.
// Provider-specific proof happens after the resume RPC, immediately before the
// first prompt or turn is sent.
func requireStrictResume(opts ExecOptions) error {
	if !opts.RequireResume {
		return nil
	}
	if strings.TrimSpace(opts.ResumeSessionID) == "" {
		return fmt.Errorf("%w: ResumeSessionID is required", ErrResumeRequired)
	}
	return nil
}

// Strict-resume support follows each backend's real resume contract, not the
// presence of a separate preflight call:
//
//   - ACP (Hermes, TraeCLI, Kimi, Dim, Qoder CN, Reasonix, Kiro, MCode, QwenPaw,
//     ZeroClaw, Grok, Muse): session/load or session/resume completes before
//     session/prompt and reports the served id. checkStrictACPResumedSessionID
//     rejects an empty or changed id. MCode additionally needs loadSession.
//   - Codex: thread/resume returns the thread id before turn/start.
//   - DSH: the runtime resumes exactly the non-empty resume_session_id before
//     followup and reports session{resumed:true, session_id} first; see
//     dsh.go.
//   - Claude: --resume loads the named transcript before the turn. The adapter
//     pre-checks the same transcript file so a missing session fails before
//     launch. File existence only moves that failure earlier; the guarantee
//     that the prompt lands in the requested session is the CLI's native
//     resume, and a CLI that ignored the id would already have the prompt.
//   - Pi: --session points at the persisted transcript, which the adapter
//     stats (and locks) before launch. Same boundary as Claude: the native
//     load is the guarantee, the stat only proves the file exists.
//   - OpenCode: `opencode export <id>` proves the id exists, then --session
//     loads it before the turn. Export is existence, not replay; a CLI version
//     that silently rebound instead would not be caught before the prompt.
//   - antigravity, codearts, codebuddy, copilot, cursor, deveco, openclaw and
//     qwen only pass a resume flag (or --conversation) and infer the outcome
//     from the final stream/stderr. None has a verified pre-prompt existence or
//     identity proof, so strict mode stays fail-closed here rather than
//     claiming support from local files the CLI may ignore.
//
// requireStrictResumeSupport is the fail-closed guard for the last group. It
// returns nil when strict mode is off, so ordinary fallback behaviour is
// untouched.
func requireStrictResumeSupport(backend string, opts ExecOptions) error {
	if err := requireStrictResume(opts); err != nil {
		return err
	}
	if !opts.RequireResume {
		return nil
	}
	return fmt.Errorf("%w: %s cannot confirm the requested session before sending the prompt", ErrResumeRequired, backend)
}

// requireExactResumedSessionID checks the identity a backend observed at its
// resume boundary. Empty means the runtime gave no evidence; a different id
// means it silently rebound to another session. Neither may reach a prompt in
// strict mode.
func requireExactResumedSessionID(opts ExecOptions, confirmed, source string) error {
	if !opts.RequireResume {
		return nil
	}
	requested := strings.TrimSpace(opts.ResumeSessionID)
	if confirmed == "" {
		return fmt.Errorf("%w: %s did not confirm session %q", ErrResumeRequired, source, requested)
	}
	if confirmed != requested {
		return fmt.Errorf("%w: %s confirmed session %q, requested %q", ErrResumeRequired, source, confirmed, requested)
	}
	return nil
}

// checkStrictACPResumedSessionID is the shared ACP resume validation. Hermes
// reports the served ACP session under
// _meta.hermes.sessionProvenance.acpSessionId rather than a top-level sessionId,
// so callers for that backend pass provenance=true.
func checkStrictACPResumedSessionID(opts ExecOptions, response json.RawMessage, provenance bool) error {
	if !opts.RequireResume {
		return nil
	}
	confirmed := extractACPSessionID(response)
	if confirmed == "" && provenance {
		confirmed = extractACPProvenanceSessionID(response)
	}
	return requireExactResumedSessionID(opts, confirmed, "ACP resume")
}

// strictResumeFailureResult is the terminal result for a run whose resume could
// not be confirmed, so it must not receive the task prompt. ResumeRejected is
// positive evidence that the requested session cannot continue, while
// ErrResumeRequired remains available to callers that receive a synchronous
// Execute error.
func strictResumeFailureResult(err error, start time.Time) Result {
	return Result{
		Status:         "failed",
		Error:          err.Error(),
		DurationMs:     time.Since(start).Milliseconds(),
		ResumeRejected: true,
	}
}

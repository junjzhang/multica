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
//     ZeroClaw, Grok): session/load or session/resume is addressed by the
//     client-supplied session id and completes before session/prompt. The
//     response is allowed to be empty — ACP's LoadSession/ResumeSession result
//     has no session-id field — so an empty result is confirmation that the
//     requested id was accepted, provided the adapter reports a missing
//     session as an RPC error (the isACPSessionNotFound /
//     classifyACPResumeFailure path these backends already use). A response
//     that names a different id is a silent rebind and is rejected.
//     Hermes is the exception: it answers a missing session by silently
//     creating a new one, so it must report the served ACP id under
//     _meta.hermes.sessionProvenance.acpSessionId and strict mode requires an
//     exact match; missing provenance stays fail-closed.
//   - Muse: its MSP session/resume response names the served session, so
//     strict mode requires that id to equal the requested one.
//   - Codex: thread/resume returns the thread id before turn/start.
//   - DSH: the protocol-v1 execute frame carries the prompt and the non-empty
//     resume_session_id together, and the verified Multica DSH runtime resumes
//     exactly that session before followup, then reports
//     session{resumed:true, session_id} first; see dsh.go. That guarantee is
//     scoped to the built-in runtime (Config.BuiltinRuntime with no launch
//     prefix); a custom runtime profile declaring the same protocol family is
//     rejected before launch because there is no evidence it honours the same
//     atomic resume contract.
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

// checkStrictACPResume validates a client-addressed ACP resume whose success
// result is allowed to be empty. The request itself names the session, and the
// adapters reject an unknown one through classifyACPResumeFailure before this
// point, so a successful empty result confirms the requested id rather than
// leaving it unproven. A non-empty response names the session the runtime
// actually served; anything but the requested id is a silent rebind and must
// not reach the prompt. rpc is only used in the failure diagnostic.
func checkStrictACPResume(opts ExecOptions, response json.RawMessage, rpc string) error {
	if !opts.RequireResume {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(response, &object); err != nil || object == nil {
		return fmt.Errorf("%w: %s returned no usable resume result", ErrResumeRequired, rpc)
	}
	confirmed := strings.TrimSpace(extractACPSessionID(response))
	if confirmed == "" {
		return nil
	}
	return requireExactResumedSessionID(opts, confirmed, rpc)
}

// checkStrictHermesResume is the Hermes-specific strict check. Hermes answers
// session/resume for a session it no longer holds by silently creating a new
// one, so neither an empty result nor a top-level sessionId is evidence. Only
// the ACP id on _meta.hermes.sessionProvenance.acpSessionId identifies the
// session the runtime actually served; without it strict mode cannot prove the
// requested conversation was restored and fails closed.
func checkStrictHermesResume(opts ExecOptions, response json.RawMessage) error {
	if !opts.RequireResume {
		return nil
	}
	confirmed := strings.TrimSpace(extractACPProvenanceSessionID(response))
	if confirmed == "" {
		return fmt.Errorf("%w: hermes session/resume reported no session provenance for %q", ErrResumeRequired, opts.ResumeSessionID)
	}
	return requireExactResumedSessionID(opts, confirmed, "hermes session/resume")
}

// strictResumeFailureResult is the terminal result for a run whose resume could
// not be confirmed, so it must not receive the task prompt. ResumeRejected is
// positive evidence that the requested session cannot continue, while
// ErrResumeRequired remains available to callers that receive a synchronous
// Execute error.
func strictResumeFailureResult(err error, start time.Time) Result {
	return Result{
		Status:            "failed",
		Error:             err.Error(),
		DurationMs:        time.Since(start).Milliseconds(),
		ResumeRejected:    true,
		StrictResumeUnmet: true,
	}
}

// markStrictResumeResult marks a strict run's terminal result without changing
// its status. Resume errors that surface as a normal Result (for example a
// session-not-found RPC failure) follow this path so the daemon can veto a
// fresh retry even though ResumeRejected may be set. Ordinary runs are left
// untouched.
func markStrictResumeResult(result Result, opts ExecOptions) Result {
	if opts.RequireResume {
		result.StrictResumeUnmet = true
	}
	return result
}

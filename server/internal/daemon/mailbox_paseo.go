package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// paseoCommandRunner isolates process execution so the adapter contract can be
// tested without installing Paseo or touching a live host.
type paseoCommandRunner func(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)

type paseoCliTransport struct {
	bin     string
	timeout time.Duration
	run     paseoCommandRunner
}

const defaultPaseoDeliveryTimeout = 20 * time.Second

func newPaseoCliTransport() *paseoCliTransport {
	return &paseoCliTransport{bin: "paseo", timeout: defaultPaseoDeliveryTimeout, run: runPaseoCommand}
}

func runPaseoCommand(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return []byte(stdout.String()), []byte(stderr.String()), err
}

type paseoSendResult struct {
	AgentID string `json:"agentId"`
	Status  string `json:"status"`
}

type paseoErrorEnvelope struct {
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Deliver performs the frozen atomic if-idle send:
//
//	paseo send <session-id> --if-idle --message-id <mailbox-id>
//	  --prompt-file <private-file> --no-wait --json
//
// The typed error result is what keeps a busy host (known no effect) distinct
// from an ambiguous accepted outcome. There is deliberately no fallback to an
// ordinary send.
func (t *paseoCliTransport) Deliver(ctx context.Context, session ExternalSession, messageID, content string) (string, error) {
	if t == nil || t.bin == "" {
		return "", &MailboxDeliveryError{
			Kind:    MailboxDeliveryUnsupported,
			Code:    mailboxErrorUnsupported,
			Message: "paseo CLI is not configured; install Paseo before enabling external-session delivery",
		}
	}
	if strings.TrimSpace(session.SessionID) == "" {
		return "", &MailboxDeliveryError{
			Kind:    MailboxDeliveryFailed,
			Code:    "missing_session_id",
			Message: "mailbox claim has no external session id",
		}
	}
	deliveryCtx := ctx
	if t.timeout > 0 {
		var cancel context.CancelFunc
		deliveryCtx, cancel = context.WithTimeout(ctx, t.timeout)
		defer cancel()
	}
	if err := t.verifyHost(deliveryCtx, session.HostID); err != nil {
		return "", err
	}

	promptFile, err := os.CreateTemp("", "multica-mailbox-*.md")
	if err != nil {
		return "", &MailboxDeliveryError{
			Kind:    MailboxDeliveryUnavailable,
			Code:    "prompt_file_unavailable",
			Message: "could not create a private prompt file for mailbox delivery",
			Err:     err,
		}
	}
	promptPath := promptFile.Name()
	defer os.Remove(promptPath)
	if _, err := promptFile.WriteString(content); err != nil {
		promptFile.Close()
		return "", &MailboxDeliveryError{
			Kind:    MailboxDeliveryUnavailable,
			Code:    "prompt_file_unavailable",
			Message: "could not write the private prompt file for mailbox delivery",
			Err:     err,
		}
	}
	if err := promptFile.Close(); err != nil {
		return "", &MailboxDeliveryError{
			Kind:    MailboxDeliveryUnavailable,
			Code:    "prompt_file_unavailable",
			Message: "could not close the private prompt file for mailbox delivery",
			Err:     err,
		}
	}
	args := []string{
		"send", session.SessionID,
		"--if-idle",
		"--message-id", messageID,
		"--prompt-file", promptPath,
		"--no-wait",
		"--json",
	}
	stdout, stderr, runErr := t.run(deliveryCtx, t.bin, args...)
	if runErr != nil {
		return "", classifyPaseoRunError(deliveryCtx, stderr, runErr)
	}

	var result paseoSendResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &result); err != nil {
		return "", &MailboxDeliveryError{
			Kind:    MailboxDeliveryOutcomeUnknown,
			Code:    mailboxErrorOutcomeUnknown,
			Message: "paseo exited successfully but returned an unrecognized result; delivery is not retried automatically",
			Err:     err,
		}
	}
	if result.Status != "sent" {
		return "", &MailboxDeliveryError{
			Kind:    MailboxDeliveryOutcomeUnknown,
			Code:    mailboxErrorOutcomeUnknown,
			Message: fmt.Sprintf("paseo returned status %q for an if-idle send; delivery is not retried automatically", result.Status),
		}
	}
	if result.AgentID != session.SessionID {
		return "", &MailboxDeliveryError{
			Kind:    MailboxDeliveryOutcomeUnknown,
			Code:    mailboxErrorOutcomeUnknown,
			Message: "paseo acknowledged a different agent id than the registered session; delivery is not retried automatically",
		}
	}
	return fmt.Sprintf("paseo:%s:%s", session.SessionID, messageID), nil
}

func (t *paseoCliTransport) verifyHost(ctx context.Context, hostID string) error {
	if hostID == "" {
		return &MailboxDeliveryError{Kind: MailboxDeliveryFailed, Code: "missing_host_id", Message: "external session has no Paseo host identity"}
	}
	stdout, _, err := t.run(ctx, t.bin, "status", "--json")
	if err != nil {
		return &MailboxDeliveryError{Kind: MailboxDeliveryUnavailable, Code: "paseo_host_probe_failed", Message: "Paseo host could not be verified before delivery", Err: err}
	}
	var status struct {
		ServerID string `json:"serverId"`
	}
	if err := json.Unmarshal(stdout, &status); err != nil || status.ServerID == "" {
		return &MailboxDeliveryError{Kind: MailboxDeliveryUnsupported, Code: "paseo_host_identity_unavailable", Message: "Paseo status did not provide a server identity; no request was sent"}
	}
	if status.ServerID != hostID {
		return &MailboxDeliveryError{Kind: MailboxDeliveryFailed, Code: "paseo_host_mismatch", Message: "registered Paseo host differs from the local daemon; no request was sent"}
	}
	return nil
}

func classifyPaseoRunError(ctx context.Context, stderr []byte, runErr error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &MailboxDeliveryError{
			Kind:    MailboxDeliveryOutcomeUnknown,
			Code:    "delivery_timeout",
			Message: "paseo delivery timed out; the outcome is not retried automatically",
			Err:     ctx.Err(),
		}
	}
	if ctx.Err() != nil {
		// The host may have accepted the request before the process was
		// stopped. Treat shutdown as an ambiguous accepted outcome rather than
		// a known no-effect failure so it is not retried automatically.
		return &MailboxDeliveryError{
			Kind:    MailboxDeliveryOutcomeUnknown,
			Code:    "delivery_interrupted",
			Message: "daemon stopped while host delivery was in flight; the outcome is not retried automatically",
			Err:     ctx.Err(),
		}
	}

	if code, ok := parsePaseoErrorCode(stderr); ok {
		switch code {
		case mailboxErrorAgentBusy:
			return &MailboxDeliveryError{
				Kind:    MailboxDeliveryBusy,
				Code:    code,
				Message: "paseo session is busy; the request was not sent and will be retried",
			}
		case mailboxErrorOutcomeUnknown:
			return &MailboxDeliveryError{
				Kind:    MailboxDeliveryOutcomeUnknown,
				Code:    code,
				Message: "paseo could not confirm whether the request was accepted; delivery is not retried automatically",
			}
		case "DAEMON_UNREACHABLE", "DAEMON_NOT_RUNNING", "DAEMON_REPLACED":
			return &MailboxDeliveryError{
				Kind:    MailboxDeliveryUnavailable,
				Code:    code,
				Message: "paseo host is unreachable; the request stays queued and will be retried",
			}
		case "DAEMON_UPDATE_REQUIRED":
			return &MailboxDeliveryError{
				Kind:    MailboxDeliveryUnsupported,
				Code:    code,
				Message: "paseo host requires an upgrade before it can accept atomic if-idle delivery",
			}
		case "AGENT_NOT_FOUND":
			return &MailboxDeliveryError{
				Kind:    MailboxDeliveryFailed,
				Code:    code,
				Message: "the registered paseo session no longer exists; fix the session binding instead of creating a replacement session",
			}
		default:
			return &MailboxDeliveryError{
				Kind:    MailboxDeliveryFailed,
				Code:    code,
				Message: fmt.Sprintf("paseo rejected mailbox delivery with code %s", code),
			}
		}
	}

	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		return &MailboxDeliveryError{
			Kind:    MailboxDeliveryUnsupported,
			Code:    "paseo_cli_unavailable",
			Message: "the paseo CLI could not be started; install Paseo and confirm `paseo` is on PATH",
			Err:     runErr,
		}
	}

	if unsupportedIfIdleCLI(stderr) {
		return &MailboxDeliveryError{
			Kind:    MailboxDeliveryUnsupported,
			Code:    mailboxErrorUnsupported,
			Message: "the installed paseo CLI does not support --if-idle; upgrade Paseo (no fallback to ordinary send was attempted)",
		}
	}

	return &MailboxDeliveryError{
		Kind:    MailboxDeliveryOutcomeUnknown,
		Code:    mailboxErrorOutcomeUnknown,
		Message: "paseo returned an unrecognized failure; delivery is not retried automatically",
		Err:     runErr,
	}
}

func parsePaseoErrorCode(stderr []byte) (string, bool) {
	var envelope paseoErrorEnvelope
	if err := json.Unmarshal(stderr, &envelope); err != nil || envelope.Error == nil {
		return "", false
	}
	code := strings.TrimSpace(envelope.Error.Code)
	return code, code != ""
}

// unsupportedIfIdleCLI recognizes the argument-parser rejection of a CLI that
// predates the frozen adapter contract. It is dependency detection, not a
// delivery-outcome heuristic: an unknown option is rejected before any request
// is sent, so treating it as a known no-effect failure is safe.
func unsupportedIfIdleCLI(stderr []byte) bool {
	text := strings.ToLower(string(stderr))
	if !strings.Contains(text, "--if-idle") {
		return false
	}
	return strings.Contains(text, "unknown option") || strings.Contains(text, "unrecognized option")
}

var _ MailboxHostTransport = (*paseoCliTransport)(nil)

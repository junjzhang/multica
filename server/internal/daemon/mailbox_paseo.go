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

	"github.com/multica-ai/multica/server/internal/delivery"
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
// The delivery.HostError result is what keeps a busy host (known no effect)
// distinct from an ambiguous accepted outcome. There is deliberately no
// fallback to an ordinary send.
func (t *paseoCliTransport) Deliver(ctx context.Context, session ExternalSession, messageID, content string) (string, error) {
	if t == nil || t.bin == "" {
		return "", &delivery.HostError{
			Kind:    delivery.HostUnsupported,
			Code:    mailboxErrorUnsupported,
			Message: "paseo CLI is not configured; install Paseo before enabling external-session delivery",
		}
	}
	if strings.TrimSpace(session.SessionID) == "" {
		return "", &delivery.HostError{
			Kind:    delivery.HostRejected,
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
		return "", &delivery.HostError{
			Kind:    delivery.HostUnavailable,
			Code:    "prompt_file_unavailable",
			Message: "could not create a private prompt file for mailbox delivery",
			Err:     err,
		}
	}
	promptPath := promptFile.Name()
	defer os.Remove(promptPath)
	if _, err := promptFile.WriteString(content); err != nil {
		promptFile.Close()
		return "", &delivery.HostError{
			Kind:    delivery.HostUnavailable,
			Code:    "prompt_file_unavailable",
			Message: "could not write the private prompt file for mailbox delivery",
			Err:     err,
		}
	}
	if err := promptFile.Close(); err != nil {
		return "", &delivery.HostError{
			Kind:    delivery.HostUnavailable,
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
		return "", &delivery.HostError{
			Kind:    delivery.HostOutcomeUnknown,
			Code:    mailboxErrorOutcomeUnknown,
			Message: "paseo exited successfully but returned an unrecognized result; delivery is not retried automatically",
			Err:     err,
		}
	}
	if result.Status != "sent" {
		return "", &delivery.HostError{
			Kind:    delivery.HostOutcomeUnknown,
			Code:    mailboxErrorOutcomeUnknown,
			Message: fmt.Sprintf("paseo returned status %q for an if-idle send; delivery is not retried automatically", result.Status),
		}
	}
	if result.AgentID != session.SessionID {
		return "", &delivery.HostError{
			Kind:    delivery.HostOutcomeUnknown,
			Code:    mailboxErrorOutcomeUnknown,
			Message: "paseo acknowledged a different agent id than the registered session; delivery is not retried automatically",
		}
	}
	return fmt.Sprintf("paseo:%s:%s", session.SessionID, messageID), nil
}

func (t *paseoCliTransport) verifyHost(ctx context.Context, hostID string) error {
	if hostID == "" {
		return &delivery.HostError{Kind: delivery.HostRejected, Code: "missing_host_id", Message: "external session has no Paseo host identity"}
	}
	stdout, _, err := t.run(ctx, t.bin, "status", "--json")
	if err != nil {
		return &delivery.HostError{Kind: delivery.HostUnavailable, Code: "paseo_host_probe_failed", Message: "Paseo host could not be verified before delivery", Err: err}
	}
	var status struct {
		ServerID string `json:"serverId"`
	}
	if err := json.Unmarshal(stdout, &status); err != nil || status.ServerID == "" {
		return &delivery.HostError{Kind: delivery.HostUnsupported, Code: "paseo_host_identity_unavailable", Message: "Paseo status did not provide a server identity; no request was sent"}
	}
	if status.ServerID != hostID {
		return &delivery.HostError{Kind: delivery.HostRejected, Code: "paseo_host_mismatch", Message: "registered Paseo host differs from the local daemon; no request was sent"}
	}
	return nil
}

func classifyPaseoRunError(ctx context.Context, stderr []byte, runErr error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &delivery.HostError{
			Kind:    delivery.HostOutcomeUnknown,
			Code:    "delivery_timeout",
			Message: "paseo delivery timed out; the outcome is not retried automatically",
			Err:     ctx.Err(),
		}
	}
	if ctx.Err() != nil {
		// The host may have accepted the request before the process was
		// stopped. Treat shutdown as an ambiguous accepted outcome rather than
		// a known no-effect failure so it is not retried automatically.
		return &delivery.HostError{
			Kind:    delivery.HostOutcomeUnknown,
			Code:    "delivery_interrupted",
			Message: "daemon stopped while host delivery was in flight; the outcome is not retried automatically",
			Err:     ctx.Err(),
		}
	}

	if code, ok := parsePaseoErrorCode(stderr); ok {
		switch code {
		case mailboxErrorAgentBusy:
			return &delivery.HostError{
				Kind:    delivery.HostBusy,
				Code:    code,
				Message: "paseo session is busy; the request was not sent and will be retried",
			}
		case mailboxErrorOutcomeUnknown:
			return &delivery.HostError{
				Kind:    delivery.HostOutcomeUnknown,
				Code:    code,
				Message: "paseo could not confirm whether the request was accepted; delivery is not retried automatically",
			}
		case "DAEMON_UNREACHABLE", "DAEMON_NOT_RUNNING", "DAEMON_REPLACED":
			return &delivery.HostError{
				Kind:    delivery.HostUnavailable,
				Code:    code,
				Message: "paseo host is unreachable; the request stays queued and will be retried",
			}
		case "DAEMON_UPDATE_REQUIRED":
			return &delivery.HostError{
				Kind:    delivery.HostUnsupported,
				Code:    code,
				Message: "paseo host requires an upgrade before it can accept atomic if-idle delivery",
			}
		case "AGENT_NOT_FOUND":
			return &delivery.HostError{
				Kind:    delivery.HostRejected,
				Code:    code,
				Message: "the registered paseo session no longer exists; fix the session binding instead of creating a replacement session",
			}
		default:
			return &delivery.HostError{
				Kind:    delivery.HostRejected,
				Code:    code,
				Message: fmt.Sprintf("paseo rejected mailbox delivery with code %s", code),
			}
		}
	}

	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		return &delivery.HostError{
			Kind:    delivery.HostUnsupported,
			Code:    "paseo_cli_unavailable",
			Message: "the paseo CLI could not be started; install Paseo and confirm `paseo` is on PATH",
			Err:     runErr,
		}
	}

	if unsupportedIfIdleCLI(stderr) {
		return &delivery.HostError{
			Kind:    delivery.HostUnsupported,
			Code:    mailboxErrorUnsupported,
			Message: "the installed paseo CLI does not support --if-idle; upgrade Paseo (no fallback to ordinary send was attempted)",
		}
	}

	return &delivery.HostError{
		Kind:    delivery.HostOutcomeUnknown,
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

// paseoReceiptResult is the frozen `paseo agent receipt --json` document. The
// lookup is read-only: it never sends, resumes, creates or archives a session.
type paseoReceiptResult struct {
	AgentID   string `json:"agentId"`
	MessageID string `json:"messageId"`
	Status    string `json:"status"`
}

// LookupReceipt reads the host's receipt for one stable message id. Missing
// receipts are reported as absent, which the worker treats as unconfirmed
// rather than as proof that the request never ran.
func (t *paseoCliTransport) LookupReceipt(ctx context.Context, session ExternalSession, messageID string) (delivery.Receipt, error) {
	if t == nil || t.bin == "" {
		return delivery.Receipt{}, &delivery.HostError{
			Kind:    delivery.HostUnsupported,
			Code:    "paseo_cli_unavailable",
			Message: "paseo CLI is not configured, so delivery receipts cannot be read",
		}
	}
	if strings.TrimSpace(session.SessionID) == "" {
		return delivery.Receipt{}, &delivery.HostError{
			Kind:    delivery.HostRejected,
			Code:    "missing_session_id",
			Message: "delivery claim has no external session id",
		}
	}
	lookupCtx := ctx
	if t.timeout > 0 {
		var cancel context.CancelFunc
		lookupCtx, cancel = context.WithTimeout(ctx, t.timeout)
		defer cancel()
	}
	// The host probe is the same read-only identity check the send path uses:
	// a receipt from another machine must never be read as this session's.
	if err := t.verifyHost(lookupCtx, session.HostID); err != nil {
		return delivery.Receipt{}, err
	}
	stdout, stderr, runErr := t.run(lookupCtx, t.bin, "agent", "receipt", session.SessionID, messageID, "--json")
	if runErr != nil {
		return delivery.Receipt{}, classifyPaseoReceiptError(lookupCtx, stderr, runErr)
	}
	var result paseoReceiptResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &result); err != nil {
		return delivery.Receipt{}, &delivery.HostError{
			Kind:    delivery.HostUnavailable,
			Code:    "paseo_receipt_unrecognized",
			Message: "paseo returned an unrecognized receipt document, so the outcome stays unconfirmed",
			Err:     err,
		}
	}
	if result.AgentID != session.SessionID {
		return delivery.Receipt{}, &delivery.HostError{
			Kind:    delivery.HostRejected,
			Code:    "paseo_receipt_identity_mismatch",
			Message: "paseo returned a receipt for a different agent identity; it is not used",
		}
	}
	if result.MessageID != messageID {
		return delivery.Receipt{}, &delivery.HostError{
			Kind:    delivery.HostRejected,
			Code:    "paseo_receipt_message_mismatch",
			Message: "paseo returned a receipt for a different message id; it is not used",
		}
	}
	switch result.Status {
	case "accepted":
		return delivery.Receipt{
			Status:    delivery.ReceiptAccepted,
			Reference: fmt.Sprintf("paseo:%s:%s:accepted", session.SessionID, messageID),
		}, nil
	case "pending":
		return delivery.Receipt{Status: delivery.ReceiptPending}, nil
	case "absent":
		return delivery.Receipt{Status: delivery.ReceiptAbsent}, nil
	default:
		return delivery.Receipt{}, &delivery.HostError{
			Kind:    delivery.HostUnavailable,
			Code:    "paseo_receipt_unrecognized",
			Message: fmt.Sprintf("paseo returned unrecognized receipt status %q", result.Status),
		}
	}
}

// classifyPaseoReceiptError maps the CLI contract's receipt failures. A host
// that predates the lookup is a capability gap, not an absent receipt.
func classifyPaseoReceiptError(ctx context.Context, stderr []byte, runErr error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &delivery.HostError{
			Kind:    delivery.HostUnavailable,
			Code:    "receipt_lookup_timeout",
			Message: "paseo receipt lookup timed out; the outcome stays unconfirmed",
			Err:     ctx.Err(),
		}
	}
	if ctx.Err() != nil {
		return &delivery.HostError{
			Kind:    delivery.HostUnavailable,
			Code:    "receipt_lookup_interrupted",
			Message: "daemon stopped during receipt lookup; the outcome stays unconfirmed",
			Err:     ctx.Err(),
		}
	}
	if code, ok := parsePaseoErrorCode(stderr); ok {
		switch code {
		case "DAEMON_UPDATE_REQUIRED", "unsupported_operation":
			return &delivery.HostError{
				Kind:    delivery.HostUnsupported,
				Code:    code,
				Message: "the installed paseo host cannot look up delivery receipts; upgrade Paseo before relying on recovery",
			}
		case "agent_request_receipt_identity_mismatch":
			return &delivery.HostError{
				Kind:    delivery.HostRejected,
				Code:    code,
				Message: "paseo refused receipt lookup for a different agent identity",
			}
		default:
			return &delivery.HostError{
				Kind:    delivery.HostUnavailable,
				Code:    code,
				Message: fmt.Sprintf("paseo receipt lookup failed with code %s; the outcome stays unconfirmed", code),
			}
		}
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		return &delivery.HostError{
			Kind:    delivery.HostUnsupported,
			Code:    "paseo_cli_unavailable",
			Message: "the paseo CLI could not be started, so delivery receipts cannot be read",
			Err:     runErr,
		}
	}
	if receiptLookupUnsupportedCLI(stderr) {
		return &delivery.HostError{
			Kind:    delivery.HostUnsupported,
			Code:    "paseo_receipt_unsupported",
			Message: "the installed paseo CLI has no receipt command; upgrade Paseo before relying on recovery",
		}
	}
	return &delivery.HostError{
		Kind:    delivery.HostUnavailable,
		Code:    "paseo_receipt_lookup_failed",
		Message: "paseo returned an unrecognized receipt lookup failure; the outcome stays unconfirmed",
		Err:     runErr,
	}
}

// receiptLookupUnsupportedCLI recognises a CLI that predates the receipt
// command. The argument parser rejects the command before any lookup happens,
// so this is dependency detection, not a receipt heuristic.
func receiptLookupUnsupportedCLI(stderr []byte) bool {
	// This lookup only invokes `paseo agent receipt`, so any argument-parser
	// rejection means the installed CLI predates the command. It happens before
	// a lookup, so it is a capability gap, never a receipt status.
	text := strings.ToLower(string(stderr))
	return strings.Contains(text, "unknown command") ||
		strings.Contains(text, "unknown option") ||
		strings.Contains(text, "unrecognized command") ||
		strings.Contains(text, "unrecognized option")
}

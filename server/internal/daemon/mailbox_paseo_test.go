package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/delivery"
)

func paseoTestSession() ExternalSession {
	return ExternalSession{
		Provider:    "paseo",
		HostID:      "host-1",
		SessionID:   "paseo-agent-1",
		DisplayName: "Reviewer",
		DaemonID:    "daemon-1",
	}
}

func TestPaseoDeliveryRejectsDifferentHostBeforeSending(t *testing.T) {
	sent := false
	transport := &paseoCliTransport{bin: "paseo", run: func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
		if args[0] == "status" {
			return []byte(`{"serverId":"another-host"}`), nil, nil
		}
		sent = true
		return []byte(`{"agentId":"paseo-agent-1","status":"sent"}`), nil, nil
	}}
	_, err := transport.Deliver(context.Background(), paseoTestSession(), "mailbox-1", "private request")
	var deliveryErr *MailboxDeliveryError
	if !errors.As(err, &deliveryErr) || deliveryErr.Code != "paseo_host_mismatch" || sent {
		t.Fatalf("wrong host must reject before send: error=%v sent=%v", err, sent)
	}
}

func TestPaseoHostProbeFailureRemainsRetryable(t *testing.T) {
	sent := false
	transport := &paseoCliTransport{bin: "paseo", run: func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
		if args[0] == "status" {
			return nil, []byte(`{"error":{"code":"DAEMON_REQUEST_TIMEOUT","message":"timeout"}}`), &exec.ExitError{}
		}
		sent = true
		return nil, nil, nil
	}}
	_, err := transport.Deliver(context.Background(), paseoTestSession(), "mailbox-1", "request")
	var failure *MailboxDeliveryError
	if !errors.As(err, &failure) || failure.Kind != MailboxDeliveryUnavailable || sent {
		t.Fatalf("read-only probe failure must retry without sending: err=%v sent=%v", err, sent)
	}
	// The worker's host contract must see the same known no-effect result, not
	// an ambiguous accepted outcome.
	var hostErr *delivery.HostError
	if !errors.As(hostErrorFromMailbox(err), &hostErr) || hostErr.Kind != delivery.HostUnavailable {
		t.Fatalf("probe failure mapped to %v, want HostUnavailable", hostErrorFromMailbox(err))
	}
}

func paseoRunner(t *testing.T, stdout, stderr string, runErr error) paseoCommandRunner {
	t.Helper()
	return func(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
		if name != "paseo" {
			t.Errorf("command = %q, want paseo", name)
		}
		if args[0] == "status" {
			return []byte(`{"serverId":"host-1"}`), nil, nil
		}
		return []byte(stdout), []byte(stderr), runErr
	}
}

func TestPaseoDeliveryUsesFrozenIfIdleContract(t *testing.T) {
	var promptPath string
	var promptBody string
	run := func(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
		if name != "paseo" {
			t.Errorf("command = %q", name)
		}
		if args[0] == "status" {
			return []byte(`{"serverId":"host-1"}`), nil, nil
		}
		wantPrefix := []string{"send", "paseo-agent-1", "--if-idle", "--message-id", "mailbox-1", "--prompt-file"}
		if len(args) < len(wantPrefix)+3 {
			t.Fatalf("args = %v", args)
		}
		for i, want := range wantPrefix {
			if args[i] != want {
				t.Fatalf("arg %d = %q, want %q (args=%v)", i, args[i], want, args)
			}
		}
		promptPath = args[6]
		if args[7] != "--no-wait" || args[8] != "--json" {
			t.Fatalf("tail args = %v", args[6:])
		}
		info, err := os.Stat(promptPath)
		if err != nil {
			t.Fatalf("prompt file: %v", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("prompt file mode = %o, want 600", info.Mode().Perm())
		}
		data, err := os.ReadFile(promptPath)
		if err != nil {
			t.Fatal(err)
		}
		promptBody = string(data)
		return []byte(`{"agentId":"paseo-agent-1","status":"sent","message":"Message sent, not waiting for completion"}`), nil, nil
	}
	transport := &paseoCliTransport{bin: "paseo", run: run}

	receipt, err := transport.Deliver(context.Background(), paseoTestSession(), "mailbox-1", "Please review.\n")
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if receipt != "paseo:paseo-agent-1:mailbox-1" {
		t.Fatalf("receipt = %q", receipt)
	}
	if promptBody != "Please review.\n" {
		t.Fatalf("prompt body = %q", promptBody)
	}
	if _, err := os.Stat(promptPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prompt file still exists after delivery: %v", err)
	}
}

func TestPaseoDeliveryErrorsAreTypedAndContentFree(t *testing.T) {
	const secretPrompt = "do not leak this prompt"
	cases := []struct {
		name     string
		stdout   string
		stderr   string
		runErr   error
		wantKind MailboxDeliveryErrorKind
		wantCode string
	}{
		{
			name:     "busy is known no effect",
			stderr:   `{"error":{"code":"agent_busy","message":"Agent is running"}}`,
			runErr:   &exec.ExitError{},
			wantKind: MailboxDeliveryBusy,
			wantCode: mailboxErrorAgentBusy,
		},
		{
			name:     "unknown outcome is not retried",
			stderr:   `{"error":{"code":"agent_request_outcome_unknown","message":"outcome unknown"}}`,
			runErr:   &exec.ExitError{},
			wantKind: MailboxDeliveryOutcomeUnknown,
			wantCode: mailboxErrorOutcomeUnknown,
		},
		{
			name:     "host unreachable is retryable",
			stderr:   `{"error":{"code":"DAEMON_UNREACHABLE","message":"Cannot connect"}}`,
			runErr:   &exec.ExitError{},
			wantKind: MailboxDeliveryUnavailable,
			wantCode: "DAEMON_UNREACHABLE",
		},
		{
			name:     "old cli flag is unsupported",
			stderr:   "error: unknown option '--if-idle'",
			runErr:   &exec.ExitError{},
			wantKind: MailboxDeliveryUnsupported,
		},
		{
			name:     "unrecognized failure is ambiguous",
			stderr:   "segfault",
			runErr:   &exec.ExitError{},
			wantKind: MailboxDeliveryOutcomeUnknown,
			wantCode: mailboxErrorOutcomeUnknown,
		},
		{
			name:     "success with wrong agent id is ambiguous",
			stdout:   `{"agentId":"other-agent","status":"sent"}`,
			wantKind: MailboxDeliveryOutcomeUnknown,
			wantCode: mailboxErrorOutcomeUnknown,
		},
		{
			name:     "success with invalid json is ambiguous",
			stdout:   `not json`,
			wantKind: MailboxDeliveryOutcomeUnknown,
			wantCode: mailboxErrorOutcomeUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := paseoRunner(t, tc.stdout, tc.stderr, tc.runErr)
			transport := &paseoCliTransport{bin: "paseo", run: run}
			_, err := transport.Deliver(context.Background(), paseoTestSession(), "mailbox-1", secretPrompt)
			if err == nil {
				t.Fatal("Deliver succeeded, want typed error")
			}
			var typed *MailboxDeliveryError
			if !errors.As(err, &typed) {
				t.Fatalf("error is not typed: %T %v", err, err)
			}
			if typed.Kind != tc.wantKind {
				t.Fatalf("kind = %v, want %v (error=%v)", typed.Kind, tc.wantKind, err)
			}
			if tc.wantCode != "" && typed.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", typed.Code, tc.wantCode)
			}
			if strings.Contains(err.Error(), secretPrompt) {
				t.Fatalf("error leaked prompt content: %v", err)
			}
		})
	}
}

func TestPaseoDeliveryMissingBinaryIsActionableDependencyError(t *testing.T) {
	run := paseoRunner(t, "", "", errors.New("exec: \"paseo\": executable file not found in $PATH"))
	transport := &paseoCliTransport{bin: "paseo", run: run}
	_, err := transport.Deliver(context.Background(), paseoTestSession(), "mailbox-1", "prompt")
	var typed *MailboxDeliveryError
	if !errors.As(err, &typed) || typed.Kind != MailboxDeliveryUnsupported {
		t.Fatalf("err = %v, want unsupported dependency error", err)
	}
	if !strings.Contains(err.Error(), "paseo CLI could not be started") {
		t.Fatalf("error is not actionable: %v", err)
	}
}

func TestPaseoDeliveryInterruptedByDaemonStopIsAmbiguous(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := func(ctx context.Context, _ string, args ...string) ([]byte, []byte, error) {
		if args[0] == "status" {
			return []byte(`{"serverId":"host-1"}`), nil, nil
		}
		cancel()
		return nil, nil, ctx.Err()
	}
	transport := &paseoCliTransport{bin: "paseo", run: run}
	_, err := transport.Deliver(ctx, paseoTestSession(), "mailbox-1", "prompt")
	var typed *MailboxDeliveryError
	if !errors.As(err, &typed) || typed.Kind != MailboxDeliveryOutcomeUnknown {
		t.Fatalf("err = %v, want ambiguous outcome on daemon stop", err)
	}
}

func TestPaseoDeliveryTimeoutIsAmbiguous(t *testing.T) {
	run := func(ctx context.Context, _ string, args ...string) ([]byte, []byte, error) {
		if args[0] == "status" {
			return []byte(`{"serverId":"host-1"}`), nil, nil
		}
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	transport := &paseoCliTransport{bin: "paseo", timeout: 10 * time.Millisecond, run: run}
	_, err := transport.Deliver(context.Background(), paseoTestSession(), "mailbox-1", "prompt")
	var typed *MailboxDeliveryError
	if !errors.As(err, &typed) || typed.Kind != MailboxDeliveryOutcomeUnknown || typed.Code != "delivery_timeout" {
		t.Fatalf("err = %v, want ambiguous delivery timeout", err)
	}
}

package daemon

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/delivery"
)

func receiptRunner(t *testing.T, stdout, stderr string, runErr error) (paseoCommandRunner, *[]string) {
	t.Helper()
	var calls []string
	return func(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
		if name != "paseo" {
			t.Errorf("command = %q, want paseo", name)
		}
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "status" {
			return []byte(`{"serverId":"host-1"}`), nil, nil
		}
		return []byte(stdout), []byte(stderr), runErr
	}, &calls
}

func TestPaseoReceiptUsesFrozenReadOnlyContract(t *testing.T) {
	run, calls := receiptRunner(t, `{"agentId":"paseo-agent-1","messageId":"mailbox-1","status":"accepted"}`, "", nil)
	transport := &paseoCliTransport{bin: "paseo", run: run}

	receipt, err := transport.LookupReceipt(context.Background(), paseoTestSession(), "mailbox-1")
	if err != nil {
		t.Fatalf("LookupReceipt: %v", err)
	}
	if receipt.Status != delivery.ReceiptAccepted {
		t.Fatalf("status = %q, want accepted", receipt.Status)
	}
	if receipt.Reference != "paseo:paseo-agent-1:mailbox-1:accepted" {
		t.Fatalf("reference = %q", receipt.Reference)
	}
	want := []string{"status --json", "agent receipt paseo-agent-1 mailbox-1 --json"}
	if len(*calls) != len(want) {
		t.Fatalf("calls = %v, want %v", *calls, want)
	}
	for i, command := range want {
		if (*calls)[i] != command {
			t.Fatalf("call %d = %q, want %q", i, (*calls)[i], command)
		}
	}
	for _, call := range *calls {
		if strings.HasPrefix(call, "send") {
			t.Fatalf("receipt lookup must never send: %v", *calls)
		}
	}
}

func TestPaseoReceiptReportsPendingAndAbsent(t *testing.T) {
	for _, status := range []string{"pending", "absent"} {
		t.Run(status, func(t *testing.T) {
			run, _ := receiptRunner(t, `{"agentId":"paseo-agent-1","messageId":"mailbox-1","status":"`+status+`"}`, "", nil)
			transport := &paseoCliTransport{bin: "paseo", run: run}

			receipt, err := transport.LookupReceipt(context.Background(), paseoTestSession(), "mailbox-1")
			if err != nil {
				t.Fatalf("LookupReceipt: %v", err)
			}
			if string(receipt.Status) != status {
				t.Fatalf("status = %q, want %q", receipt.Status, status)
			}
		})
	}
}

func TestPaseoReceiptIdentityMismatchIsNotAbsent(t *testing.T) {
	run, _ := receiptRunner(t, `{"agentId":"another-agent","messageId":"mailbox-1","status":"accepted"}`, "", nil)
	transport := &paseoCliTransport{bin: "paseo", run: run}

	_, err := transport.LookupReceipt(context.Background(), paseoTestSession(), "mailbox-1")
	var hostErr *delivery.HostError
	if !errors.As(err, &hostErr) || hostErr.Code != "paseo_receipt_identity_mismatch" {
		t.Fatalf("error = %v, want an identity mismatch", err)
	}
}

func TestPaseoReceiptMessageMismatchIsNotAbsent(t *testing.T) {
	run, _ := receiptRunner(t, `{"agentId":"paseo-agent-1","messageId":"another-message","status":"absent"}`, "", nil)
	transport := &paseoCliTransport{bin: "paseo", run: run}

	_, err := transport.LookupReceipt(context.Background(), paseoTestSession(), "mailbox-1")
	var hostErr *delivery.HostError
	if !errors.As(err, &hostErr) || hostErr.Code != "paseo_receipt_message_mismatch" {
		t.Fatalf("error = %v, want a message mismatch", err)
	}
}

func TestPaseoReceiptMalformedDocumentIsNotAbsent(t *testing.T) {
	run, _ := receiptRunner(t, `not-json`, "", nil)
	transport := &paseoCliTransport{bin: "paseo", run: run}

	receipt, err := transport.LookupReceipt(context.Background(), paseoTestSession(), "mailbox-1")
	if err == nil {
		t.Fatalf("expected an error, got status %q", receipt.Status)
	}
	if receipt.Status == delivery.ReceiptAbsent {
		t.Fatal("a malformed document must not become an absent receipt")
	}
}

func TestPaseoReceiptUnsupportedHostIsCapabilityGap(t *testing.T) {
	run, _ := receiptRunner(t, "", `{"error":{"code":"DAEMON_UPDATE_REQUIRED","message":"update"}}`, &exec.ExitError{})
	transport := &paseoCliTransport{bin: "paseo", run: run}

	_, err := transport.LookupReceipt(context.Background(), paseoTestSession(), "mailbox-1")
	var hostErr *delivery.HostError
	if !errors.As(err, &hostErr) || hostErr.Kind != delivery.HostUnsupported {
		t.Fatalf("error = %v, want an unsupported host", err)
	}
}

func TestPaseoReceiptOldCLIIsCapabilityGap(t *testing.T) {
	run, _ := receiptRunner(t, "", "error: unknown command 'receipt'", &exec.ExitError{})
	transport := &paseoCliTransport{bin: "paseo", run: run}

	_, err := transport.LookupReceipt(context.Background(), paseoTestSession(), "mailbox-1")
	var hostErr *delivery.HostError
	if !errors.As(err, &hostErr) || hostErr.Kind != delivery.HostUnsupported {
		t.Fatalf("error = %v, want an unsupported CLI", err)
	}
}

func TestPaseoDeliveryHostMapsBusyAndNeverSendsOrdinary(t *testing.T) {
	var calls []string
	transport := &paseoCliTransport{bin: "paseo", run: func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "status" {
			return []byte(`{"serverId":"host-1"}`), nil, nil
		}
		return nil, []byte(`{"error":{"code":"agent_busy","message":"busy"}}`), &exec.ExitError{}
	}}
	host := &paseoDeliveryHost{transport: transport}
	claim := delivery.Claim{
		Delivery: delivery.Delivery{ID: "delivery-1", Kind: "ci_terminal", Payload: []byte(`{"run_id":"run-42"}`)},
		Session:  delivery.HostTarget{Provider: "paseo", HostID: "host-1", SessionID: "paseo-agent-1", DaemonID: "daemon-1"},
	}

	_, err := host.DeliverIfIdle(context.Background(), claim.Session, claim.Delivery)
	var hostErr *delivery.HostError
	if !errors.As(err, &hostErr) || hostErr.Kind != delivery.HostBusy {
		t.Fatalf("error = %v, want busy", err)
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "send") && !strings.Contains(call, "--if-idle") {
			t.Fatalf("ordinary send fallback was attempted: %v", calls)
		}
	}
}

func TestGenericDeliveryPromptCarriesStableIdentityWithoutGrantingPermission(t *testing.T) {
	prompt := renderGenericDelivery(delivery.Delivery{
		ID:        "delivery-1",
		Kind:      "ci_terminal",
		Payload:   []byte(`{"run_id":"run-42","conclusion":"success"}`),
		Recipient: delivery.Recipient{Type: "agent", ID: "agent-1"},
	})
	for _, want := range []string{"delivery-1", "ci_terminal", "run-42", "does not grant new permissions"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt %q is missing %q", prompt, want)
		}
	}
}

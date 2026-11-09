package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/delivery"
)

func TestDelegationResultDeliveryReachesFrozenPaseoSession(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "prompt")
	argsFile := filepath.Join(t.TempDir(), "args")
	fake := filepath.Join(t.TempDir(), "paseo")
	script := `#!/bin/sh
case "$1" in
status) printf '%s\n' '{"serverId":"original-host"}' ;;
send)
 printf '%s\n' "$@" > "` + argsFile + `"
 while [ "$#" -gt 0 ]; do
  if [ "$1" = '--prompt-file' ]; then shift; cat "$1" > "` + capture + `"; break; fi
  shift
 done
 printf '%s\n' '{"agentId":"original-session","status":"sent"}' ;;
*) exit 93 ;;
esac
`
	writeTestExecutable(t, fake, []byte(script))
	payload, _ := json.Marshal(map[string]string{"result_task_id": "delegated-run", "delegation_return_id": "accepted-return", "issue_id": "delegated-issue", "outcome": "completed"})
	notification := delivery.Delivery{ID: "result-notification", Kind: "delegation_result", Recipient: delivery.Recipient{Type: "agent", ID: "dispatcher"}, Refs: delivery.Refs{IssueID: "delegated-issue"}, Payload: payload}
	target := delivery.HostTarget{Provider: "paseo", HostID: "original-host", SessionID: "original-session", DaemonID: "delivery-daemon"}
	host := &paseoDeliveryHost{transport: &paseoCliTransport{bin: fake, run: runPaseoCommand}}
	receipt, err := host.DeliverIfIdle(context.Background(), target, notification)
	if err != nil || receipt != "paseo:original-session:result-notification" {
		t.Fatalf("delivery receipt=%q error=%v", receipt, err)
	}
	prompt, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prompt), string(payload)) || !strings.Contains(string(prompt), "Issue ID: delegated-issue") {
		t.Fatalf("saved result references did not reach host: %s", prompt)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(args), "send\noriginal-session\n--if-idle\n--message-id\nresult-notification\n") {
		t.Fatalf("delivery changed frozen session or atomic send: %s", args)
	}
}

package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestClaimReturnContextPreservesUnknownAndKnownEmpty(t *testing.T) {
	var old, empty Task
	if err := json.Unmarshal([]byte(`{"id":"old"}`), &old); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"id":"empty","accepted_returns":[]}`), &empty); err != nil {
		t.Fatal(err)
	}
	if old.AcceptedReturns != nil || empty.AcceptedReturns == nil || len(*empty.AcceptedReturns) != 0 {
		t.Fatal("missing server context became an empty accepted return set")
	}
	if !strings.Contains(BuildPrompt(old, "codex"), "no authoritative return context") || !strings.Contains(BuildPrompt(empty, "codex"), "No automatic return relationship was accepted at this snapshot") {
		t.Fatal("runtime cannot distinguish unavailable return context from known empty")
	}
	returns := []protocol.AcceptedReturn{{ReturnID: "return-1", RecipientAgentID: "dispatcher-1", Kind: "managed_task"}}
	empty.AcceptedReturns = &returns
	prompt := BuildPrompt(empty, "codex")
	if !strings.Contains(prompt, "return-1") || !strings.Contains(prompt, "dispatcher-1") || !strings.Contains(prompt, "do not send an extra mention") {
		t.Fatal("runtime lost its accepted automatic return instructions")
	}
	returns = append(returns, protocol.AcceptedReturn{})
	if !strings.Contains(BuildPrompt(empty, "codex"), "no authoritative return context") {
		t.Fatal("malformed return was silently omitted from a supposedly complete snapshot")
	}
}

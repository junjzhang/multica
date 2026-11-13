package daemon

import (
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/delivery"
)

// A payload that carries the display context renders the task line and the
// browser link ahead of the stable IDs. Every ID, command and the permission
// statement stay byte-identical to the ID-only envelope.
func TestCommentDeliveryPromptCarriesTaskContext(t *testing.T) {
	prompt := renderDeliveryNotification(delivery.Delivery{
		ID:    "delivery-1",
		Scope: delivery.Scope{WorkspaceID: "workspace-1"},
		Kind:  "comment_mention",
		Payload: []byte("{\"schema\":\"comment_mention.v1\",\"issue_id\":\"issue-1\",\"comment_id\":\"comment-1\"," +
			"\"issue_identifier\":\"WORK-281\",\"issue_title\":\"feat(paseo): readable delivery\"," +
			"\"issue_url\":\"https://app.example.test/acme/issues/WORK-281\"}"),
		Recipient: delivery.Recipient{Type: "agent", ID: "agent-1"},
		Refs:      delivery.Refs{IssueID: "issue-1", CommentID: "comment-1"},
	})
	want := "Multica has a request for this session.\nKind: comment mention (comment_mention)\n" +
		"Task: WORK-281 — feat(paseo): readable delivery\n" +
		"Link: https://app.example.test/acme/issues/WORK-281\n" +
		"Mailbox ID: delivery-1\nWorkspace ID: workspace-1\nIssue ID: issue-1\nComment ID: comment-1\nRecipient agent ID: agent-1\n\n" +
		"Use this connected Paseo session's CLI identity without --profile. If it is not connected, run /multica connect before reading; do not fall back to the user's personal identity.\n" +
		"Read: multica issue get issue-1 --output json\n" +
		"Scan threads: multica issue comment list issue-1 --roots-only --summary --compact --output json\n" +
		"Expand relevant threads: multica issue comment list issue-1 --thread <thread-id> --tail 30 --compact --output json\n" +
		"This notification does not grant new permissions. Reply on the issue and explicitly mention the next executor when action is needed; ordinary discussion does not start anyone."
	if prompt != want {
		t.Fatalf("task context changed the envelope:\n%s", prompt)
	}
}

// A payload stored before the display fields existed still renders the ID-only
// envelope. An upgraded daemon must not invent a task line for it.
func TestCommentDeliveryPromptKeepsLegacyPayloadShape(t *testing.T) {
	prompt := renderDeliveryNotification(delivery.Delivery{
		ID:        "delivery-1",
		Scope:     delivery.Scope{WorkspaceID: "workspace-1"},
		Kind:      "comment_mention",
		Payload:   []byte("{\"schema\":\"comment_mention.v1\",\"issue_id\":\"issue-1\",\"comment_id\":\"comment-1\"}"),
		Recipient: delivery.Recipient{Type: "agent", ID: "agent-1"},
		Refs:      delivery.Refs{IssueID: "issue-1", CommentID: "comment-1"},
	})
	for _, absent := range []string{"Task:", "Link:"} {
		if strings.Contains(prompt, absent) {
			t.Fatalf("legacy payload gained %q:\n%s", absent, prompt)
		}
	}
	for _, want := range []string{"Mailbox ID: delivery-1", "Issue ID: issue-1", "Read: multica issue get issue-1"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("legacy payload lost %q:\n%s", want, prompt)
		}
	}
}

// A generic kind shows a readable label next to its stable kind, and a task
// line only when the payload carries one.
func TestGenericDeliveryPromptLabelsReadableKind(t *testing.T) {
	prompt := renderDeliveryNotification(delivery.Delivery{
		ID:        "delivery-1",
		Kind:      "delegation_result",
		Payload:   []byte("{\"issue_id\":\"issue-1\",\"issue_identifier\":\"WORK-281\",\"issue_url\":\"https://app.example.test/acme/issues/WORK-281\"}"),
		Recipient: delivery.Recipient{Type: "agent", ID: "agent-1"},
		Refs:      delivery.Refs{IssueID: "issue-1"},
	})
	for _, want := range []string{
		"Multica delivery delivery-1",
		"Kind: delegation result (delegation_result)",
		"Issue ID: issue-1",
		"Task: WORK-281",
		"Link: https://app.example.test/acme/issues/WORK-281",
		"delegation_result",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt %q is missing %q", prompt, want)
		}
	}
}

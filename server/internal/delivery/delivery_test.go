package delivery

import (
	"encoding/json"
	"strings"
	"testing"
)

func validRequest() EnqueueRequest {
	return EnqueueRequest{
		Scope:           Scope{WorkspaceID: "0190f0f0-0000-7000-8000-000000000001"},
		Source:          Source{Type: "ci_run", Key: "run-1"},
		Target:          Target{SessionID: "0190f0f0-0000-7000-8000-000000000004"},
		Recipient:       Recipient{Type: "agent", ID: "0190f0f0-0000-7000-8000-000000000002"},
		NotificationKey: "ci:run-1",
		Kind:            "ci_terminal",
		Payload:         json.RawMessage(`{"run_id":"run-1"}`),
	}
}

func TestEnqueueRequestValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*EnqueueRequest)
		wantErr string
	}{
		{name: "valid"},
		{name: "no workspace", mutate: func(r *EnqueueRequest) { r.Scope.WorkspaceID = "" }, wantErr: "workspace id"},
		{name: "no source type", mutate: func(r *EnqueueRequest) { r.Source.Type = "" }, wantErr: "source type"},
		{name: "no source key", mutate: func(r *EnqueueRequest) { r.Source.Key = "" }, wantErr: "source key"},
		{name: "no recipient type", mutate: func(r *EnqueueRequest) { r.Recipient.Type = "" }, wantErr: "recipient type"},
		{name: "no recipient id", mutate: func(r *EnqueueRequest) { r.Recipient.ID = "" }, wantErr: "recipient id"},
		{name: "no target session", mutate: func(r *EnqueueRequest) { r.Target.SessionID = "" }, wantErr: "target session id"},
		{name: "no notification key", mutate: func(r *EnqueueRequest) { r.NotificationKey = "" }, wantErr: "notification key"},
		{name: "no kind", mutate: func(r *EnqueueRequest) { r.Kind = "" }, wantErr: "kind"},
		{name: "no payload", mutate: func(r *EnqueueRequest) { r.Payload = nil }, wantErr: "payload"},
		{name: "null payload", mutate: func(r *EnqueueRequest) { r.Payload = json.RawMessage(`null`) }, wantErr: "must not be null"},
		{name: "invalid payload", mutate: func(r *EnqueueRequest) { r.Payload = json.RawMessage(`{`) }, wantErr: "invalid payload"},
		{name: "oversized payload", mutate: func(r *EnqueueRequest) {
			r.Payload = json.RawMessage(`"` + strings.Repeat("a", MaxPayloadBytes) + `"`)
		}, wantErr: "exceeds"},
		{name: "generic comment ref", mutate: func(r *EnqueueRequest) {
			r.Refs.CommentID = "0190f0f0-0000-7000-8000-000000000003"
		}, wantErr: "requires source type"},
		{name: "comment source with ref", mutate: func(r *EnqueueRequest) {
			r.Source.Type = "comment"
			r.Refs.CommentID = "0190f0f0-0000-7000-8000-000000000003"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validRequest()
			if tt.mutate != nil {
				tt.mutate(&req)
			}
			err := req.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

// A notification key is unique per workspace, so a stored row may only answer
// the request that created it. Only the frozen target may differ: a producer
// retry that re-resolves after a rebind still replays the original delivery.
func TestSameContentRequiresOneLogicalNotification(t *testing.T) {
	stored := Delivery{
		ID:              "0190f0f0-0000-7000-8000-000000000009",
		Scope:           Scope{WorkspaceID: "0190f0f0-0000-7000-8000-000000000001"},
		Source:          Source{Type: "comment", Key: "0190f0f0-0000-7000-8000-000000000003", ActorType: "member", ActorID: "0190f0f0-0000-7000-8000-000000000008"},
		Target:          Target{SessionID: "0190f0f0-0000-7000-8000-000000000004"},
		Recipient:       Recipient{Type: "agent", ID: "0190f0f0-0000-7000-8000-000000000002"},
		NotificationKey: "comment:0190f0f0-0000-7000-8000-000000000003:agent:0190f0f0-0000-7000-8000-000000000002",
		Kind:            "comment_mention",
		Payload:         json.RawMessage(`{"schema":"comment_mention.v1"}`),
		Refs:            Refs{IssueID: "0190f0f0-0000-7000-8000-000000000005", CommentID: "0190f0f0-0000-7000-8000-000000000003"},
	}
	request := func(mutate func(*EnqueueRequest)) EnqueueRequest {
		req := EnqueueRequest{
			Scope:           stored.Scope,
			Source:          stored.Source,
			Target:          stored.Target,
			Recipient:       stored.Recipient,
			NotificationKey: stored.NotificationKey,
			Kind:            stored.Kind,
			Payload:         stored.Payload,
			Refs:            stored.Refs,
		}
		if mutate != nil {
			mutate(&req)
		}
		return req
	}

	tests := []struct {
		name   string
		mutate func(*EnqueueRequest)
		want   bool
	}{
		{name: "identical replay", want: true},
		{name: "retry after rebind keeps the frozen route", mutate: func(r *EnqueueRequest) {
			r.Target.SessionID = "0190f0f0-0000-7000-8000-000000000006"
		}, want: true},
		{name: "another recipient under one key", mutate: func(r *EnqueueRequest) {
			r.Recipient.ID = "0190f0f0-0000-7000-8000-000000000007"
		}},
		{name: "another recipient kind under one key", mutate: func(r *EnqueueRequest) {
			r.Recipient.Type = "squad"
		}},
		{name: "another source event under one key", mutate: func(r *EnqueueRequest) {
			r.Source.Key = "0190f0f0-0000-7000-8000-000000000010"
		}},
		{name: "another source type under one key", mutate: func(r *EnqueueRequest) {
			r.Source.Type = "issue_wakeup"
		}},
		{name: "another kind under one key", mutate: func(r *EnqueueRequest) {
			r.Kind = "issue_wakeup"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := request(tt.mutate).sameContent(stored); got != tt.want {
				t.Fatalf("sameContent() = %t, want %t", got, tt.want)
			}
		})
	}

	// Legacy comment rows predate source keys and payload snapshots. The legacy
	// (recipient, comment) lookup is what finds them, so their empty source key
	// must not turn a replay into a conflict.
	legacy := stored
	legacy.Source = Source{Type: "comment", ActorType: "member", ActorID: stored.Source.ActorID}
	legacy.Payload = nil
	legacy.NotificationKey = ""
	if !request(nil).sameContent(legacy) {
		t.Fatal("sameContent() = false for a legacy comment row found by recipient and comment")
	}
}

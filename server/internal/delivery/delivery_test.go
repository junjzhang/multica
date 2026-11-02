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

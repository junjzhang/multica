package handler

import (
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// TestIssueCoordinatorForClaim pins the claim mapping: a recorded coordinator
// travels with the claim, and a missing type or id stays empty instead of
// producing a half-usable return address.
func TestIssueCoordinatorForClaim(t *testing.T) {
	agentUUID := parseUUID("fca1e614-84a7-40dc-a41a-d24be53e3bdd")
	memberUUID := parseUUID("9b5c6420-6be0-4eb4-8c7f-5f9922e273c7")

	cases := []struct {
		name     string
		typ      pgtype.Text
		id       pgtype.UUID
		wantType string
		wantID   string
	}{
		{
			name:     "agent coordinator",
			typ:      pgtype.Text{String: "agent", Valid: true},
			id:       pgtype.UUID{Bytes: agentUUID.Bytes, Valid: true},
			wantType: "agent",
			wantID:   "fca1e614-84a7-40dc-a41a-d24be53e3bdd",
		},
		{
			name:     "member coordinator",
			typ:      pgtype.Text{String: "member", Valid: true},
			id:       pgtype.UUID{Bytes: memberUUID.Bytes, Valid: true},
			wantType: "member",
			wantID:   "9b5c6420-6be0-4eb4-8c7f-5f9922e273c7",
		},
		{
			name: "null coordinator",
			typ:  pgtype.Text{},
			id:   pgtype.UUID{},
		},
		{
			name:     "type without id",
			typ:      pgtype.Text{String: "agent", Valid: true},
			id:       pgtype.UUID{},
			wantType: "",
			wantID:   "",
		},
		{
			name: "id without type",
			typ:  pgtype.Text{},
			id:   pgtype.UUID{Bytes: agentUUID.Bytes, Valid: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotType, gotID := issueCoordinatorForClaim(tc.typ, tc.id)
			if gotType != tc.wantType || gotID != tc.wantID {
				t.Fatalf("issueCoordinatorForClaim() = (%q, %q), want (%q, %q)", gotType, gotID, tc.wantType, tc.wantID)
			}
		})
	}
}

// TestClaimResponseCarriesCoordinatorFields pins the wire names the daemon
// decodes, and that an unrecorded coordinator stays off the payload.
func TestClaimResponseCarriesCoordinatorFields(t *testing.T) {
	encoded, err := json.Marshal(AgentTaskResponse{
		CoordinatorType: "agent",
		CoordinatorID:   "fca1e614-84a7-40dc-a41a-d24be53e3bdd",
	})
	if err != nil {
		t.Fatalf("marshal claim response: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("decode claim response: %v", err)
	}
	if payload["coordinator_type"] != "agent" || payload["coordinator_id"] != "fca1e614-84a7-40dc-a41a-d24be53e3bdd" {
		t.Fatalf("coordinator fields = %v / %v", payload["coordinator_type"], payload["coordinator_id"])
	}

	encoded, err = json.Marshal(AgentTaskResponse{})
	if err != nil {
		t.Fatalf("marshal empty claim response: %v", err)
	}
	payload = nil
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("decode empty claim response: %v", err)
	}
	for _, key := range []string{"coordinator_type", "coordinator_id"} {
		if _, ok := payload[key]; ok {
			t.Errorf("unrecorded coordinator still sent %q", key)
		}
	}
}

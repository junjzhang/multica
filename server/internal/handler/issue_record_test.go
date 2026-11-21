package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func recordRequest(method, issueID string, body any) *http.Request {
	return testutil.WithURLParams(newRequest(method, "/api/issues/"+issueID+"/records", body), "id", issueID)
}

func postRecord(t *testing.T, issueID string, body map[string]any, want int) IssueRecordResponse {
	t.Helper()
	var out IssueRecordResponse
	resp := testutil.Call(t, testHandler.CreateIssueRecord, recordRequest(http.MethodPost, issueID, body)).Want(want)
	if want < 300 {
		resp.JSON(&out)
	}
	return out
}

func TestIssueRecordAppendSupersedeAndList(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	issueID := dbfx.Issue(t, "issue record ledger", nil)

	run := postRecord(t, issueID, map[string]any{
		"client_request_id": "01900000-0000-7000-8000-0000000000a1",
		"kind":              "run",
		"summary":           "FA3 dQ repeat",
		"evidence":          []string{"ci-run/0817b8c4c486"},
		"verdict":           "pending",
		"fields":            map[string]any{"protocol": "v1", "gpus": 8},
	}, http.StatusCreated)
	if run.Seq != 1 || run.Fields["gpus"] != float64(8) || run.AuthorType != "member" {
		t.Fatalf("unexpected first record: %+v", run)
	}

	decision := postRecord(t, issueID, map[string]any{
		"client_request_id": "01900000-0000-7000-8000-0000000000a2",
		"kind":              "decision",
		"summary":           "Keep 128x64 tile",
		"verdict":           "keep",
	}, http.StatusCreated)
	if decision.Seq != 2 || len(decision.Evidence) != 0 {
		t.Fatalf("unexpected decision: %+v", decision)
	}

	concluded := postRecord(t, issueID, map[string]any{
		"client_request_id": "01900000-0000-7000-8000-0000000000a3",
		"kind":              "run",
		"summary":           "FA3 dQ repeat concluded",
		"verdict":           "pass",
		"supersedes_seq":    1,
	}, http.StatusCreated)
	if concluded.Seq != 3 || concluded.SupersedesSeq == nil || *concluded.SupersedesSeq != 1 {
		t.Fatalf("unexpected conclusion: %+v", concluded)
	}

	var all struct {
		Records []IssueRecordResponse `json:"records"`
	}
	testutil.Call(t, testHandler.ListIssueRecords, recordRequest(http.MethodGet, issueID, nil)).Want(http.StatusOK).JSON(&all)
	if len(all.Records) != 3 || all.Records[0].Seq != 1 || all.Records[2].Seq != 3 {
		t.Fatalf("unexpected list: %+v", all.Records)
	}

	runsReq := testutil.WithURLParams(newRequest(http.MethodGet, "/api/issues/"+issueID+"/records?kind=run", nil), "id", issueID)
	var runs struct {
		Records []IssueRecordResponse `json:"records"`
	}
	testutil.Call(t, testHandler.ListIssueRecords, runsReq).Want(http.StatusOK).JSON(&runs)
	if len(runs.Records) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(runs.Records))
	}
}

func TestIssueRecordReplayIsIdempotent(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	issueID := dbfx.Issue(t, "issue record replay", nil)
	body := map[string]any{
		"client_request_id": "01900000-0000-7000-8000-0000000000b1",
		"kind":              "decision",
		"summary":           "Revert prefetch",
		"verdict":           "revert",
	}
	first := postRecord(t, issueID, body, http.StatusCreated)
	replay := postRecord(t, issueID, body, http.StatusOK)
	if replay.ID != first.ID {
		t.Fatalf("replay created a new record: %s != %s", replay.ID, first.ID)
	}
	body["summary"] = "Different"
	postRecord(t, issueID, body, http.StatusConflict)
}

func TestIssueRecordRejectsInvalidRequests(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	issueID := dbfx.Issue(t, "issue record validation", nil)
	postRecord(t, issueID, map[string]any{
		"client_request_id": "01900000-0000-7000-8000-0000000000c1",
		"kind":              "run",
		"summary":           "base",
		"verdict":           "fail",
	}, http.StatusCreated)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"unknown kind", map[string]any{"kind": "note", "summary": "x", "verdict": "keep"}},
		{"verdict of other kind", map[string]any{"kind": "decision", "summary": "x", "verdict": "pass"}},
		{"empty summary", map[string]any{"kind": "run", "summary": "", "verdict": "pass"}},
		{"long summary", map[string]any{"kind": "run", "summary": strings.Repeat("x", maxIssueRecordSummary+1), "verdict": "pass"}},
		{"nested field", map[string]any{"kind": "run", "summary": "x", "verdict": "pass", "fields": map[string]any{"a": map[string]any{}}}},
		{"missing supersedes target", map[string]any{"kind": "run", "summary": "x", "verdict": "pass", "supersedes_seq": 9}},
		{"supersedes other kind", map[string]any{"kind": "decision", "summary": "x", "verdict": "keep", "supersedes_seq": 1}},
		{"missing request id", map[string]any{"client_request_id": "", "kind": "run", "summary": "x", "verdict": "pass"}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := tc.body["client_request_id"]; !ok {
				tc.body["client_request_id"] = "01900000-0000-7000-8000-0000000001" + string(rune('0'+i/10)) + string(rune('0'+i%10))
			}
			postRecord(t, issueID, tc.body, http.StatusBadRequest)
		})
	}
}

func TestIssueRecordsDeletedWithIssue(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	issueID := dbfx.Issue(t, "issue record cleanup", nil)
	postRecord(t, issueID, map[string]any{
		"client_request_id": "01900000-0000-7000-8000-0000000000d1",
		"kind":              "decision",
		"summary":           "x",
		"verdict":           "keep",
	}, http.StatusCreated)
	testutil.Call(t, testHandler.DeleteIssue, testutil.WithURLParams(newRequest(http.MethodDelete, "/api/issues/"+issueID, nil), "id", issueID)).WantOneOf(http.StatusOK, http.StatusNoContent)
	var n int
	dbfx.QueryRow(t, `SELECT count(*) FROM issue_record WHERE issue_id = $1`, issueID).Scan(&n)
	if n != 0 {
		t.Fatalf("expected records to be deleted with the issue, found %d", n)
	}
}

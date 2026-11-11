package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

const (
	testCoordinatorIssueID  = "11111111-1111-4111-8111-111111111111"
	testCoordinatorMemberID = "22222222-2222-4222-8222-222222222222"
)

func coordinationTestServer(t *testing.T, body *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/ws-1/members":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"user_id": testCoordinatorMemberID, "name": "Alice"}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/agents":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodPut && r.URL.Path == "/api/issues/"+testCoordinatorIssueID:
			if err := json.NewDecoder(r.Body).Decode(body); err != nil {
				t.Errorf("decode update: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": testCoordinatorIssueID, "identifier": "MUL-1"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/issues":
			if err := json.NewDecoder(r.Body).Decode(body); err != nil {
				t.Errorf("decode create: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": testCoordinatorIssueID, "identifier": "MUL-1", "title": "Coordinated"})
		default:
			http.NotFound(w, r)
		}
	}))
}

func setCoordinationTestEnv(t *testing.T, serverURL string) {
	t.Helper()
	t.Chdir(t.TempDir())
	setCLITestServerEnv(t, serverURL)
	t.Setenv("MULTICA_AGENT_ID", "")
	t.Setenv("MULTICA_TASK_ID", "")
	t.Setenv("MULTICA_DAEMON_PORT", "")
}

func TestRunIssueUpdateSendsCoordinatorAndMergeAuthorization(t *testing.T) {
	var body map[string]any
	srv := coordinationTestServer(t, &body)
	defer srv.Close()
	setCoordinationTestEnv(t, srv.URL)

	cmd := newIssueUpdateTestCmd()
	for name, value := range map[string]string{
		"coordinator-id":       testCoordinatorMemberID,
		"merge-allowed":        "true",
		"merge-repository-url": "https://github.com/example/repo",
		"merge-target-branch":  "main",
	} {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := runIssueUpdate(cmd, []string{testCoordinatorIssueID}); err != nil {
		t.Fatalf("runIssueUpdate: %v", err)
	}

	coordinator, ok := body["coordinator"].(map[string]any)
	if !ok {
		t.Fatalf("coordinator = %#v", body["coordinator"])
	}
	if coordinator["type"] != "member" || coordinator["id"] != testCoordinatorMemberID {
		t.Fatalf("coordinator = %v", coordinator)
	}
	merge, ok := body["merge_authorization"].(map[string]any)
	if !ok {
		t.Fatalf("merge_authorization = %#v", body["merge_authorization"])
	}
	if merge["allowed"] != true || merge["repository_url"] != "https://github.com/example/repo" || merge["target_branch"] != "main" {
		t.Fatalf("merge_authorization = %v", merge)
	}
}

func TestRunIssueUpdateRejectsEmptyCoordinator(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	setCoordinationTestEnv(t, srv.URL)

	cmd := newIssueUpdateTestCmd()
	if err := cmd.Flags().Set("coordinator-id", ""); err != nil {
		t.Fatal(err)
	}
	err := runIssueUpdate(cmd, []string{testCoordinatorIssueID})
	if err == nil || !strings.Contains(err.Error(), "resolve coordinator") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunIssueUpdateRevokesMergeAuthorization(t *testing.T) {
	var body map[string]any
	srv := coordinationTestServer(t, &body)
	defer srv.Close()
	setCoordinationTestEnv(t, srv.URL)

	cmd := newIssueUpdateTestCmd()
	if err := cmd.Flags().Set("merge-allowed", "false"); err != nil {
		t.Fatal(err)
	}
	if err := runIssueUpdate(cmd, []string{testCoordinatorIssueID}); err != nil {
		t.Fatalf("runIssueUpdate: %v", err)
	}
	merge, ok := body["merge_authorization"].(map[string]any)
	if !ok || merge["allowed"] != false {
		t.Fatalf("merge_authorization = %#v", body["merge_authorization"])
	}
	if _, exists := merge["repository_url"]; exists {
		t.Fatalf("revoke must not carry a repository scope: %v", merge)
	}
}

func TestRunIssueUpdateRejectsIncompleteMergeScope(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	setCoordinationTestEnv(t, srv.URL)

	cmd := newIssueUpdateTestCmd()
	_ = cmd.Flags().Set("merge-repository-url", "https://github.com/example/repo")
	err := runIssueUpdate(cmd, []string{testCoordinatorIssueID})
	if err == nil || !strings.Contains(err.Error(), "require --merge-allowed") {
		t.Fatalf("err = %v", err)
	}

	cmd = newIssueUpdateTestCmd()
	_ = cmd.Flags().Set("merge-allowed", "true")
	_ = cmd.Flags().Set("merge-repository-url", "https://github.com/example/repo")
	err = runIssueUpdate(cmd, []string{testCoordinatorIssueID})
	if err == nil || !strings.Contains(err.Error(), "--merge-target-branch") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunIssueCreateSendsCoordinator(t *testing.T) {
	var body map[string]any
	srv := coordinationTestServer(t, &body)
	defer srv.Close()
	setCoordinationTestEnv(t, srv.URL)

	cmd := newIssueCreateTestCmdForCoordinator()
	if err := cmd.Flags().Set("title", "Coordinated"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("coordinator-id", testCoordinatorMemberID); err != nil {
		t.Fatal(err)
	}
	if err := runIssueCreate(cmd, nil); err != nil {
		t.Fatalf("runIssueCreate: %v", err)
	}
	coordinator, ok := body["coordinator"].(map[string]any)
	if !ok || coordinator["type"] != "member" || coordinator["id"] != testCoordinatorMemberID {
		t.Fatalf("coordinator = %#v", body["coordinator"])
	}
}

// newIssueCreateTestCmdForCoordinator carries the flags runIssueCreate reads.
func newIssueCreateTestCmdForCoordinator() *cobra.Command {
	cmd := &cobra.Command{Use: "create"}
	cmd.Flags().String("title", "", "")
	cmd.Flags().String("description", "", "")
	cmd.Flags().Bool("description-stdin", false, "")
	cmd.Flags().String("description-file", "", "")
	cmd.Flags().Bool("allow-external-file", false, "")
	cmd.Flags().String("status", "", "")
	cmd.Flags().String("priority", "", "")
	cmd.Flags().String("assignee", "", "")
	cmd.Flags().String("assignee-id", "", "")
	cmd.Flags().String("coordinator", "", "")
	cmd.Flags().String("coordinator-id", "", "")
	cmd.Flags().String("parent", "", "")
	cmd.Flags().Int("stage", 0, "")
	cmd.Flags().String("project", "", "")
	cmd.Flags().String("start-date", "", "")
	cmd.Flags().String("due-date", "", "")
	cmd.Flags().Bool("allow-duplicate", false, "")
	cmd.Flags().String("output", "json", "")
	cmd.Flags().StringSlice("attachment", nil, "")
	cmd.Flags().StringSlice("attachment-id", nil, "")
	cmd.Flags().StringArray("property", nil, "")
	return cmd
}

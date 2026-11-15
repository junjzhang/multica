package handler

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// Entering a squad-handled workflow step reassigns the issue to the squad and
// previews the step brief, but it is display state: no leader run starts until
// someone explicitly @-mentions the leader or reruns the issue.
func TestWorkflowHandoffToSquadReassignsWithoutStartingLeader(t *testing.T) {
	seedTestCatalog(t)
	fx := newSquadBriefingClaimFixture(t, t.Context(), "Workflow squad handoff")
	wf := createTestWorkflow(t, "Squad flow "+workflowTestSuffix(), "in_progress", "in_review")
	projectID := createWorkflowTestProject(t, "Squad handoff project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	dbfx.Exec(t, `UPDATE issue SET project_id=$2,status='todo' WHERE id=$1`, fx.IssueID, projectID)

	for _, status := range []string{"in_progress", "in_review"} {
		instruction := "Coordinate only the " + status + " step."
		setWorkflowHandoff(t, wf.ID, status, map[string]any{
			"handler": map[string]any{"type": "squad", "id": fx.SquadID}, "instructions": instruction,
			"next_status_key": "done", "back_status_key": "todo",
		}).Want(http.StatusOK)

		var preview WorkflowHandoffPreviewResponse
		req := withURLParam(newRequest(http.MethodGet, "/api/issues/"+fx.IssueID+"/workflow-handoff?status="+status, nil), "id", fx.IssueID)
		testutil.Call(t, testHandler.PreviewWorkflowHandoff, req).Want(http.StatusOK).JSON(&preview)
		if !preview.Handoff || preview.HandlerType != "squad" || preview.HandlerID != fx.SquadID || !strings.Contains(preview.Brief, instruction) {
			t.Fatalf("squad handoff preview = %+v", preview)
		}

		updateIssueForTest(t, fx.IssueID, map[string]any{"status": status}).Want(http.StatusOK)
		_, assigneeType, assigneeID := issueStatusAndAssignee(t, fx.IssueID)
		if assigneeType != "squad" || assigneeID != fx.SquadID {
			t.Fatalf("handoff assignee = %s/%s, want squad/%s", assigneeType, assigneeID, fx.SquadID)
		}
		tasks, err := testHandler.Queries.ListTasksByIssue(t.Context(), parseUUID(fx.IssueID))
		if err != nil || len(tasks) != 0 {
			t.Fatalf("step %s queued %d run(s), %v; workflow handoffs must not start work", status, len(tasks), err)
		}
	}
}

func TestWorkflowSquadHandoffKeepsAssignmentGates(t *testing.T) {
	seedTestCatalog(t)
	wf := createTestWorkflow(t, "Squad permissions "+workflowTestSuffix(), "in_progress")
	leaderID := seededReadyAgentID(t)
	otherWS := dbfx.Workspace(t, "Other squad workspace", "squad-handoff-"+workflowTestSuffix())
	privateLeaderID, _, _ := privateAgentTestFixture(t)
	cases := []struct {
		name, squadID string
		status        int
	}{
		{"another workspace", dbfx.Squad(t, "Foreign squad", leaderID, testutil.Cols{"workspace_id": otherWS}), http.StatusBadRequest},
		{"archived squad", dbfx.Squad(t, "Archived squad", leaderID, testutil.Cols{"archived_at": time.Now()}), http.StatusBadRequest},
		{"private leader", dbfx.Squad(t, "Private leader squad", privateLeaderID), http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setWorkflowHandoff(t, wf.ID, "in_progress", map[string]any{
				"handler": map[string]any{"type": "squad", "id": tc.squadID},
			}).Want(tc.status)
		})
	}
}

package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestExplicitDispatchKeepsCurrentPrincipalInsteadOfIssueCreator(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	for _, action := range []string{"assignment", "rerun"} {
		for _, sourceKind := range []string{"authorized", "unattributed", "accountable_only"} {
			t.Run(action+"/"+sourceKind, func(t *testing.T) {
				human := dbfx.User(t, "Source human", fmt.Sprintf("source-human-%s-%s@example.test", action, sourceKind))
				dbfx.Member(t, testWorkspaceID, human, "member")
				_, sourceID, token := managedDispatcherFixture(t, action+"-"+sourceKind)
				var originator, accountable any
				if sourceKind == "authorized" {
					originator, accountable = human, human
				} else if sourceKind == "accountable_only" {
					accountable = human
				}
				dbfx.Exec(t, `UPDATE agent_task_queue SET originator_user_id=$2,accountable_user_id=$3 WHERE id=$1`, sourceID, originator, accountable)
				target, runtimeID := executableTarget(t, action+"-target-"+sourceKind)
				issueID := dbfx.Issue(t, "Issue belongs to another human", testutil.Cols{
					"creator_type": "member", "creator_id": testUserID,
				})
				t.Cleanup(func() { cleanupDelegatedIssue(t, issueID) })
				if action == "assignment" {
					w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.UpdateIssue), http.MethodPut, "/api/issues/"+issueID,
						map[string]any{"assignee_type": "agent", "assignee_id": target}, map[string]string{"id": issueID}, nil)
					if w.Code != http.StatusOK {
						t.Fatalf("assignment = %d: %s", w.Code, w.Body.String())
					}
				} else {
					dbfx.Exec(t, `UPDATE issue SET assignee_type='agent',assignee_id=$2 WHERE id=$1`, issueID, target)
					prior := dbfx.Task(t, target, testutil.Cols{
						"issue_id": issueID, "runtime_id": runtimeID, "status": "completed",
						"originator_user_id": testUserID, "accountable_user_id": testUserID,
					})
					w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.RerunIssue), http.MethodPost, "/api/issues/"+issueID+"/rerun",
						map[string]any{"task_id": prior}, map[string]string{"id": issueID}, nil)
					if w.Code != http.StatusAccepted {
						t.Fatalf("rerun = %d: %s", w.Code, w.Body.String())
					}
				}
				id := latestTaskIDFor(t, issueID, target)
				task, err := testHandler.Queries.GetAgentTask(t.Context(), parseUUID(id))
				if err != nil {
					t.Fatal(err)
				}
				wantOriginator := ""
				if sourceKind == "authorized" {
					wantOriginator = human
				}
				if uuidToString(task.OriginatorUserID) != wantOriginator || uuidToString(task.DelegatedFromTaskID) != sourceID || task.OriginatorSource.String == "direct_human" {
					t.Fatalf("current invocation lost: originator=%s want=%s parent=%s want=%s source=%s", uuidToString(task.OriginatorUserID), wantOriginator, uuidToString(task.DelegatedFromTaskID), sourceID, task.OriginatorSource.String)
				}
				if accountable != nil && uuidToString(task.AccountableUserID) != human {
					t.Fatalf("source accountability changed to %s", uuidToString(task.AccountableUserID))
				}
				// The runtime owner's credential must not lend the issue creator's
				// private-agent permission to this delegated execution.
				dbfx.Exec(t, `UPDATE agent_task_queue SET status='running',session_id=$2,work_dir=$3,
					resource_control_required=true,session_pin_confirmed=true,session_pin_generation=gen_random_uuid()
					WHERE id=$1`, id, "principal-session-"+id, "/work/principal-"+id)
				childToken := taskTokenFixture(t, target, id)
				privateTarget := externalPrivateTarget(t, testUserID, "issue creator private agent")
				w := serveAuthenticated(t, childToken, http.HandlerFunc(testHandler.CreateComment), http.MethodPost, "/api/issues/"+issueID+"/comments",
					map[string]any{"content": fmt.Sprintf("[@Private](mention://agent/%s) Request", privateTarget)}, map[string]string{"id": issueID}, nil)
				if w.Code != http.StatusCreated {
					t.Fatalf("downstream gate = %d: %s", w.Code, w.Body.String())
				}
				if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2`, issueID, privateTarget); n != 0 {
					t.Fatal("delegated execution borrowed issue creator's private permission")
				}
			})
		}
	}
}

func TestManagedQuickCreatePreservesCurrentInvocation(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	for _, authorized := range []bool{true, false} {
		t.Run(fmt.Sprintf("authorized=%v", authorized), func(t *testing.T) {
			human := dbfx.User(t, "Quick create originator", fmt.Sprintf("quick-originator-%v@example.test", authorized))
			dbfx.Member(t, testWorkspaceID, human, "member")
			_, sourceID, token := managedDispatcherFixture(t, fmt.Sprintf("quick-origin-%v", authorized))
			var originator any
			if authorized {
				originator = human
			}
			dbfx.Exec(t, `UPDATE agent_task_queue SET originator_user_id=$2,accountable_user_id=$3 WHERE id=$1`, sourceID, originator, human)
			target, runtimeID := executableTarget(t, "quick invocation target")
			dbfx.Exec(t, `UPDATE agent_runtime SET metadata='{"cli_version":"0.6.5"}'::jsonb WHERE id=$1`, runtimeID)
			w := serveAuthenticated(t, token, http.HandlerFunc(testHandler.QuickCreateIssue), http.MethodPost, "/api/issues/quick-create",
				map[string]any{"agent_id": target, "prompt": "Create a task for this request"}, nil, nil)
			if w.Code != http.StatusAccepted {
				t.Fatalf("quick create = %d: %s", w.Code, w.Body.String())
			}
			var out struct {
				TaskID string `json:"task_id"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM agent_task_queue WHERE id=$1`, out.TaskID) })
			task, err := testHandler.Queries.GetAgentTask(t.Context(), parseUUID(out.TaskID))
			if err != nil {
				t.Fatal(err)
			}
			wantOriginator := ""
			if authorized {
				wantOriginator = human
			}
			if uuidToString(task.OriginatorUserID) != wantOriginator || uuidToString(task.DelegatedFromTaskID) != sourceID || task.OriginatorSource.String != "delegation" {
				t.Fatalf("quick create substituted principal: originator=%s want=%s parent=%s want=%s source=%s", uuidToString(task.OriginatorUserID), wantOriginator, uuidToString(task.DelegatedFromTaskID), sourceID, task.OriginatorSource.String)
			}
			dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed' WHERE id=$1`, sourceID)
			closed := serveAuthenticated(t, token, http.HandlerFunc(testHandler.QuickCreateIssue), http.MethodPost, "/api/issues/quick-create",
				map[string]any{"agent_id": target, "prompt": "Must not start after source completion"}, nil, nil)
			if closed.Code < http.StatusBadRequest {
				t.Fatalf("completed source started more work: %d %s", closed.Code, closed.Body.String())
			}
			if n := countValue(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1`, target); n != 1 {
				t.Fatalf("completed source added a run: %d", n)
			}
		})
	}
}

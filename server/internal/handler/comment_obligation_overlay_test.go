package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/runtimeapps"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type obligationOverlayBuilder func(context.Context, pgtype.UUID, db.Agent) (runtimeapps.MCPOverlayResult, error)

func (f obligationOverlayBuilder) BuildTaskOverlay(ctx context.Context, user pgtype.UUID, agent db.Agent) (runtimeapps.MCPOverlayResult, error) {
	return f(ctx, user, agent)
}

func TestCommentObligationOverlayAfterCommitAndCAS(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	for _, mode := range []string{"queued", "media_deferred", "originator_changed", "trigger_changed", "builder_failure", "rollback"} {
		t.Run(mode, func(t *testing.T) {
			_, parent, token := managedDispatcherFixture(t, "overlay-affinity-"+mode)
			dbfx.Exec(t, `UPDATE agent_task_queue SET originator_user_id=$2,accountable_user_id=$2 WHERE id=$1`, parent, testUserID)
			target, runtime := executableTarget(t, "overlay-affinity-target-"+mode)
			issue := dbfx.Issue(t, "overlay affinity "+mode)
			t.Cleanup(func() { cleanupDelegatedIssue(t, issue) })
			comment := pendingCommentObligation(t, "overlay", "", issue, token, target)
			other := dbfx.User(t, "prior plan owner", "prior-plan-"+comment+"@example.test")
			dbfx.Member(t, testWorkspaceID, other, "member")
			cols := testutil.Cols{"runtime_id": runtime, "issue_id": issue, "trigger_comment_id": comment, "status": "queued", "originator_user_id": other, "accountable_user_id": other, "runtime_mcp_overlay": []byte(`{"owner":"old"}`), "runtime_connected_apps": []byte(`[{"provider":"old"}]`)}
			if mode == "media_deferred" {
				cols["status"] = "deferred"
				cols["context"] = []byte(`{"channel_issue_media_pending":true}`)
			}
			taskID := dbfx.Task(t, target, cols)
			var obligation string
			dbfx.QueryRow(t, `SELECT id::text FROM comment_dispatch_obligation WHERE comment_id=$1`, comment).Scan(&obligation)
			if mode == "rollback" {
				fn := "obligation_commit_fail_" + strings.ReplaceAll(comment, "-", "_")
				dbfx.Exec(t, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger AS $$ BEGIN IF NEW.id='%s' AND NEW.status='settled' THEN RAISE EXCEPTION 'commit rollback probe'; END IF; RETURN NEW; END $$ LANGUAGE plpgsql; CREATE CONSTRAINT TRIGGER %s AFTER UPDATE ON comment_dispatch_obligation DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION %s();`, fn, obligation, fn, fn))
				t.Cleanup(func() {
					dbfx.Exec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON comment_dispatch_obligation; DROP FUNCTION IF EXISTS %s();`, fn, fn))
				})
			}
			h := New(db.New(testPool), testPool, testHandler.Hub, testHandler.Bus, nil, nil, nil, analytics.NoopClient{}, Config{})
			withComposioMCPAppsFlag(t, h, true)
			calls := 0
			h.TaskService.Composio = obligationOverlayBuilder(func(ctx context.Context, user pgtype.UUID, _ db.Agent) (runtimeapps.MCPOverlayResult, error) {
				calls++
				row, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(taskID))
				if err != nil {
					return runtimeapps.MCPOverlayResult{}, err
				}
				if uuidToString(user) != testUserID || uuidToString(row.OriginatorUserID) != testUserID || len(row.RuntimeMcpOverlay) != 0 || len(row.RuntimeConnectedApps) != 0 {
					t.Error("overlay built before committed attribution/old capability removal")
				}
				if countValue(t, `SELECT count(*) FROM agent_task_delegation_return WHERE task_id=$1`, taskID) != 1 {
					t.Error("overlay built before accepted returns committed")
				}
				if mode == "originator_changed" {
					dbfx.Exec(t, `UPDATE agent_task_queue SET originator_user_id=$2,accountable_user_id=$2,runtime_mcp_overlay='{"owner":"newer"}' WHERE id=$1`, taskID, other)
				}
				if mode == "trigger_changed" {
					newer := dbfx.Comment(t, issue, "newer instruction", testutil.Cols{"parent_id": comment})
					dbfx.Exec(t, `UPDATE agent_task_queue SET trigger_comment_id=$2,runtime_mcp_overlay='{"owner":"newer"}' WHERE id=$1`, taskID, newer)
				}
				if mode == "builder_failure" {
					return runtimeapps.MCPOverlayResult{}, errors.New("optional overlay unavailable")
				}
				return runtimeapps.MCPOverlayResult{MCPOverlay: json.RawMessage(`{"owner":"built"}`), ConnectedApps: []runtimeapps.ConnectedApp{{Provider: "composio", ToolkitSlug: "fixture"}}}, nil
			})
			coverage, _, err := h.dispatchCommentObligation(t.Context(), parseUUID(obligation), false)
			after, readErr := testHandler.Queries.GetAgentTask(t.Context(), parseUUID(taskID))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if mode == "rollback" {
				if err == nil || calls != 0 || uuidToString(after.OriginatorUserID) != other || !strings.Contains(string(after.RuntimeMcpOverlay), "old") {
					t.Fatalf("rollback leaked capability side effects: calls=%d err=%v", calls, err)
				}
				if countValue(t, `SELECT count(*) FROM agent_task_delegation_return WHERE task_id=$1`, taskID) != 0 {
					t.Fatal("rollback left a return link")
				}
				return
			}
			if err != nil || !coverage.settled || calls != 1 {
				t.Fatalf("fold result: settled=%v calls=%d err=%v", coverage.settled, calls, err)
			}
			switch mode {
			case "originator_changed", "trigger_changed":
				if !strings.Contains(string(after.RuntimeMcpOverlay), "newer") {
					t.Fatal("stale overlay replaced newer plan")
				}
			case "builder_failure":
				if len(after.RuntimeMcpOverlay) != 0 || len(after.RuntimeConnectedApps) != 0 {
					t.Fatal("soft failure retained old actor capabilities")
				}
			default:
				if !strings.Contains(string(after.RuntimeMcpOverlay), "built") || len(after.RuntimeConnectedApps) == 0 {
					t.Fatal("committed plan did not receive its matching optional overlay")
				}
			}
		})
	}
}

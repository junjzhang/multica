package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestProviderStoreRetainsUnknownFrozenSource(t *testing.T) {
	if testHandler == nil {
		t.Fatal("PostgreSQL required")
	}
	for _, missing := range []string{"runtime", "source"} {
		t.Run(missing, func(t *testing.T) {
			label := "lost-" + missing
			source, result, _ := resultFixture(t, label)
			runtimeID := uuidToString(source.RuntimeID)
			finishResultFixture(t, result)
			completeResultHandlingTurn(t, result)
			testutil.Call(t, testHandler.UpdateIssue, withURLParam(newRequest("PATCH", "/issue", map[string]string{"status": "done"}), "id", uuidToString(result.IssueID))).Want(200)
			if missing == "runtime" {
				unbindRuntime(t, context.Background(), uuidToString(source.RuntimeID), uuidToString(source.AgentID))
				// A new runtime on the same real daemon proves permission for the legacy store.
				rt := dbfx.Runtime(t, "replacement runtime", testutil.Cols{"runtime_mode": "local", "provider": "codex", "daemon_id": "result-" + label})
				dbfx.Exec(t, `UPDATE agent SET runtime_id=$2 WHERE id=$1`, source.AgentID, rt)
				runtimeID = rt
			} else {
				dbfx.Exec(t, `DELETE FROM agent_task_queue WHERE id=$1`, source.ID)
			}
			testutil.Call(t, testHandler.UpdateIssue, withURLParam(newRequest("PATCH", "/issue", map[string]string{"status": "todo"}), "id", uuidToString(result.IssueID))).Want(200)
			token, _ := insertTestPAT(t, time.Now().Add(time.Hour))
			req := protocol.ResourceReleaseRequest{Kind: "provider_store", DaemonID: "result-" + label, Provider: "codex", AgentID: uuidToString(source.AgentID), ScopeType: "issue", ScopeID: uuidToString(source.IssueID), ResourcePath: "/retained/codex/original-store"}
			out := authenticatedResourceRelease(t, token, req)
			if out.Code != http.StatusOK {
				t.Fatalf("release HTTP%d: %s", out.Code, out.Body.String())
			}
			var decision protocol.ResourceReleaseDecision
			if err := json.Unmarshal(out.Body.Bytes(), &decision); err != nil {
				t.Fatal(err)
			}
			t.Logf("missing %s + issue reopen + legacy store release: %+v", missing, decision)
			if decision.ReleaseAllowed || decision.Reason != "source_scope_unknown" || len(decision.ProtectedResources) == 0 {
				t.Fatal("unknown frozen source scope allowed deletion of original provider store")
			}
			if decision.ProtectedResources[0].ScopeType != "" || decision.ProtectedResources[0].ScopeID != "" {
				t.Fatal("unknown original scope replaced by an invented scope")
			}
			otherAgent := dbfx.Agent(t, "unrelated store owner", runtimeID)
			req.AgentID = otherAgent
			other := authenticatedResourceRelease(t, token, req)
			if other.Code != 200 || json.Unmarshal(other.Body.Bytes(), &decision) != nil || !decision.ReleaseAllowed {
				t.Fatalf("unknown scope leaked across actor identity: HTTP %d %s", other.Code, other.Body.String())
			}
		})
	}
}

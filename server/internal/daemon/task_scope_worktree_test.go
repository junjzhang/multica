package daemon

import (
	"context"
	"encoding/json"
	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStrictReturnFromTaskScopedLocalWorktree(t *testing.T) {
	for _, scope := range []string{"issue", "task", "task_linked", "task_recovered_prepare", "empty_owner", "conflicting_owner"} {
		t.Run(scope, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			d, argsFile, cleanup := newLeaderReuseTestDaemon(t)
			defer cleanup()
			d.cfg.DaemonID = "scope-probe"
			d.localPathLocks = NewLocalPathLocker()
			repo := t.TempDir()
			gitOK(t, repo, "init", "-q", "-b", "main")
			gitOK(t, repo, "config", "user.name", "Fixture")
			gitOK(t, repo, "config", "user.email", "fixture@example.test")
			if err := os.WriteFile(filepath.Join(repo, "proof.txt"), []byte("retained source"), 0600); err != nil {
				t.Fatal(err)
			}
			gitOK(t, repo, "add", "-A")
			gitOK(t, repo, "commit", "-qm", "initial")
			source := leaderReuseTestTask("scope-source-S")
			source.IsLeaderTask = false
			if scope != "issue" {
				source.IssueID = ""
				source.QuickCreatePrompt = "original task-only source"
			}
			home := t.TempDir()
			source.Agent.CustomEnv = map[string]string{"CLAUDE_CONFIG_DIR": home}
			ref, _ := json.Marshal(localDirectoryRef{LocalPath: repo, DaemonID: d.cfg.DaemonID, ExecutionMode: localDirectoryModeWorktree})
			source.ProjectResources = []ProjectResourceData{{ID: "local-repo", ResourceType: localDirectoryResourceType, ResourceRef: ref}}
			source.SessionPinGeneration = "22222222-2222-4222-8222-222222222222"
			if scope == "task_recovered_prepare" {
				old, err := execenv.Prepare(execenv.PrepareParams{WorkspacesRoot: d.cfg.WorkspacesRoot, WorkspaceID: source.WorkspaceID, TaskID: source.ID, Provider: "claude", LocalWorktree: &execenv.LocalWorktreeParams{LocalPath: repo}, Task: execenv.TaskContextForEnv{AgentID: source.AgentID, ScopeTaskID: source.ID}}, d.logger)
				if err != nil {
					t.Fatal(err)
				}
				old.ReleaseLock()
				rewriteScopeWorktreeOwner(t, old.RootDir, map[string]string{})
				source.PriorWorkDir = old.WorkDir
			}
			marker := filepath.Join(t.TempDir(), "pin-verified")
			fake := d.cfg.Agents["claude"].Path
			script, err := os.ReadFile(fake)
			if err != nil {
				t.Fatal(err)
			}
			quoted := "'" + strings.ReplaceAll(marker, "'", "'\\''") + "'"
			script = []byte(strings.Replace(string(script), "printf '%s\\n' '{\"type\":\"result\"", "while [ ! -f "+quoted+" ]; do sleep 0.01; done\nprintf '%s\\n' '{\"type\":\"result\"", 1))
			writeTestExecutable(t, fake, script)
			seenPin := make(chan struct{}, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/daemon/tasks/"+source.ID+"/session" {
					var pin struct {
						WorkDir    string `json:"work_dir"`
						Generation string `json:"generation"`
					}
					if err := json.NewDecoder(r.Body).Decode(&pin); err != nil {
						t.Error(err)
					}
					if pin.Generation != source.SessionPinGeneration {
						t.Errorf("missing v1 pin generation: %+v", pin)
					}
					data, err := os.ReadFile(filepath.Join(filepath.Dir(pin.WorkDir), ".local_worktree.json"))
					if err != nil {
						t.Error(err)
					}
					var saved struct {
						Owner       struct{ WorkspaceID, AgentID, ConversationID string }
						TracksState bool `json:"tracks_state"`
					}
					if err := json.Unmarshal(data, &saved); err != nil {
						t.Error(err)
					}
					expectedScope := source.IssueID
					if expectedScope == "" {
						expectedScope = source.ID
					}
					if saved.Owner.WorkspaceID != source.WorkspaceID || saved.Owner.AgentID != source.AgentID || saved.Owner.ConversationID != expectedScope {
						t.Errorf("owner was not persisted before v1 pin: %+v", saved)
					}
					if scope != "issue" && saved.TracksState {
						t.Error("task branch started tracking shared state")
					}
					if err := os.WriteFile(marker, []byte("ready"), 0600); err != nil {
						t.Error(err)
					}
					select {
					case seenPin <- struct{}{}:
					default:
					}
					w.WriteHeader(http.StatusNoContent)
					return
				}
				w.Write([]byte(`{}`))
			}))
			defer srv.Close()
			d.client = NewClient(srv.URL)
			first, err := d.runTask(context.Background(), source, "claude", 0, d.logger)
			if err != nil || first.Status != "completed" {
				t.Fatalf("actual source execution/finalization failed: %+v %v", first, err)
			}
			select {
			case <-seenPin:
			case <-time.After(time.Second):
				t.Fatal("source v1 pin was not observed")
			}
			if _, err := os.Stat(first.WorkDir); err != nil {
				t.Fatal(err)
			}
			descriptor, err := os.ReadFile(filepath.Join(first.EnvRoot, ".local_worktree.json"))
			if err != nil {
				t.Fatal(err)
			}
			var metadata map[string]any
			if err := json.Unmarshal(descriptor, &metadata); err != nil {
				t.Fatal(err)
			}
			t.Logf("scope=%s persisted owner=%v tracks_state=%v finalized_tip=%v", scope, metadata["owner"], metadata["tracks_state"], metadata["finalized_tip"])
			project := filepath.Join(home, "projects", "source")
			if err := os.MkdirAll(project, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(project, first.SessionID+".jsonl"), []byte("{}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			turn := resultTurnTask("return-R", source, first)
			turn.AuthToken = "mat_return_probe"
			if scope == "task_linked" {
				// LinkTaskToIssue changes the source row after acceptance. The claim's
				// frozen task namespace remains S, not that later issue association.
				source.IssueID = "created-issue-after-source-completion"
				if turn.IssueID != "" || turn.DelegationResult.ScopeTaskID != source.ID {
					t.Fatal("claim no longer carries original task scope")
				}
			}
			if scope == "empty_owner" {
				rewriteScopeWorktreeOwner(t, first.EnvRoot, map[string]string{})
			}
			if scope == "conflicting_owner" {
				rewriteScopeWorktreeOwner(t, first.EnvRoot, map[string]string{"WorkspaceID": source.WorkspaceID, "AgentID": "another-agent", "ConversationID": source.ID})
			}

			returned, err := d.runTask(context.Background(), turn, "claude", 0, d.logger)
			args, readErr := os.ReadFile(argsFile)
			if readErr != nil {
				t.Fatal(readErr)
			}
			invocations := strings.Count(string(args), "--invocation-end--")
			t.Logf("strict return status=%s reason=%s native_invocations=%d", returned.Status, returned.FailureReason, invocations)
			if scope == "empty_owner" || scope == "conflicting_owner" {
				if err != nil || returned.FailureReason != "delegation_result_unavailable" || invocations != 1 {
					t.Fatalf("unknown ownership started input: %+v %v invocations=%d", returned, err, invocations)
				}
				if _, err := os.Stat(first.WorkDir); err != nil {
					t.Fatal("unproven source was deleted", err)
				}
				return
			}
			if err != nil || returned.Status != "completed" || invocations != 2 || !sameDir(t, first.WorkDir, returned.WorkDir) {
				t.Fatalf("retained %s scope could not restore strict return: %+v %v", scope, returned, err)
			}
		})
	}
}

func rewriteScopeWorktreeOwner(t *testing.T, root string, owner map[string]string) {
	t.Helper()
	file := filepath.Join(root, ".local_worktree.json")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata["owner"] = owner
	data, err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
}

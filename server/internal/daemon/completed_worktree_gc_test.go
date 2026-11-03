package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestCompletedRemovedWorktreeGC(t *testing.T) {
	for _, mode := range []string{"acknowledged", "no_edits", "no_edits_repo_replaced", "response_lost", "unconfirmed", "storage_missing", "permission_unknown", "receipt_unavailable", "address_changed", "finalization_missing", "replacement_directory", "branch_missing", "linked_scope", "linked_owner_mismatch", "linked_provenance_missing"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			d, _, cleanup := newLeaderReuseTestDaemon(t)
			defer cleanup()
			d.cfg.DaemonID = "completed-gc"
			d.localPathLocks = NewLocalPathLocker()
			repo := t.TempDir()
			gitOK(t, repo, "init", "-q", "-b", "main")
			gitOK(t, repo, "config", "user.name", "Fixture")
			gitOK(t, repo, "config", "user.email", "fixture@example.test")
			if err := os.WriteFile(filepath.Join(repo, "original.txt"), []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			gitOK(t, repo, "add", "-A")
			gitOK(t, repo, "commit", "-qm", "initial")
			if !strings.HasPrefix(mode, "no_edits") {
				fake := d.cfg.Agents["claude"].Path
				script, err := os.ReadFile(fake)
				if err != nil {
					t.Fatal(err)
				}
				script = []byte(strings.Replace(string(script), "IFS= read -r _", "IFS= read -r _\nprintf 'saved artifact' > result.txt", 1))
				writeTestExecutable(t, fake, script)
			}
			task := leaderReuseTestTask("completed-gc")
			task.IsLeaderTask = false
			if strings.HasPrefix(mode, "linked_") {
				task.IssueID = ""
				task.QuickCreatePrompt = "Save the fixture work"
			}
			ref, _ := json.Marshal(localDirectoryRef{LocalPath: repo, DaemonID: d.cfg.DaemonID, ExecutionMode: localDirectoryModeWorktree})
			task.ProjectResources = []ProjectResourceData{{ID: "directory", ResourceType: localDirectoryResourceType, ResourceRef: ref}}
			var mu sync.Mutex
			var terminalReport json.RawMessage
			var completed TaskResult
			var receipt protocol.ResourceReleaseReceipt
			var receiptCalls int
			collecting := false
			d.runner = taskRunnerFunc(func(ctx context.Context, task Task, provider string, slot int, logger *slog.Logger) (TaskResult, error) {
				result, err := d.runTask(ctx, task, provider, slot, logger)
				mu.Lock()
				completed = result
				mu.Unlock()
				return result, err
			})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case strings.HasSuffix(r.URL.Path, "/complete"):
					if err := json.NewDecoder(r.Body).Decode(&terminalReport); err != nil {
						t.Error(err)
					}
					if _, err := os.Stat(completed.WorkDir); err != nil {
						t.Errorf("removed before terminal report: %v", err)
					}
					w.Write([]byte(`{}`))
				case r.URL.Path == "/api/daemon/resources/release":
					if len(terminalReport) == 0 {
						t.Error("release preceded terminal report")
					}
					if collecting && mode == "permission_unknown" {
						http.Error(w, "dependencies unavailable", 503)
						return
					}
					json.NewEncoder(w).Encode(protocol.ResourceReleaseDecision{AdmissionClosed: true, ControlRequired: true, ReleaseAllowed: true, ProtectedResources: []protocol.ManagedResourceAddress{}})
				case strings.HasSuffix(r.URL.Path, "/resources-released"):
					var got protocol.ResourceReleaseReceipt
					if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
						t.Error(err)
					}
					if _, err := os.Lstat(got.ResourcePath); !os.IsNotExist(err) {
						t.Errorf("ACK preceded actual removal: %v", err)
					}
					if got.DurableWorkDir != repo {
						t.Errorf("wrong durable path: %+v", got)
					}
					receiptCalls++
					if mode != "unconfirmed" {
						receipt = got
					}
					if mode == "unconfirmed" || mode == "response_lost" {
						http.Error(w, "response unavailable", 500)
						return
					}
					json.NewEncoder(w).Encode(protocol.ResourceReleaseAcknowledgement{Recorded: true, DurableWorkDir: got.DurableWorkDir})
				case strings.HasSuffix(r.URL.Path, "/gc-check"):
					if mode == "receipt_unavailable" {
						http.Error(w, "receipt unavailable", 500)
						return
					}
					current, err := readManagedResource(completed.EnvRoot)
					if err != nil {
						t.Error(err)
						http.Error(w, "identity", 500)
						return
					}
					a := current.Address
					identity := protocol.ManagedResourceAddress{SourceTaskID: current.OwnerTaskID, WorkspaceID: a.WorkspaceID, AgentID: a.AgentID, RuntimeID: a.RuntimeID, Provider: a.Provider, SessionID: a.SessionID, WorkDir: a.WorkDir, ScopeType: a.ScopeType, ScopeID: a.ScopeID}
					if mode == "address_changed" {
						identity.WorkDir = filepath.Join(repo, "another-location")
					}
					if mode == "linked_scope" {
						identity.ScopeType, identity.ScopeID = "issue", "later-linked-issue"
					}
					var durable *string
					if receipt.DurableWorkDir != "" {
						durable = &receipt.DurableWorkDir
					}
					now := time.Now()
					retained := false
					json.NewEncoder(w).Encode(TaskGCStatus{Status: "completed", ResourceControlRequired: true, TerminalReportedAt: &now, ResourceIdentity: &identity, ResourcesRetained: &retained, DurableWorkDir: durable})
				case strings.HasSuffix(r.URL.Path, "/status"):
					w.Write([]byte(`{"status":"running"}`))
				default:
					w.Write([]byte(`{}`))
				}
			}))
			defer srv.Close()
			d.client = NewClient(srv.URL)
			d.handleTask(context.Background(), task, 0)
			mu.Lock()
			collecting = true
			result := completed
			reportBefore := string(terminalReport)
			callsBefore := receiptCalls
			mu.Unlock()
			if result.Status != "completed" || reportBefore == "" || callsBefore != 1 {
				t.Fatalf("ordinary lifecycle did not finish: %+v report=%s ACKs=%d", result, reportBefore, callsBefore)
			}
			root := result.EnvRoot
			if _, err := os.Lstat(result.WorkDir); !os.IsNotExist(err) {
				t.Fatalf("actual Finalize/Remove did not remove checkout: %v", err)
			}
			if mode == "no_edits_repo_replaced" {
				moved := repo + "-original"
				if err := os.Rename(repo, moved); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.RemoveAll(repo); os.Rename(moved, repo) })
				if err := os.MkdirAll(repo, 0700); err != nil {
					t.Fatal(err)
				}
				gitOK(t, repo, "init", "-q", "-b", "main")
				gitOK(t, repo, "config", "user.name", "Replacement")
				gitOK(t, repo, "config", "user.email", "replacement@example.test")
				if err := os.WriteFile(filepath.Join(repo, "unrelated.txt"), []byte("unrelated"), 0600); err != nil {
					t.Fatal(err)
				}
				gitOK(t, repo, "add", "-A")
				gitOK(t, repo, "commit", "-qm", "unrelated repository")
			}
			if mode == "storage_missing" {
				moved := repo + "-offline"
				if err := os.Rename(repo, moved); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Rename(moved, repo) })
			}
			if mode == "finalization_missing" || mode == "linked_owner_mismatch" {
				file := filepath.Join(root, ".local_worktree.json")
				raw, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				var data map[string]any
				if err := json.Unmarshal(raw, &data); err != nil {
					t.Fatal(err)
				}
				if mode == "finalization_missing" {
					delete(data, "finalized_tip")
				} else {
					data["owner"] = map[string]string{"AgentID": "different-owner"}
				}
				raw, _ = json.Marshal(data)
				if err := os.WriteFile(file, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "linked_provenance_missing" {
				if err := os.Remove(filepath.Join(root, ".managed_env.json")); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "branch_missing" {
				gitOK(t, repo, "branch", "-D", result.BranchName)
			}
			if mode == "replacement_directory" {
				if err := os.MkdirAll(result.WorkDir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(result.WorkDir, "unowned.txt"), []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			meta, err := execenv.ReadGCMeta(root)
			if err != nil {
				t.Fatal(err)
			}
			restarted := New(Config{WorkspacesRoot: d.cfg.WorkspacesRoot}, slog.Default())
			restarted.client = d.client
			removed := restarted.applyGCActionForMeta(context.Background(), root, gcActionClean, &gcStats{}, meta)
			wantRemoved := mode == "acknowledged" || mode == "no_edits" || mode == "response_lost" || mode == "linked_scope"
			if (removed == 1) != wantRemoved {
				t.Fatalf("root GC=%d, want removed=%v", removed, wantRemoved)
			}
			_, rootErr := os.Lstat(root)
			if os.IsNotExist(rootErr) != wantRemoved {
				t.Fatalf("root removal=%v, want %v", rootErr, wantRemoved)
			}
			if mode != "storage_missing" {
				if _, err := os.Stat(repo); err != nil {
					t.Fatalf("user repository removed: %v", err)
				}
			}
			if wantRemoved && mode != "no_edits" {
				if got := gitOK(t, repo, "show", result.BranchName+":result.txt"); got != "saved artifact" {
					t.Fatalf("saved work lost: %q", got)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if string(terminalReport) != reportBefore || receiptCalls != callsBefore {
				t.Fatal("GC rewrote result or invented another cleanup ACK")
			}
		})
	}
}

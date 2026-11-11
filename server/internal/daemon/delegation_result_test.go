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
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func resultTurnTask(id string, source Task, result TaskResult) Task {
	task := source
	task.ID = id
	task.RequireResume = true
	task.SessionPinGeneration = "11111111-1111-4111-8111-111111111111"
	task.PriorSessionID, task.PriorWorkDir = result.SessionID, result.WorkDir
	task.DelegationResult = &protocol.DelegationResultTurn{
		ResultTaskID: "child-run", DelegationReturnID: "accepted-return", SourceTaskID: source.ID, ScopeTaskID: originalTaskScopeID(source),
		DelegatedIssueID: "child-issue", Outcome: "completed", RuntimeID: task.RuntimeID,
		Provider: "claude", SessionID: result.SessionID, WorkDir: result.WorkDir,
	}
	return task
}

func TestResultReturnResumesOriginalSessionAfterSourceCompletes(t *testing.T) {
	d, argsFile, cleanup := newLeaderReuseTestDaemon(t)
	defer cleanup()
	source := leaderReuseTestTask("source")
	result, err := d.runTask(context.Background(), source, "claude", 0, d.logger)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	project := filepath.Join(home, "projects", "source")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, result.SessionID+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source.Agent.CustomEnv = map[string]string{"CLAUDE_CONFIG_DIR": home}
	task := resultTurnTask("result-turn", source, result)
	got, err := d.runTask(context.Background(), task, "claude", 0, d.logger)
	if err != nil || got.Status != "completed" {
		t.Fatalf("return: %+v, %v", got, err)
	}
	if !sameDir(t, got.WorkDir, result.WorkDir) {
		t.Fatalf("moved source workdir: %+v", got)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(args), "--invocation-end--") != 2 || !strings.Contains(string(args), "--resume\nsession-leader-reuse\n") {
		t.Fatalf("unexpected provider executions: %s", args)
	}
}

func TestResultReturnMissingWorkdirNeverLaunchesFresh(t *testing.T) {
	d, argsFile, cleanup := newLeaderReuseTestDaemon(t)
	defer cleanup()
	source := leaderReuseTestTask("source")
	task := resultTurnTask("result-turn", source, TaskResult{SessionID: "original-session", WorkDir: t.TempDir()})
	got, err := d.runTask(context.Background(), task, "claude", 0, d.logger)
	if err != nil || got.FailureReason != "delegation_result_unavailable" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if data, err := os.ReadFile(argsFile); !os.IsNotExist(err) {
		t.Fatalf("provider was invoked: %s, %v", data, err)
	}
}

func TestResultReturnPostPromptResumeRejectionDoesNotRetry(t *testing.T) {
	d, argsFile, cleanup := newLeaderReuseTestDaemon(t)
	defer cleanup()
	source := leaderReuseTestTask("source")
	result, err := d.runTask(context.Background(), source, "claude", 0, d.logger)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	project := filepath.Join(home, "projects", "source")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, result.SessionID+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source.Agent.CustomEnv = map[string]string{"CLAUDE_CONFIG_DIR": home}
	script := `#!/bin/sh
printf '%s\n' "$@" >> "` + argsFile + `"
printf '%s\n' '--invocation-end--' >> "` + argsFile + `"
IFS= read -r _
printf '%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"No conversation found with session ID: session-leader-reuse"}'
`
	writeTestExecutable(t, d.cfg.Agents["claude"].Path, []byte(script))
	task := resultTurnTask("result-turn", source, result)
	got, err := d.runTask(context.Background(), task, "claude", 0, d.logger)
	if err != nil || got.FailureReason != "delegation_result_input_unknown" {
		t.Fatalf("got %+v, %v", got, err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(args), "--invocation-end--") != 2 {
		t.Fatalf("strict turn retried after input: %s", args)
	}
}

func TestResultReturnReusesRetainedWorktreeWithChangedProjectDirectory(t *testing.T) {
	d, _, cleanup := newLeaderReuseTestDaemon(t)
	defer cleanup()
	d.cfg.DaemonID = "daemon-result"
	d.localPathLocks = NewLocalPathLocker()
	repo := t.TempDir()
	gitOK(t, repo, "init", "-q", "-b", "main")
	gitOK(t, repo, "config", "user.name", "Test")
	gitOK(t, repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "source.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitOK(t, repo, "add", "-A")
	gitOK(t, repo, "commit", "-qm", "initial")
	source := leaderReuseTestTask("source-worktree")
	source.IsLeaderTask = false
	ref, err := json.Marshal(localDirectoryRef{LocalPath: repo, DaemonID: d.cfg.DaemonID, ExecutionMode: localDirectoryModeWorktree})
	if err != nil {
		t.Fatal(err)
	}
	source.ProjectResources = []ProjectResourceData{{ID: "project-directory", ResourceType: localDirectoryResourceType, ResourceRef: ref}}
	first, err := d.runTask(context.Background(), source, "claude", 0, d.logger)
	if err != nil || first.Status != "completed" {
		t.Fatalf("source: %+v %v", first, err)
	}
	if filepath.Base(first.WorkDir) != "worktree" {
		t.Fatalf("fixture did not run in local worktree: %+v", first)
	}
	if _, err := os.Stat(first.WorkDir); err != nil {
		t.Fatalf("source completion deleted return target: %v", err)
	}
	home := t.TempDir()
	project := filepath.Join(home, "projects", "source")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, first.SessionID+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source.Agent.CustomEnv = map[string]string{"CLAUDE_CONFIG_DIR": home}
	returned := resultTurnTask("result-turn", source, first)
	changedRef, _ := json.Marshal(localDirectoryRef{LocalPath: filepath.Join(t.TempDir(), "unavailable-new-path"), DaemonID: d.cfg.DaemonID, ExecutionMode: localDirectoryModeWorktree})
	returned.ProjectResources[0].ResourceRef = changedRef
	got, err := d.runTask(context.Background(), returned, "claude", 0, d.logger)
	if err != nil || got.Status != "completed" {
		t.Fatalf("return: %+v %v", got, err)
	}
	if !sameDir(t, got.WorkDir, first.WorkDir) {
		t.Fatalf("frozen directory moved: %+v", got)
	}
	if got.DurableWorkDir != "" {
		t.Fatalf("claimed deletion before release: %+v", got)
	}
}

func TestResultReturnBusyWorkdirWaitsOnSameTaskWithoutFreshPrompt(t *testing.T) {
	d, argsFile, cleanup := newLeaderReuseTestDaemon(t)
	defer cleanup()
	source := leaderReuseTestTask("source-busy")
	first, err := d.runTask(context.Background(), source, "claude", 0, d.logger)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	project := filepath.Join(home, "projects", "source")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, first.SessionID+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source.Agent.CustomEnv = map[string]string{"CLAUDE_CONFIG_DIR": home}
	task := resultTurnTask("same-result-task", source, first)
	ws, err := os.OpenRoot(d.cfg.WorkspacesRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	rel, _ := filepath.Rel(d.cfg.WorkspacesRoot, first.EnvRoot)
	held, _, err := execenv.LockEnvRootForReuse(ws, rel, first.EnvRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	waiting := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/wait-local-directory") {
			if !strings.Contains(r.URL.Path, "/"+task.ID+"/") {
				t.Error("busy wait changed task identity")
			}
			close(waiting)
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	d.client = NewClient(srv.URL)
	d.envRootBusyWait = 0
	d.taskPrepareTimeout = time.Nanosecond
	done := make(chan TaskResult, 1)
	go func() { got, _ := d.runTask(context.Background(), task, "claude", 0, d.logger); done <- got }()
	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("strict task did not enter resource wait")
	}
	args, _ := os.ReadFile(argsFile)
	if strings.Count(string(args), "--invocation-end--") != 1 {
		t.Fatalf("provider launched while old workdir locked: %s", args)
	}
	held.Release()
	select {
	case got := <-done:
		if got.Status != "completed" {
			t.Fatalf("same task did not resume after wait: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("strict task remained stuck after resource release")
	}
	args, _ = os.ReadFile(argsFile)
	if strings.Count(string(args), "--invocation-end--") != 2 {
		t.Fatalf("unexpected launches after resource wait: %s", args)
	}
}

func TestResultReturnUsesNewTaskCredentials(t *testing.T) {
	d, _, cleanup := newLeaderReuseTestDaemon(t)
	defer cleanup()
	source := leaderReuseTestTask("old-source")
	source.IssueID = ""
	source.QuickCreatePrompt = "old quick-create instructions"
	source.QuickCreateAttachmentIDs = []string{"old-attachment"}
	t.Setenv("MULTICA_QUICK_CREATE_TASK_ID", "ambient-old-source")
	t.Setenv("MULTICA_QUICK_CREATE_ATTACHMENT_IDS", `["ambient-old-attachment"]`)
	first, err := d.runTask(context.Background(), source, "claude", 0, d.logger)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	project := filepath.Join(home, "projects", "source")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, first.SessionID+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source.Agent.CustomEnv = map[string]string{"CLAUDE_CONFIG_DIR": home}
	identityFile := filepath.Join(t.TempDir(), "identity")
	briefFile := filepath.Join(t.TempDir(), "runtime-brief")
	script := `#!/bin/sh
printf '%s\n' "$MULTICA_TASK_ID" "$MULTICA_TOKEN" "$MULTICA_QUICK_CREATE_TASK_ID" "$MULTICA_QUICK_CREATE_ATTACHMENT_IDS" > "` + identityFile + `"
cat CLAUDE.md > "` + briefFile + `"
IFS= read -r _
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"session_id":"session-leader-reuse","result":"done"}'
`
	writeTestExecutable(t, d.cfg.Agents["claude"].Path, []byte(script))
	task := resultTurnTask("new-return-turn", source, first)
	task.AuthToken = "mat_new_return_credentials"
	got, err := d.runTask(context.Background(), task, "claude", 0, d.logger)
	if err != nil || got.Status != "completed" {
		t.Fatalf("return %+v %v", got, err)
	}
	data, err := os.ReadFile(identityFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new-return-turn\nmat_new_return_credentials\n\n\n" {
		t.Fatalf("return inherited closed source credentials: %q", data)
	}
	brief, err := os.ReadFile(briefFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, old := range []string{"Every issue turn runs the same workflow", "This is a quick-create task", "old quick-create instructions", "Final results MUST be delivered via"} {
		if strings.Contains(string(brief), old) {
			t.Fatalf("result inherited source execution workflow %q: %s", old, brief)
		}
	}
	if !strings.Contains(string(brief), "original task scope `old-source`") {
		t.Fatalf("result brief omitted original task scope: %s", brief)
	}
}

func TestOrdinaryReuseRetainsSymlinkedWorkspacesRoot(t *testing.T) {
	d, _, cleanup := newLeaderReuseTestDaemon(t)
	defer cleanup()
	link := filepath.Join(t.TempDir(), "workspace-link")
	if err := os.Symlink(d.cfg.WorkspacesRoot, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	d.cfg.WorkspacesRoot = link
	source := leaderReuseTestTask("symlink-source")
	first, err := d.runTask(context.Background(), source, "claude", 0, d.logger)
	if err != nil {
		t.Fatal(err)
	}
	next := source
	next.ID = "symlink-followup"
	next.PriorSessionID = first.SessionID
	next.PriorWorkDir = first.WorkDir
	second, err := d.runTask(context.Background(), next, "claude", 0, d.logger)
	if err != nil || !sameDir(t, first.WorkDir, second.WorkDir) {
		t.Fatalf("ordinary symlink-root followup lost original directory: %+v %v", second, err)
	}
	if _, err := d.resourceForGC(filepath.Dir(first.WorkDir)); err != nil {
		t.Fatalf("physical alias lost current ownership proof: %v", err)
	}
}

func TestResultReturnPreservesOriginalTaskScopeAcrossTurns(t *testing.T) {
	d, _, cleanup := newLeaderReuseTestDaemon(t)
	defer cleanup()
	source := leaderReuseTestTask("original-task-S")
	source.IssueID = ""
	source.QuickCreatePrompt = "original task request"
	home := t.TempDir()
	source.Agent.CustomEnv = map[string]string{"CLAUDE_CONFIG_DIR": home}
	var first TaskResult
	d.runner = taskRunnerFunc(func(ctx context.Context, task Task, provider string, slot int, logger *slog.Logger) (TaskResult, error) {
		var err error
		first, err = d.runTask(ctx, task, provider, slot, logger)
		return first, err
	})
	d.handleTask(context.Background(), source, 0)
	if first.Status != "completed" {
		t.Fatalf("source did not reach terminal: %+v", first)
	}
	if meta, err := execenv.ReadGCMeta(first.EnvRoot); err != nil || meta.TaskID != source.ID {
		t.Fatalf("source terminal metadata=%+v error=%v", meta, err)
	}
	project := filepath.Join(home, "projects", "source")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, first.SessionID+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	previous, previousResult := source, first
	for _, id := range []string{"handling-R", "handling-R2"} {
		task := resultTurnTask(id, previous, previousResult)
		task.QuickCreatePrompt = ""
		task.AuthToken = "mat_" + id
		if task.DelegationResult.ScopeTaskID != source.ID {
			t.Fatalf("source scope moved to handling task: %+v", task.DelegationResult)
		}
		result, err := d.runTask(context.Background(), task, "claude", 0, d.logger)
		if err != nil || result.Status != "completed" || !sameDir(t, first.WorkDir, result.WorkDir) {
			t.Fatalf("return changed original task namespace: %+v %v", result, err)
		}
		resource, err := readManagedResource(result.EnvRoot)
		if err != nil || resource.OwnerTaskID != id || resource.Address.ScopeType != "task" || resource.Address.ScopeID != source.ID {
			t.Fatalf("resource scope/actor conflated: %+v %v", resource, err)
		}
		if meta, ok := gcMetaForTask(task); !ok || meta.Kind != execenv.GCKindQuickCreate || meta.TaskID != id {
			t.Fatalf("handling completion has no current owner: %+v %v", meta, ok)
		}
		previous, previousResult = task, result
	}
	if err := os.RemoveAll(first.WorkDir); err != nil {
		t.Fatal(err)
	}
	missing := resultTurnTask("missing-original-environment", source, first)
	missing.QuickCreatePrompt = ""
	failed, err := d.runTask(context.Background(), missing, "claude", 0, d.logger)
	if err != nil || failed.FailureReason != protocol.DelegationResultUnavailable {
		t.Fatalf("missing original task environment did not fail visibly: %+v %v", failed, err)
	}
}

func TestResultReturnCannotDowngradeMissingBusinessScopeToTaskScope(t *testing.T) {
	d, argsFile, cleanup := newLeaderReuseTestDaemon(t)
	defer cleanup()
	source := leaderReuseTestTask("source-with-issue")
	result, err := d.runTask(context.Background(), source, "claude", 0, d.logger)
	if err != nil {
		t.Fatal(err)
	}
	task := resultTurnTask("invalid-task-scope", source, result)
	task.IssueID = ""
	task.DelegationResult.ScopeTaskID = source.ID
	got, err := d.runTask(context.Background(), task, "claude", 0, d.logger)
	if err != nil || got.FailureReason != protocol.DelegationResultUnavailable {
		t.Fatalf("missing business scope was adopted: %+v %v", got, err)
	}
	args, _ := os.ReadFile(argsFile)
	if strings.Count(string(args), "--invocation-end--") != 1 {
		t.Fatalf("invalid task scope reached provider: %s", args)
	}
}

func TestResultReturnCompletedWithoutAddressRemainsUnknown(t *testing.T) {
	d, _, cleanup := newLeaderReuseTestDaemon(t)
	defer cleanup()
	source := leaderReuseTestTask("source")
	first, err := d.runTask(context.Background(), source, "claude", 0, d.logger)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	project := filepath.Join(home, "projects", "source")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, first.SessionID+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source.Agent.CustomEnv = map[string]string{"CLAUDE_CONFIG_DIR": home}
	writeTestExecutable(t, d.cfg.Agents["claude"].Path, []byte(`#!/bin/sh
IFS= read -r _
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"observed reply without returned address"}'
`))
	got, err := d.runTask(context.Background(), resultTurnTask("result-turn", source, first), "claude", 0, d.logger)
	if err != nil || got.Status != "blocked" || got.FailureReason != protocol.DelegationResultInputUnknown || got.SessionID != "" {
		t.Fatalf("missing address was reported as received or filled from hint: %+v %v", got, err)
	}
	if !strings.Contains(got.Comment, "observed reply without returned address") {
		t.Fatalf("unconfirmed provider response was lost: %+v", got)
	}
}

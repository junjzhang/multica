package daemon

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Only the provider executable is fake. CLI, provider parsing/resume, worktree,
// admission, Auth, DB queue and accepted upstream return are production.
func TestManagedWaitCLIContinuation(t *testing.T) {
	for _, mode := range []string{"success", "nonzero", "timeout", "cancel", "restart", "task-cancel", "provider-fail"} {
		t.Run(mode, func(t *testing.T) { managedWaitCLIContinuation(t, mode) })
	}
}

func managedWaitCLIContinuation(t *testing.T, mode string) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command fixture")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("isolated DATABASE_URL required")
	}
	pool, err := pgxpool.New(t.Context(), os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f := testutil.New(pool, "", "")
	f.UserID = f.User(t, "Managed wait owner", uuid.NewString()+"@managed-wait.test")
	f.WorkspaceID = f.Workspace(t, "Managed wait integration", "managed-wait-"+uuid.NewString())
	f.Member(t, f.WorkspaceID, f.UserID, "owner")
	daemonID := "managed-wait-" + uuid.NewString()
	runtimeID := f.Runtime(t, "Managed wait runtime", testutil.Cols{"daemon_id": daemonID, "provider": "claude", "runtime_mode": "local"})
	agentID := f.Agent(t, "Managed waiter", runtimeID)
	projectID := f.Project(t, "Managed wait worktree")
	issueID := f.Issue(t, "Managed continuation", testutil.Cols{"project_id": projectID})
	sourceID, waitID := uuid.NewString(), uuid.NewString()
	dir := t.TempDir()
	root := filepath.Join(dir, "workspaces")
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	gitOK(t, repo, "init", "-q", "-b", "main")
	gitOK(t, repo, "config", "user.name", "Test")
	gitOK(t, repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "source.txt"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	gitOK(t, repo, "add", "source.txt")
	gitOK(t, repo, "commit", "-qm", "test: initial source")
	ref, _ := json.Marshal(localDirectoryRef{LocalPath: repo, DaemonID: daemonID, ExecutionMode: localDirectoryModeWorktree})
	f.Insert(t, "project_resource", testutil.Cols{"workspace_id": f.WorkspaceID, "project_id": projectID, "resource_type": "local_directory", "resource_ref": ref})
	f.Task(t, agentID, testutil.Cols{"id": sourceID, "runtime_id": runtimeID, "issue_id": issueID, "status": "queued"})
	external := f.Agent(t, "Original upstream", "", testutil.Cols{"execution_kind": "external"})
	externalSession := f.Insert(t, "external_agent_session", testutil.Cols{"workspace_id": f.WorkspaceID, "agent_id": external, "owner_user_id": f.UserID, "provider": "paseo", "host_id": "original-host", "session_id": "original-upstream-session", "daemon_id": daemonID})
	accepted := f.Insert(t, "issue_delegation_return", testutil.Cols{"workspace_id": f.WorkspaceID, "issue_id": issueID, "dispatcher_agent_id": external, "return_kind": "external_session", "external_session_id": externalSession})
	f.InsertNoID(t, "agent_task_delegation_return", testutil.Cols{"task_id": sourceID, "delegation_return_id": accepted}, "task_id=$1 AND delegation_return_id=$2", sourceID, accepted)
	f.Cleanup(t, "DELETE FROM agent_mailbox_message WHERE recipient_agent_id IN ($1,$2)", agentID, external)
	mdt, err := auth.GenerateDaemonToken()
	if err != nil {
		t.Fatal(err)
	}
	f.Insert(t, "daemon_token", testutil.Cols{"workspace_id": f.WorkspaceID, "daemon_id": daemonID, "owner_id": f.UserID, "token_hash": auth.HashToken(mdt), "expires_at": time.Now().Add(time.Hour)})
	q := db.New(pool)
	hub := realtime.NewHub()
	go hub.Run()
	h := handler.New(q, pool, hub, events.New(), nil, nil, nil, nil, handler.Config{})
	suspended := make(chan struct{}, 8)
	completed := make(chan struct{}, 8)
	released := make(chan struct{}, 8)
	router := chi.NewRouter()
	router.Group(func(r chi.Router) {
		r.Use(middleware.Auth(q, nil, nil, nil))
		r.Post("/api/background-waits/authorize", h.AuthorizeBackgroundWait)
		r.Post("/api/background-waits/{waitId}/read", h.ReadBackgroundWait)
		r.Post("/api/background-waits/{waitId}/access", h.ReadBackgroundWait)
	})
	router.Group(func(r chi.Router) {
		r.Use(middleware.DaemonAuth(q, nil, nil, nil))
		r.Post("/api/daemon/background-waits/complete", func(w http.ResponseWriter, r *http.Request) { h.CompleteBackgroundWait(w, r); completed <- struct{}{} })

		r.Get("/api/daemon/tasks/{taskId}/status", h.GetTaskStatus)
		r.Post("/api/daemon/resources/release", h.ReleaseManagedResource)
		r.Post("/api/daemon/tasks/{taskId}/resources-released", func(w http.ResponseWriter, r *http.Request) { h.RecordResourcesReleased(w, r); released <- struct{}{} })
		r.Post("/api/daemon/tasks/{taskId}/complete", func(w http.ResponseWriter, r *http.Request) { h.CompleteTask(w, r); suspended <- struct{}{} })
		r.Post("/api/daemon/tasks/{taskId}/fail", h.FailTask)
		r.Post("/api/daemon/tasks/{taskId}/start", h.StartTask)
		r.Post("/api/daemon/tasks/{taskId}/progress", h.ReportTaskProgress)
		r.Post("/api/daemon/tasks/{taskId}/usage", h.ReportTaskUsage)
		r.Post("/api/daemon/tasks/{taskId}/messages", h.ReportTaskMessages)
		r.Post("/api/daemon/tasks/{taskId}/session", h.PinTaskSession)
		r.Post("/api/daemon/runtimes/{runtimeId}/tasks/{taskId}/prepare-lease", h.ExtendTaskPrepareLease)
		r.Post("/api/daemon/runtimes/{runtimeId}/tasks/claim", h.ClaimTaskByRuntime)
		r.Post("/api/daemon/runtimes/{runtimeId}/recover-orphans", h.RecoverOrphanedTasks)
		r.Post("/api/daemon/runtimes/{runtimeId}/tasks/{taskId}/skill-bundles/resolve", h.ResolveTaskSkillBundles)
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	binary := filepath.Join(dir, "multica")
	build := exec.Command("go", "build", "-buildvcs=false", "-o", binary, "./cmd/multica")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v %s", err, out)
	}
	d := New(Config{DaemonID: daemonID, ServerBaseURL: server.URL, WorkspacesRoot: root, Agents: map[string]AgentEntry{}}, slog.New(slog.NewTextHandler(os.Stdout, nil)))
	d.executionEnvironmentCommand = func() ([]string, error) { return []string{binary, execenv.PreparationHelperArg}, nil }
	d.client.SetToken(mdt)
	d.runtimeIndex[runtimeID] = Runtime{ID: runtimeID, Provider: "claude"}
	local := httptest.NewUnstartedServer(d.backgroundWaitHandler())
	if err := d.openBackgroundWaits(t.Context(), dir, "http://"+local.Listener.Addr().String(), []string{binary, "internal-wait-exec"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.backgroundWaits.Close)
	local.Start()
	t.Cleanup(local.Close)
	_, port, _ := net.SplitHostPort(local.Listener.Addr().String())
	d.cfg.HealthPort, _ = strconv.Atoi(port)
	d.cfg.AgentTimeout = 30 * time.Second
	fake := filepath.Join(dir, "fake-claude")
	trace := filepath.Join(dir, "provider-trace")
	registration := filepath.Join(dir, "registration.json")
	requestedPrompt := filepath.Join(dir, "resumed-prompt.json")
	script := `#!/bin/sh
if [ "$1" = --version ]; then printf '2.1.0'; exit 0; fi
printf '%s\n' '--turn--' >> "$WAIT_TRACE"
resume=no
for arg in "$@"; do
  if [ "$arg" = --resume ]; then resume=yes; fi
  printf '%s\n' "$arg" >> "$WAIT_TRACE"
done
IFS= read -r prompt
printf '%s\n' '{"type":"system","session_id":"managed-provider-session"}'
if [ "$resume" = no ]; then
  printf 'retained original work' > scratch
  mkdir -p "$CLAUDE_CONFIG_DIR/projects/fixture"
  printf '{}\n' > "$CLAUDE_CONFIG_DIR/projects/fixture/managed-provider-session.jsonl"
  count=0
  until "$WAIT_CLI" wait start --request-id "$WAIT_ID" --deadline "$WAIT_DEADLINE" -- /bin/sh -c 'while [ ! -e release ]; do sleep 0.01; done; printf x >> executions; exit WAIT_EXIT' > "$WAIT_REGISTRATION" 2> "$WAIT_REGISTRATION.error"; do
    count=$((count+1))
    [ "$count" -lt 40 ] || exit 21
    sleep 0.05
  done
  if [ "$WAIT_MODE" = cancel ]; then "$WAIT_CLI" wait cancel "$WAIT_ID" > /dev/null || exit 23; fi
else
  [ "$(cat scratch)" = 'retained original work' ] || exit 22
  printf '%s\n' "$prompt" > "$WAIT_PROMPT"
fi
printf '%s\n' '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"original task progress"}]}}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"session_id":"managed-provider-session","result":"original task progress","modelUsage":{"fake-model":{"inputTokens":10,"outputTokens":2}}}'
`
	exitCode := "7"
	if mode == "success" {
		exitCode = "0"
	}
	if mode == "timeout" || mode == "cancel" {
		exitCode = "0"
		script = strings.ReplaceAll(script, "while [ ! -e release ]; do sleep 0.01; done; printf x >> executions; exit WAIT_EXIT", "printf x >> executions; sleep 30")
	}
	script = strings.ReplaceAll(script, "WAIT_EXIT", exitCode)
	script = strings.Replace(script, "-- /bin/sh -c '", "-- /bin/sh -c 'printf x >> started-executions; ", 1)
	if mode == "provider-fail" {
		script = strings.Replace(script, "else\n  [ ", "  exit 8\nelse\n  [ ", 1)
	}

	writeTestExecutable(t, fake, []byte(script))
	available := map[string]AgentEntry{"claude": {Path: fake}}
	d.agentsAvailable.Store(&available)
	deadline := time.Now().Add(2 * time.Minute)
	if mode == "timeout" {
		deadline = time.Now().Add(3 * time.Second)
	}
	custom, _ := json.Marshal(map[string]string{"WAIT_MODE": mode, "WAIT_CLI": binary, "WAIT_ID": waitID, "WAIT_TRACE": trace, "WAIT_REGISTRATION": registration, "WAIT_PROMPT": requestedPrompt, "WAIT_DEADLINE": deadline.UTC().Format(time.RFC3339Nano), "CLAUDE_CONFIG_DIR": filepath.Join(dir, "claude-home")})
	f.Exec(t, "UPDATE agent SET custom_env=$1 WHERE id=$2", custom, agentID)
	source, err := d.client.ClaimTask(t.Context(), runtimeID)
	if err != nil || source == nil {
		t.Fatalf("initial claim: %+v %v", source, err)
	}
	d.handleTask(t.Context(), *source, 0)
	if mode != "provider-fail" {
		select {
		case <-suspended:
		case <-time.After(10 * time.Second):
			t.Fatal("source turn did not suspend")
		}
	}
	row, err := q.GetAgentTask(t.Context(), mustUUID(sourceID))
	if err != nil {
		t.Fatal(err)
	}
	if mode == "provider-fail" {
		if row.Status != "failed" || row.BranchName.String == "" || strings.TrimSpace(gitOK(t, repo, "show", row.BranchName.String+":scratch")) != "retained original work" || d.backgroundWaits.Active() != 0 {
			t.Fatalf("failed provider lost branch or command: %s %s active=%d", row.Status, row.BranchName.String, d.backgroundWaits.Active())
		}
		return
	}
	if row.Status != "deferred" || row.CompletedAt.Valid {
		t.Fatalf("source turn completed too early: %+v", row)
	}
	var upstreamCount int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM agent_mailbox_message WHERE recipient_agent_id=$1", external).Scan(&upstreamCount); err != nil || upstreamCount != 0 {
		t.Fatalf("premature upstream result: %d %v", upstreamCount, err)
	}
	if mode != "cancel" {
		if next, err := d.client.ClaimTask(t.Context(), runtimeID); err != nil || next != nil {
			t.Fatalf("model polled while waiting: %+v %v", next, err)
		}
	}
	traceBytes, err := os.ReadFile(trace)
	if err != nil || strings.Count(string(traceBytes), "--turn--") != 1 {
		t.Fatalf("extra model turn while waiting: %s %v", traceBytes, err)
	}
	var started protocol.WaitStatus
	data, err := os.ReadFile(registration)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &started); err != nil {
		t.Fatal(err)
	}
	workdir := started.Address.WorkDir
	if filepath.Base(workdir) != "worktree" {
		t.Fatalf("fixture did not use managed worktree: %s", workdir)
	}
	if !strings.Contains(gitOK(t, workdir, "status", "--porcelain"), "scratch") {
		t.Fatal("suspension finalized in-flight work")
	}
	if mode == "task-cancel" {
		if _, err := h.TaskService.CancelTask(t.Context(), mustUUID(sourceID)); err != nil {
			t.Fatal(err)
		}
		decision, err := d.client.ReleaseResource(t.Context(), protocol.ResourceReleaseRequest{Kind: "task", WorkspaceID: f.WorkspaceID, SourceTaskID: sourceID})
		if err != nil || decision.ReleaseAllowed {
			t.Fatalf("cancel released a live command: %+v %v", decision, err)
		}
		select {
		case <-released:
		case <-time.After(15 * time.Second):
			t.Fatal("cancelled wait resources not finalized")
		}
		row, err = q.GetAgentTask(t.Context(), mustUUID(sourceID))
		if err != nil || row.Status != "cancelled" || !row.TerminalReportedAt.Valid || d.backgroundWaits.Active() != 0 || strings.TrimSpace(gitOK(t, repo, "show", row.BranchName.String+":scratch")) != "retained original work" {
			t.Fatalf("task cancellation lost work: %s branch=%s active=%d %v", row.Status, row.BranchName.String, d.backgroundWaits.Active(), err)
		}
		if _, err := os.Stat(workdir); !os.IsNotExist(err) {
			t.Fatalf("cancelled worktree retained: %v", err)
		}
		return
	}
	if mode == "restart" {
		d.backgroundWaits.Close()
		if err := d.openBackgroundWaits(t.Context(), dir, "http://"+local.Listener.Addr().String(), []string{binary, "internal-wait-exec"}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(d.backgroundWaits.Close)
		if err := d.client.RecoverOrphans(t.Context(), runtimeID); err != nil {
			t.Fatal(err)
		}
	}
	f.Exec(t, "UPDATE task_token SET expires_at=now()-interval '1 minute' WHERE task_id=$1", sourceID)
	if err := os.WriteFile(filepath.Join(workdir, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-completed:
	case <-time.After(15 * time.Second):
		t.Fatal("command result missing")
	}
	next, err := d.client.ClaimTask(t.Context(), runtimeID)
	if err != nil || next == nil {
		t.Fatalf("resume claim: %+v %v", next, err)
	}
	if next.ID != sourceID || next.PriorWorkDir != workdir || next.PriorSessionID != started.Address.SessionID || !next.RequireResume {
		t.Fatalf("wrong resume claim: %+v", next)
	}
	if _, ok := shouldReusePriorWorkdir(*next, nil, root); !ok {
		t.Fatalf("original worktree scope cannot be reused: %+v", next)
	}
	expected := map[string]string{"success": "succeeded", "nonzero": "failed", "timeout": "timed_out", "cancel": "cancelled", "restart": "lost"}[mode]
	if next.BackgroundWait == nil || next.BackgroundWait.Result.State != expected {
		t.Fatalf("wrong wait result: %+v", next.BackgroundWait)
	}
	executions, err := os.ReadFile(filepath.Join(workdir, "executions"))
	if mode != "cancel" && mode != "restart" && (err != nil || string(executions) != "x") {
		t.Fatalf("command rerun or missing: %s %v", executions, err)
	}
	startedExecutions, err := os.ReadFile(filepath.Join(workdir, "started-executions"))
	if err != nil || string(startedExecutions) != "x" {
		t.Fatalf("command execution count: %s %v", startedExecutions, err)
	}
	d.handleTask(t.Context(), *next, 0)
	row, err = q.GetAgentTask(t.Context(), mustUUID(sourceID))
	if err != nil || row.Status != "completed" {
		t.Fatalf("original task did not complete: %s %v", row.Status, err)
	}
	if _, err := h.TaskService.ReconcileDelegationResults(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	traceBytes, err = os.ReadFile(trace)
	if err != nil || strings.Count(string(traceBytes), "--turn--") != 2 || !strings.Contains(string(traceBytes), "--resume\nmanaged-provider-session\n") {
		t.Fatalf("provider did not resume exactly once: %s %v", traceBytes, err)
	}
	prompt, err := os.ReadFile(requestedPrompt)
	if err != nil || !strings.Contains(string(prompt), waitID) || !strings.Contains(string(prompt), "exit_code") || !strings.Contains(string(prompt), "log_path") {
		t.Fatalf("result missing from native provider prompt: %s %v", prompt, err)
	}
	usage, err := q.GetTaskUsage(t.Context(), mustUUID(sourceID))
	if err != nil || len(usage) != 1 || usage[0].InputTokens != 20 {
		t.Fatalf("resumed usage overwrote source turn: %+v %v", usage, err)
	}
	links, err := q.ListAcceptedTaskReturns(t.Context(), db.ListAcceptedTaskReturnsParams{WorkspaceID: mustUUID(f.WorkspaceID), TaskIds: []pgtype.UUID{mustUUID(sourceID)}})
	if err != nil || len(links) != 1 || links[0].ReturnID != mustUUID(accepted) {
		t.Fatalf("accepted return changed: %+v %v", links, err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM agent_mailbox_message WHERE recipient_agent_id=$1 AND target_session_id=$2", external, externalSession).Scan(&upstreamCount); err != nil || upstreamCount != 1 {
		t.Fatalf("final upstream result: %d %v", upstreamCount, err)
	}
	if row.BranchName.String == "" || strings.TrimSpace(gitOK(t, repo, "show", row.BranchName.String+":scratch")) != "retained original work" {
		t.Fatal("original work was not delivered on original branch")
	}
	if _, err := os.Stat(workdir); !os.IsNotExist(err) {
		t.Fatalf("terminal worktree was not released: %v", err)
	}

}

func mustUUID(id string) pgtype.UUID {
	value, err := util.ParseUUID(id)
	if err != nil {
		panic(err)
	}
	return value
}

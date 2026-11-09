package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// The provider is a test-created task runner. CLI, local admission, Auth,
// suspension/outbox, DB claim/start/complete and upstream return are production.
func TestManagedWaitCLIContinuation(t *testing.T) {
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
	issueID := f.Issue(t, "Managed continuation")
	sourceID, generation, waitID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	dir := t.TempDir()
	root := filepath.Join(dir, "workspaces")
	workdir := filepath.Join(root, f.WorkspaceID, sourceID, "workdir")
	if err := os.MkdirAll(workdir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "scratch"), []byte("retained original work"), 0600); err != nil {
		t.Fatal(err)
	}
	f.Task(t, agentID, testutil.Cols{"id": sourceID, "runtime_id": runtimeID, "issue_id": issueID, "status": "running", "session_id": "managed-provider-session", "work_dir": workdir, "resource_control_required": true, "session_pin_confirmed": true, "session_pin_generation": generation})
	external := f.Agent(t, "Original upstream", "", testutil.Cols{"execution_kind": "external"})
	externalSession := f.Insert(t, "external_agent_session", testutil.Cols{"workspace_id": f.WorkspaceID, "agent_id": external, "owner_user_id": f.UserID, "provider": "paseo", "host_id": "original-host", "session_id": "original-upstream-session", "daemon_id": daemonID})
	accepted := f.Insert(t, "issue_delegation_return", testutil.Cols{"workspace_id": f.WorkspaceID, "issue_id": issueID, "dispatcher_agent_id": external, "return_kind": "external_session", "external_session_id": externalSession})
	f.InsertNoID(t, "agent_task_delegation_return", testutil.Cols{"task_id": sourceID, "delegation_return_id": accepted}, "task_id=$1 AND delegation_return_id=$2", sourceID, accepted)
	f.Cleanup(t, "DELETE FROM agent_mailbox_message WHERE recipient_agent_id IN ($1,$2)", agentID, external)
	mat, err := auth.GenerateAgentTaskToken()
	if err != nil {
		t.Fatal(err)
	}
	f.Insert(t, "task_token", testutil.Cols{"task_id": sourceID, "agent_id": agentID, "workspace_id": f.WorkspaceID, "user_id": f.UserID, "token_hash": auth.HashToken(mat), "expires_at": time.Now().Add(time.Hour)})
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
	router := chi.NewRouter()
	router.Group(func(r chi.Router) {
		r.Use(middleware.Auth(q, nil, nil, nil))
		r.Post("/api/background-waits/authorize", h.AuthorizeBackgroundWait)
		r.Post("/api/background-waits/{waitId}/read", h.ReadBackgroundWait)
	})
	router.Group(func(r chi.Router) {
		r.Use(middleware.DaemonAuth(q, nil, nil, nil))
		r.Post("/api/daemon/background-waits/complete", func(w http.ResponseWriter, r *http.Request) { h.CompleteBackgroundWait(w, r); completed <- struct{}{} })
		r.Post("/api/daemon/tasks/{taskId}/background-wait/suspend", func(w http.ResponseWriter, r *http.Request) { h.SuspendBackgroundWait(w, r); suspended <- struct{}{} })
		r.Get("/api/daemon/tasks/{taskId}/status", h.GetTaskStatus)
		r.Post("/api/daemon/tasks/{taskId}/complete", h.CompleteTask)
		r.Post("/api/daemon/tasks/{taskId}/fail", h.FailTask)
		r.Post("/api/daemon/tasks/{taskId}/start", h.StartTask)
		r.Post("/api/daemon/tasks/{taskId}/progress", h.ReportTaskProgress)
		r.Post("/api/daemon/runtimes/{runtimeId}/tasks/claim", h.ClaimTaskByRuntime)
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	binary := filepath.Join(dir, "multica")
	build := exec.Command("go", "build", "-buildvcs=false", "-o", binary, "./cmd/multica")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v %s", err, out)
	}
	d := New(Config{DaemonID: daemonID, ServerBaseURL: server.URL, WorkspacesRoot: root, Agents: map[string]AgentEntry{}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	runCLI := func(token string, args ...string) protocol.WaitStatus {
		t.Helper()
		cmd := exec.Command(binary, args...)
		cmd.Dir = workdir
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "MULTICA_") && !strings.HasPrefix(entry, "PASEO_") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, "MULTICA_TOKEN="+token, "MULTICA_AGENT_ID="+agentID, "MULTICA_TASK_ID="+sourceID, "MULTICA_WORKSPACE_ID="+f.WorkspaceID, "MULTICA_SERVER_URL="+server.URL, "MULTICA_DAEMON_PORT="+port, "MULTICA_TASK_CONFIG_ROOT="+filepath.Join(dir, "private-task-config"))
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("CLI %v: %v %s", args, err, stderr.String())
		}
		var status protocol.WaitStatus
		if err := json.Unmarshal(out, &status); err != nil {
			t.Fatalf("response: %s %v", out, err)
		}
		return status
	}
	var turns atomic.Int32
	d.runner = taskRunnerFunc(func(ctx context.Context, task Task, provider string, _ int, _ *slog.Logger) (TaskResult, error) {
		d.registerActiveRepoCheckoutTask(task.AuthToken, activeRepoCheckoutTask{WorkspaceID: f.WorkspaceID, TaskID: task.ID, AgentID: agentID, WorkDir: workdir})
		defer d.clearActiveRepoCheckoutTask(task.AuthToken)
		if turns.Add(1) == 1 {
			deadline := time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
			status := runCLI(task.AuthToken, "wait", "start", "--request-id", waitID, "--deadline", deadline, "--", "/bin/sh", "-c", "while [ ! -e release ]; do sleep 0.01; done; printf x >> executions; exit 7")
			if status.ID != waitID || status.Address.TaskID != sourceID || status.State != "running" {
				t.Fatalf("registration: %+v", status)
			}
		} else {
			if err := validateDelegationResultTask(task, provider); err != nil {
				t.Fatal(err)
			}
			if task.ID != sourceID || !task.RequireResume || task.PriorSessionID != "managed-provider-session" || task.PriorWorkDir != workdir {
				t.Fatalf("retargeted resume: %+v", task)
			}
			if !strings.Contains(BuildPrompt(task, provider), `"exit_code":7`) {
				t.Fatal("result missing from resumed prompt")
			}
			if _, err := d.client.StartTask(ctx, task); err != nil {
				return TaskResult{}, err
			}
			if err := d.backgroundWaits.MarkHandled(task.ID, waitID); err != nil {
				return TaskResult{}, err
			}
		}
		return TaskResult{Status: "completed", Comment: "original work complete", SessionID: "managed-provider-session", WorkDir: workdir}, nil
	})
	source := Task{ID: sourceID, AgentID: agentID, RuntimeID: runtimeID, WorkspaceID: f.WorkspaceID, IssueID: issueID, AuthToken: mat, SessionPinGeneration: generation, Agent: &AgentData{ID: agentID, Name: "Fake provider"}}
	d.handleTask(t.Context(), source, 0)
	select {
	case <-suspended:
	case <-time.After(10 * time.Second):
		t.Fatal("source turn did not suspend")
	}
	row, err := q.GetAgentTask(t.Context(), mustUUID(sourceID))
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "deferred" || row.CompletedAt.Valid {
		t.Fatalf("source turn completed too early: %+v", row)
	}
	var upstreamCount int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM agent_mailbox_message WHERE recipient_agent_id=$1", external).Scan(&upstreamCount); err != nil || upstreamCount != 0 {
		t.Fatalf("premature upstream result: %d %v", upstreamCount, err)
	}
	if next, err := d.client.ClaimTask(t.Context(), runtimeID); err != nil || next != nil {
		t.Fatalf("model polled while waiting: %+v %v", next, err)
	}
	if turns.Load() != 1 {
		t.Fatal("extra model turn while waiting")
	}
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
	d.handleTask(t.Context(), *next, 0)
	row, err = q.GetAgentTask(t.Context(), mustUUID(sourceID))
	if err != nil || row.Status != "completed" {
		t.Fatalf("original task did not complete: %s %v", row.Status, err)
	}
	if _, err := h.TaskService.ReconcileDelegationResults(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	if turns.Load() != 2 {
		t.Fatalf("turns=%d", turns.Load())
	}
	links, err := q.ListAcceptedTaskReturns(t.Context(), db.ListAcceptedTaskReturnsParams{WorkspaceID: mustUUID(f.WorkspaceID), TaskIds: []pgtype.UUID{mustUUID(sourceID)}})
	if err != nil || len(links) != 1 || links[0].ReturnID != mustUUID(accepted) {
		t.Fatalf("accepted return changed: %+v %v", links, err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM agent_mailbox_message WHERE recipient_agent_id=$1 AND target_session_id=$2", external, externalSession).Scan(&upstreamCount); err != nil || upstreamCount != 1 {
		t.Fatalf("final upstream result: %d %v", upstreamCount, err)
	}
	data, err := os.ReadFile(filepath.Join(workdir, "executions"))
	if err != nil || string(data) != "x" {
		t.Fatalf("command rerun: %s %v", data, err)
	}
}

func mustUUID(id string) pgtype.UUID {
	value, err := util.ParseUUID(id)
	if err != nil {
		panic(err)
	}
	return value
}

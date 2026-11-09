package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/backgroundwait"
	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Uses a built CLI, production local daemon API, real Auth/DaemonAuth, isolated
// DB, production delivery worker, and a test-created Paseo executable.
func TestBackgroundWaitCLIToMailboxAndFrozenHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX host fixture")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("isolated DATABASE_URL required")
	}
	pool, err := pgxpool.New(t.Context(), os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	f := testutil.New(pool, "", "")
	f.UserID = f.User(t, "Wait owner", uuid.NewString()+"@wait.test")
	f.WorkspaceID = f.Workspace(t, "Wait integration", "wait-"+uuid.NewString())
	f.Member(t, f.WorkspaceID, f.UserID, "owner")
	daemonID := "wait-" + uuid.NewString()
	f.Runtime(t, "Wait execution daemon", testutil.Cols{"daemon_id": daemonID})
	agentID := f.Agent(t, "Wait external session", "", testutil.Cols{"execution_kind": "external"})
	sessionID := f.Insert(t, "external_agent_session", testutil.Cols{"workspace_id": f.WorkspaceID, "agent_id": agentID, "owner_user_id": f.UserID, "provider": "paseo", "host_id": "isolated-host", "session_id": "chosen-host-session", "daemon_id": daemonID})
	f.Insert(t, "external_agent_session", testutil.Cols{"workspace_id": f.WorkspaceID, "agent_id": agentID, "owner_user_id": f.UserID, "provider": "paseo", "host_id": "another-host", "session_id": "newer-session-must-not-receive", "daemon_id": daemonID})
	externalToken, err := auth.GenerateExternalAgentToken()
	if err != nil {
		t.Fatal(err)
	}
	f.Insert(t, "external_agent_token", testutil.Cols{"workspace_id": f.WorkspaceID, "agent_id": agentID, "owner_user_id": f.UserID, "token_hash": auth.HashToken(externalToken), "expires_at": time.Now().Add(time.Hour)})
	daemonToken, err := auth.GenerateDaemonToken()
	if err != nil {
		t.Fatal(err)
	}
	f.Insert(t, "daemon_token", testutil.Cols{"workspace_id": f.WorkspaceID, "daemon_id": daemonID, "owner_id": f.UserID, "token_hash": auth.HashToken(daemonToken), "expires_at": time.Now().Add(time.Hour)})
	q := db.New(pool)
	hub := realtime.NewHub()
	go hub.Run()
	h := handler.New(q, pool, hub, events.New(), nil, nil, nil, nil, handler.Config{})
	var authorizeCalls atomic.Int32
	completed := make(chan string, 8)
	router := chi.NewRouter()
	router.Group(func(r chi.Router) {
		r.Use(middleware.Auth(q, nil, nil, nil))
		r.Post("/api/background-waits/authorize", func(w http.ResponseWriter, req *http.Request) {
			authorizeCalls.Add(1)
			h.AuthorizeBackgroundWait(w, req)
		})
		r.Get("/api/agents/{id}/mailbox", h.ListAgentMailbox)
	})
	router.Group(func(r chi.Router) {
		r.Use(middleware.DaemonAuth(q, nil, nil, nil))
		r.Post("/api/daemon/background-waits/complete", func(w http.ResponseWriter, req *http.Request) {
			recorder := httptest.NewRecorder()
			h.CompleteBackgroundWait(recorder, req)
			for k, v := range recorder.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(recorder.Body.Bytes())
			if recorder.Code == 200 {
				var out struct {
					ID string `json:"delivery_id"`
				}
				_ = json.Unmarshal(recorder.Body.Bytes(), &out)
				completed <- out.ID
			}
		})
		r.Post("/api/daemon/deliveries/claim", h.ClaimDaemonDeliveries)
		r.Post("/api/daemon/deliveries/{deliveryId}/outcome", h.RecordDaemonDeliveryOutcome)
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	dir := t.TempDir()
	binary := filepath.Join(dir, "multica")
	build := exec.Command("go", "build", "-o", binary, "./cmd/multica")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v %s", err, out)
	}
	d := &Daemon{cfg: Config{DaemonID: daemonID, ServerBaseURL: server.URL}, client: NewClient(server.URL)}
	d.client.SetToken(daemonToken)
	local := httptest.NewUnstartedServer(d.backgroundWaitHandler())
	if err := d.openBackgroundWaits(t.Context(), dir, "http://"+local.Listener.Addr().String(), []string{binary, "internal-wait-exec"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.backgroundWaits.Close)
	local.Start()
	t.Cleanup(local.Close)
	contextPath := filepath.Join(dir, "session.json")
	bound := map[string]any{"version": 1, "provider": "paseo", "host_id": "isolated-host", "session_id": "chosen-host-session", "agent_id": agentID, "daemon_id": daemonID, "server_url": server.URL, "workspace_id": f.WorkspaceID, "token": externalToken, "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)}
	if err := backgroundwait.WritePrivate(contextPath, bound); err != nil {
		t.Fatal(err)
	}
	runCLI := func(args ...string) protocol.WaitStatus {
		t.Helper()
		cmd := exec.Command(binary, args...)
		cmd.Dir = dir
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "MULTICA_") && !strings.HasPrefix(entry, "PASEO_") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, "MULTICA_EXTERNAL_SESSION_FILE="+contextPath, cli.TaskConfigRootEnv+"="+dir)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("CLI %v: %v %s %s", args, err, out, stderr.String())
		}
		var result protocol.WaitStatus
		if err := json.Unmarshal(out, &result); err != nil {
			t.Fatalf("CLI response: %s %v", out, err)
		}
		return result
	}
	requestID := uuid.NewString()
	deadline := time.Now().Add(20 * time.Second).UTC().Format(time.RFC3339Nano)
	args := []string{"wait", "start", "--request-id", requestID, "--deadline", deadline, "--cwd", dir, "--", "/bin/sh", "-c", "sleep 0.4; printf x >> executions; exit 7"}
	started := runCLI(args...)
	if started.ID != requestID || started.State != "running" {
		t.Fatalf("registration waited for command: %+v", started)
	}
	// No caller needs to query the wait after registration. API authorization
	// is not polled during execution; the result arrives through the outbox.
	runCLI(args...)
	var deliveryID string
	select {
	case deliveryID = <-completed:
	case <-time.After(25 * time.Second):
		t.Fatal("no result enqueue")
	}
	if authorizeCalls.Load() != 2 {
		t.Fatalf("unexpected authorization polling: %d", authorizeCalls.Load())
	}
	f.Cleanup(t, "DELETE FROM agent_mailbox_message WHERE recipient_agent_id=$1", agentID)
	status := runCLI("wait", "get", requestID)
	if status.State != "failed" || status.ExitCode == nil || *status.ExitCode != 7 {
		t.Fatalf("status = %+v", status)
	}
	var target string
	var count int
	if err := pool.QueryRow(t.Context(), "SELECT target_session_id FROM agent_mailbox_message WHERE id=$1", deliveryID).Scan(&target); err != nil || target != sessionID {
		t.Fatalf("target = %s err=%v", target, err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM agent_task_queue t JOIN agent a ON a.id=t.agent_id WHERE a.workspace_id=$1", f.WorkspaceID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("fabricated %d managed runs: %v", count, err)
	}
	prompt := acceptIsolatedAssignmentRequests(t, server.URL, daemonToken, f.WorkspaceID, daemonID, 1)
	for _, required := range []string{requestID, "background_wait", `"state":"failed"`, `"exit_code":7`} {
		if !strings.Contains(prompt, required) {
			t.Errorf("host prompt missing %s: %s", required, prompt)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "executions"))
	if err != nil || string(data) != "x" {
		t.Fatalf("duplicate execution: %q %v", data, err)
	}
	if strings.Contains(prompt, externalToken) || strings.Contains(prompt, "sleep 0.4") {
		t.Fatal("credentials or command leaked into host prompt")
	}
	t.Log(fmt.Sprintf("CLI -> local daemon -> external auth -> command exit 7 -> daemon auth -> DB delivery %s -> exact fake Paseo session; no model polling or managed runs", deliveryID))
}

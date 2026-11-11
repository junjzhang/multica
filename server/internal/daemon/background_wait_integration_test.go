package daemon

import (
	"bytes"
	"context"
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
	"github.com/multica-ai/multica/server/internal/delivery"
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
	f.Insert(t, "external_agent_token", testutil.Cols{"workspace_id": f.WorkspaceID, "agent_id": agentID, "owner_user_id": f.UserID, "token_hash": auth.HashToken(externalToken), "external_session_id": sessionID, "expires_at": time.Now().Add(time.Hour)})
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
	build := exec.Command("go", "build", "-buildvcs=false", "-o", binary, "./cmd/multica")
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
	deadline := time.Now().Add(60 * time.Second).UTC().Format(time.RFC3339Nano)
	args := []string{"wait", "start", "--request-id", requestID, "--deadline", deadline, "--cwd", dir, "--", "/bin/sh", "-c", "while [ ! -e release ]; do sleep 0.01; done; printf x >> executions; exit 7"}
	started := runCLI(args...)
	if started.ID != requestID || started.State != "running" {
		t.Fatalf("registration waited for command: %+v", started)
	}
	if d.trySetClaimBarrier() {
		d.releaseClaimBarrier()
		t.Fatal("automatic reload admitted while a wait was active")
	}
	if got := d.tryBeginServerUpdate(t.Context()); got != serverUpdateRuntimeBusy {
		d.releaseClaimBarrier()
		d.updating.Store(false)
		t.Fatalf("server update admitted while a wait was active: %v", got)
	}
	// No caller needs to query the wait after registration. API authorization
	// is not polled during execution; the result arrives through the outbox.
	runCLI(args...)
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
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
	if !d.trySetClaimBarrier() {
		t.Fatal("completed wait still blocked reload")
	}
	var endpoint protocol.WaitEndpoint
	endpointBytes, err := os.ReadFile(filepath.Join(dir, "wait.endpoint.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(endpointBytes, &endpoint); err != nil {
		t.Fatal(err)
	}
	blockedID := uuid.NewString()
	body, _ := json.Marshal(protocol.WaitStart{ID: blockedID, WorkspaceID: f.WorkspaceID, SessionRef: "chosen-host-session", Argv: []string{"/bin/sh", "-c", "touch must-not-start"}, Cwd: dir, Deadline: time.Now().Add(time.Minute)})
	req, err := http.NewRequest(http.MethodPost, local.URL+"/waits", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+endpoint.Secret)
	req.Header.Set("X-Wait-Actor-Token", externalToken)
	response, err := local.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 503 {
		t.Fatalf("registration crossed restart barrier: %d", response.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(dir, "waits", blockedID+".json")); !os.IsNotExist(err) {
		t.Fatal("blocked request was journaled or executed")
	}
	d.releaseClaimBarrier()
	if got := d.tryBeginServerUpdate(t.Context()); got != serverUpdateAcquired {
		t.Fatalf("completed wait still blocked server update: %v", got)
	}
	d.releaseClaimBarrier()
	d.updating.Store(false)
	var target string
	var count int
	if err := pool.QueryRow(t.Context(), "SELECT target_session_id FROM agent_mailbox_message WHERE id=$1", deliveryID).Scan(&target); err != nil || target != sessionID {
		t.Fatalf("target = %s err=%v", target, err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM agent_task_queue t JOIN agent a ON a.id=t.agent_id WHERE a.workspace_id=$1", f.WorkspaceID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("fabricated %d managed runs: %v", count, err)
	}
	prompt := acceptWaitAfterBusyAndUnknown(t, pool, server.URL, daemonToken, f.WorkspaceID, daemonID, deliveryID)
	for _, required := range []string{requestID, "background_wait", `"state":"failed"`, `"exit_code":7`} {
		if !strings.Contains(prompt, required) {
			t.Errorf("host prompt missing %s: %s", required, prompt)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "executions"))
	if err != nil || string(data) != "x" {
		t.Fatalf("duplicate execution: %q %v", data, err)
	}
	if strings.Contains(prompt, externalToken) || strings.Contains(prompt, "printf x >> executions") {
		t.Fatal("credentials or command leaked into host prompt")
	}
	t.Log(fmt.Sprintf("CLI -> local daemon -> external auth -> command exit 7 -> daemon auth -> DB delivery %s -> exact fake Paseo session; no model polling or managed runs", deliveryID))
}

func acceptWaitAfterBusyAndUnknown(t *testing.T, pool *pgxpool.Pool, serverURL, token, workspaceID, daemonID, deliveryID string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MULTICA_WAIT_HOST_FIXTURE", dir)
	fake := filepath.Join(dir, "fake-paseo")
	script := `#!/bin/sh
if [ "$1" = status ]; then printf '{"serverId":"isolated-host"}'; exit 0; fi
if [ "$1" = agent ] && [ "$2" = receipt ]; then
  [ "$3" = chosen-host-session ] || exit 17
  printf 'receipt %s\n' "$4" >> "$MULTICA_WAIT_HOST_FIXTURE/trace"
  if [ ! -e "$MULTICA_WAIT_HOST_FIXTURE/first-receipt" ]; then
    touch "$MULTICA_WAIT_HOST_FIXTURE/first-receipt"
    printf '{"agentId":"chosen-host-session","messageId":"%s","status":"pending"}' "$4"
    exit 0
  fi
  printf '{"agentId":"chosen-host-session","messageId":"%s","status":"accepted"}' "$4"
  exit 0
fi
[ "$1" = send ] && [ "$2" = chosen-host-session ] || exit 18
shift 2
idle=no
while [ "$#" -gt 0 ]; do
  case "$1" in
    --if-idle) idle=yes ;;
    --message-id) shift; message="$1" ;;
    --prompt-file) shift; prompt="$1" ;;
  esac
  shift
done
[ "$idle" = yes ] || exit 19
printf 'send %s\n' "$message" >> "$MULTICA_WAIT_HOST_FIXTURE/trace"
if [ ! -e "$MULTICA_WAIT_HOST_FIXTURE/busy" ]; then
  touch "$MULTICA_WAIT_HOST_FIXTURE/busy"
  printf '{"error":{"code":"agent_busy"}}' >&2
  exit 1
fi
cat "$prompt" >> "$MULTICA_WAIT_HOST_FIXTURE/prompt"
printf '{"error":{"code":"agent_request_outcome_unknown"}}' >&2
exit 1
`
	if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	client := NewClient(serverURL)
	client.SetToken(token)
	client.mailboxDaemonID = daemonID
	observed := observedAssignmentClient{Client: client, results: make(chan delivery.Delivery, 8)}
	host := &paseoDeliveryHost{transport: &paseoCliTransport{bin: fake, run: runPaseoCommand, timeout: 3 * time.Second}}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	worker := delivery.NewWorker(observed, host, func() []string { return []string{workspaceID} }, daemonID, nil)
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	for _, state := range []string{"pending", "unknown", "delivered"} {
		select {
		case result := <-observed.results:
			if result.ID != deliveryID || result.DeliveryStatus != state {
				t.Errorf("outcome = %+v, want %s for %s", result, state, deliveryID)
			}
			// Backoff durations are already tested in delivery. Advance only
			// this isolated fixture's next attempt to exercise receipt recovery.
			if state != "delivered" {
				if _, err := pool.Exec(t.Context(), "UPDATE agent_mailbox_message SET next_attempt_at=now() WHERE id=$1", deliveryID); err != nil {
					t.Error(err)
				}
			}
		case <-ctx.Done():
			t.Error("host receipt timeout")
		}
	}
	cancel()
	<-done
	trace, err := os.ReadFile(filepath.Join(dir, "trace"))
	want := fmt.Sprintf("send %s\nsend %s\nreceipt %s\nreceipt %s\n", deliveryID, deliveryID, deliveryID, deliveryID)
	if err != nil || string(trace) != want {
		t.Fatalf("busy/unknown replayed or retargeted: %q want %q err=%v", trace, want, err)
	}
	prompt, err := os.ReadFile(filepath.Join(dir, "prompt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(prompt)
}

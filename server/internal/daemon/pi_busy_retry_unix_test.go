//go:build !windows

package daemon

import (
	"context"
	"encoding/json"
	"io"
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

	"golang.org/x/sys/unix"
)

// TestRunTaskPiBusyRetryDoesNotRetireHealthySession pins the distinction
// between a permanently rejected resume and a healthy transcript that another
// execution owns temporarily. The busy attempt must still fall back to a fresh
// session for this turn, but that fallback must not remove the original JSONL
// from every future session lookup.
func TestRunTaskPiBusyRetryDoesNotRetireHealthySession(t *testing.T) {
	home := t.TempDir()
	redirectTestHome(t, home)

	priorSessionID := filepath.Join(t.TempDir(), "busy-session.jsonl")
	claim, err := os.OpenFile(priorSessionID, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open prior session: %v", err)
	}
	defer claim.Close()
	if _, err := claim.WriteString("{}\n"); err != nil {
		t.Fatalf("seed prior session: %v", err)
	}
	if err := unix.Flock(int(claim.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("lock prior session: %v", err)
	}
	defer unix.Flock(int(claim.Fd()), unix.LOCK_UN) //nolint:errcheck -- best-effort test cleanup

	fakeBin := filepath.Join(t.TempDir(), "pi")
	script := `#!/bin/sh
cat > /dev/null
printf '%s\n' '{"type":"agent_start"}'
printf '%s\n' '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"done"}}'
printf '%s\n' '{"type":"turn_end","message":{"role":"assistant","model":"test","usage":{"input":1,"output":1}}}'
`
	if err := os.WriteFile(fakeBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pi: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := &Daemon{
		client:         NewClient(srv.URL),
		logger:         logger,
		workspaces:     make(map[string]*workspaceState),
		runtimeIndex:   map[string]Runtime{"rt-pi": {ID: "rt-pi", Provider: "pi"}},
		activeEnvRoots: make(map[string]int),
		cfg: Config{
			WorkspacesRoot: t.TempDir(),
			AgentTimeout:   5 * time.Second,
			ServerBaseURL:  srv.URL,
			Agents: map[string]AgentEntry{
				"pi": {Path: fakeBin},
			},
		},
	}
	task := Task{
		ID:             "task-pi-busy",
		WorkspaceID:    "ws-pi",
		RuntimeID:      "rt-pi",
		IssueID:        "issue-pi",
		AgentID:        "agent-pi",
		AuthToken:      "mat_pi_busy",
		PriorSessionID: priorSessionID,
		Agent: &AgentData{
			ID:   "agent-pi",
			Name: "pi-agent",
		},
	}

	result, err := d.runTask(context.Background(), task, "pi", 0, logger)
	if err != nil {
		t.Fatalf("runTask: %v", err)
	}
	if result.Status != "completed" || result.Comment != "done" {
		t.Fatalf("result = %+v, want successful fresh-session retry", result)
	}
	if result.SessionID == "" || result.SessionID == priorSessionID {
		t.Fatalf("SessionID = %q, want a new session", result.SessionID)
	}
	if result.RetiredSessionID != "" {
		t.Fatalf("RetiredSessionID = %q, want empty for a temporarily busy healthy session", result.RetiredSessionID)
	}
}

func TestRunTaskPiStrictBusyWaitsWithoutFreshPrompt(t *testing.T) {
	root := t.TempDir()
	source, err := execenv.Prepare(execenv.PrepareParams{WorkspacesRoot: root, WorkspaceID: "workspace", TaskID: "source", Provider: "pi", Task: execenv.TaskContextForEnv{AgentID: "agent", IssueID: "issue"}}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	source.ReleaseLock()
	prior := filepath.Join(t.TempDir(), "original.jsonl")
	held, err := os.OpenFile(prior, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if _, err := held.WriteString("{}\n"); err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(held.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer unix.Flock(int(held.Fd()), unix.LOCK_UN)
	invoked := filepath.Join(t.TempDir(), "prompts")
	fake := filepath.Join(t.TempDir(), "pi")
	script := `#!/bin/sh
if [ "$1" = '--version' ]; then echo '1.0'; exit 0; fi
printf '%s\n' 'prompt' >> "` + invoked + `"
cat > /dev/null
printf '%s\n' '{"type":"agent_start"}'
printf '%s\n' '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"handled"}}'
printf '%s\n' '{"type":"turn_end","message":{"role":"assistant","model":"test","usage":{"input":1,"output":1}}}'
`
	writeTestExecutable(t, fake, []byte(script))
	waiting := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/progress") {
			var body struct {
				Summary string `json:"summary"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.Summary == "Waiting for the original provider session" {
				once.Do(func() { close(waiting) })
			}
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	d := New(Config{WorkspacesRoot: root, Agents: map[string]AgentEntry{"pi": {Path: fake}}, AgentTimeout: 5 * time.Second, ServerBaseURL: srv.URL}, slog.Default())
	d.executionEnvironmentCommand = nil
	d.client = NewClient(srv.URL)
	task := Task{ID: "same-result-task", AgentID: "agent", RuntimeID: "runtime", WorkspaceID: "workspace", IssueID: "issue", AuthToken: "mat_result", RequireResume: true, SessionPinGeneration: "33333333-3333-4333-8333-333333333333", PriorSessionID: prior, PriorWorkDir: source.WorkDir, Agent: &AgentData{ID: "agent", Name: "agent"}, DelegationResult: &protocol.DelegationResultTurn{ResultTaskID: "child-run", DelegationReturnID: "return", SourceTaskID: "source", DelegatedIssueID: "child", Outcome: "completed", RuntimeID: "runtime", Provider: "pi", SessionID: prior, WorkDir: source.WorkDir}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan TaskResult, 1)
	go func() { result, _ := d.runTask(ctx, task, "pi", 0, d.logger); done <- result }()
	select {
	case <-waiting:
	case result := <-done:
		t.Fatalf("busy strict session stopped before waiting: %+v", result)
	case <-ctx.Done():
		t.Fatal("busy strict session did not wait")
	}
	if _, err := os.Stat(invoked); !os.IsNotExist(err) {
		t.Fatalf("busy provider received a prompt: %v", err)
	}
	if err := unix.Flock(int(held.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.Status != "completed" || result.SessionID != prior || result.RetiredSessionID != "" {
			t.Fatalf("busy strict task changed address: %+v", result)
		}
	case <-ctx.Done():
		t.Fatal("strict task did not resume after session was released")
	}
	prompts, err := os.ReadFile(invoked)
	if err != nil || string(prompts) != "prompt\n" {
		t.Fatalf("provider prompts=%q err=%v", prompts, err)
	}
}

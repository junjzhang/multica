package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestStrictReasonixLeaseWaitsBeforeSendingOriginalInput(t *testing.T) {
	busy := filepath.Join(t.TempDir(), "busy")
	if err := os.WriteFile(busy, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	prompts := filepath.Join(t.TempDir(), "prompts")
	resumes := filepath.Join(t.TempDir(), "resumes")
	fake := filepath.Join(t.TempDir(), "reasonix")
	script := `#!/bin/sh
while IFS= read -r line; do
 id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
 case "$line" in
 *'"method":"initialize"'*) printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{}}}\n' "$id" ;;
 *'"method":"session/resume"'*)
  printf '%s\n' "$line" >> "` + resumes + `"
  if [ -f "` + busy + `" ]; then
   printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32600,"message":"session is in use; close the other Reasonix window or process first"}}\n' "$id"
   exit 0
  fi
  printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"frozen"}}\n' "$id" ;;
 *'"method":"session/new"'*) exit 91 ;;
 *'"method":"session/prompt"'*)
  printf '%s\n' "$line" >> "` + prompts + `"
  printf '{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}\n' "$id"
  exit 0 ;;
 esac
done
`
	writeTestExecutable(t, fake, []byte(script))
	backend, err := agent.New("reasonix", agent.Config{ExecutablePath: fake, Logger: slog.Default()})
	if err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/progress") {
			once.Do(func() { close(waiting) })
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	d := &Daemon{client: NewClient(srv.URL), logger: slog.Default()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type finished struct {
		result agent.Result
		err    error
	}
	done := make(chan finished, 1)
	go func() {
		result, _, err := d.executeTaskInput(ctx, backend, "the original result input", agent.ExecOptions{RequireResume: true, ResumeSessionID: "frozen", Cwd: t.TempDir(), Timeout: 5 * time.Second}, d.logger, "same-return-task", "", new(atomic.Int32))
		done <- finished{result, err}
	}()
	select {
	case <-waiting:
	case got := <-done:
		t.Fatalf("lease conflict terminated the strict turn: %+v %v", got.result, got.err)
	case <-ctx.Done():
		t.Fatal("no busy wait observed")
	}
	if data, err := os.ReadFile(prompts); !os.IsNotExist(err) {
		t.Fatalf("busy session received input: %s %v", data, err)
	}
	if err := os.Remove(busy); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil || got.result.Status != "completed" || got.result.SessionID != "frozen" {
			t.Fatalf("resume after lease release: %+v %v", got.result, got.err)
		}
	case <-ctx.Done():
		t.Fatal("same session was not retried")
	}
	data, err := os.ReadFile(prompts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "\n") != 1 || !strings.Contains(string(data), "the original result input") {
		t.Fatalf("input duplicated or changed: %s", data)
	}
	data, err = os.ReadFile(resumes)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if !strings.Contains(line, `"sessionId":"frozen"`) {
			t.Fatalf("resume changed address: %s", line)
		}
	}
}

func TestStrictDshPostInputMismatchRemainsUnknown(t *testing.T) {
	d, _, cleanup := newLeaderReuseTestDaemon(t)
	defer cleanup()
	source, err := execenv.Prepare(execenv.PrepareParams{WorkspacesRoot: d.cfg.WorkspacesRoot, WorkspaceID: "ws-leader", TaskID: "source", Provider: "dsh", Task: execenv.TaskContextForEnv{AgentID: "agent-leader", IssueID: "issue-leader"}}, d.logger)
	if err != nil {
		t.Fatal(err)
	}
	source.ReleaseLock()
	record := filepath.Join(t.TempDir(), "execute")
	fake := filepath.Join(t.TempDir(), "dsh")
	script := `#!/bin/sh
printf '%s\n' '{"v":1,"type":"ready","runtime":"dsh","plugin_version":"test","capabilities":{}}'
IFS= read -r execute
printf '%s\n' "$execute" >> "` + record + `"
printf '%s\n' '{"v":1,"type":"session","request_id":"strict-dsh-turn","session_id":"frozen","resumed":true}'
printf '%s\n' '{"v":1,"type":"result","request_id":"strict-dsh-turn","status":"failed","session_id":"different","error":{"code":"AFTER_INPUT","message":"after-prompt detail"}}'
`
	writeTestExecutable(t, fake, []byte(script))
	d.cfg.Agents = map[string]AgentEntry{"dsh": {Path: fake}}
	task := resultTurnTask("strict-dsh-turn", leaderReuseTestTask("source"), TaskResult{SessionID: "frozen", WorkDir: source.WorkDir})
	task.DelegationResult.Provider = "dsh"
	result, err := d.runTask(context.Background(), task, "dsh", 0, d.logger)
	if err != nil || result.FailureReason != protocol.DelegationResultInputUnknown {
		t.Fatalf("post-input mismatch falsely certified no input: %+v %v", result, err)
	}
	if result.SessionID != "" || !strings.Contains(result.Comment, "after-prompt detail") {
		t.Fatalf("wrong identity adopted or refusal lost: %+v", result)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "\n") != 1 || !strings.Contains(string(data), `"resume_session_id":"frozen"`) {
		t.Fatalf("strict input repeated or address changed: %s", data)
	}
}

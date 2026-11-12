//go:build agentintegration

package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDshRealRuntimeSmoke is opt-in because it calls the configured DeepSeek
// model and consumes API quota. It exercises the complete Multica backend,
// installed DSH profile, model provider, and terminal-result path.
func TestDshRealRuntimeSmoke(t *testing.T) {
	if os.Getenv("MULTICA_RUN_REAL_AGENT_SMOKE") != "1" {
		t.Skip("set MULTICA_RUN_REAL_AGENT_SMOKE=1 to run the DSH integration smoke")
	}
	path := os.Getenv("MULTICA_DSH_PATH")
	if path == "" {
		t.Fatal("MULTICA_DSH_PATH is required")
	}
	sessionRoot := t.TempDir()
	b, err := New("dsh", Config{ExecutablePath: path, TaskID: "dsh-real-smoke", Logger: slog.Default(),
		Env: map[string]string{"MULTICA_DSH_SESSION_ROOT": sessionRoot}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cwd := t.TempDir()
	model := os.Getenv("MULTICA_DSH_SMOKE_MODEL")
	if model == "" {
		model = "deepseek-official/deepseek-v4-flash"
	}
	firstBackend := b
	if previousPath := os.Getenv("MULTICA_DSH_SMOKE_PREVIOUS_PATH"); previousPath != "" {
		previousHome := os.Getenv("MULTICA_DSH_SMOKE_PREVIOUS_HOME")
		if previousHome == "" {
			t.Fatal("MULTICA_DSH_SMOKE_PREVIOUS_HOME is required when testing an upgrade")
		}
		firstBackend, err = New("dsh", Config{ExecutablePath: previousPath, TaskID: "dsh-upgrade-smoke", Logger: slog.Default(),
			Env: map[string]string{"DSH_HOME": previousHome, "MULTICA_DSH_SESSION_ROOT": sessionRoot}})
		if err != nil {
			t.Fatal(err)
		}
	}
	session, err := firstBackend.Execute(ctx, "Remember the code word PINEAPPLE. Use the shell tool to write exactly DSH_TOOL_OK to a file named smoke.txt in the working directory. Then reply with exactly DSH_MULTICA_OK and nothing else.", ExecOptions{
		Cwd: cwd, Model: model, ThinkingLevel: "off",
		Timeout: 90 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	toolCalls := map[string]string{}
	toolResults := map[string]string{}
	for message := range session.Messages {
		if message.Type == MessageToolUse {
			toolCalls[message.CallID] = message.Tool
		}
		if message.Type == MessageToolResult {
			toolResults[message.CallID] = message.Tool
		}
	}
	result := <-session.Result
	if result.Status != "completed" {
		t.Fatalf("DSH smoke failed: status=%q error=%q", result.Status, result.Error)
	}
	if strings.TrimSpace(result.Output) != "DSH_MULTICA_OK" {
		t.Fatalf("unexpected DSH output: %q", result.Output)
	}
	if result.SessionID == "" {
		t.Fatal("DSH smoke returned no session ID")
	}
	content, err := os.ReadFile(filepath.Join(cwd, "smoke.txt"))
	if err != nil || strings.TrimSpace(string(content)) != "DSH_TOOL_OK" {
		t.Fatalf("DSH shell tool did not write the expected file: content=%q error=%v", content, err)
	}
	if len(toolCalls) == 0 || len(toolCalls) != len(toolResults) {
		t.Fatalf("DSH tool events are incomplete: calls=%v results=%v", toolCalls, toolResults)
	}
	for callID, name := range toolCalls {
		if callID == "" || name == "" || toolResults[callID] != name {
			t.Fatalf("DSH tool result lost its call ID or name: calls=%v results=%v", toolCalls, toolResults)
		}
	}

	resumed, err := b.Execute(ctx, "Reply with exactly the code word from the previous turn and nothing else.", ExecOptions{
		Cwd: cwd, Model: model, ThinkingLevel: "off",
		ResumeSessionID: result.SessionID, Timeout: 90 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range resumed.Messages {
	}
	resumeResult := <-resumed.Result
	if resumeResult.Status != "completed" {
		t.Fatalf("DSH resume smoke failed: status=%q error=%q", resumeResult.Status, resumeResult.Error)
	}
	if strings.TrimSpace(resumeResult.Output) != "PINEAPPLE" {
		t.Fatalf("DSH resume lost conversation context: %q", resumeResult.Output)
	}
	if resumeResult.SessionID != result.SessionID {
		t.Fatalf("DSH resume changed session ID: %q -> %q", result.SessionID, resumeResult.SessionID)
	}
}

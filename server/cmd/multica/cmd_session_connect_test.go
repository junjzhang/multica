package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func connectProcessEnv(t *testing.T, home, serverURL string) map[string]string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test executable uses a POSIX shell")
	}
	bin := t.TempDir()
	source := writeSessionBinding(t, filepath.Join(bin, "binding.json"), sessionBinding("paseo-agent-1", serverURL, nil))
	script := `#!/bin/sh
set -eu
printf '%s\n' "$@" > "$CONNECT_ARGS"
mkdir -p "$HOME/.multica/external-sessions/paseo"
cp "$CONNECT_SOURCE" "$HOME/.multica/external-sessions/paseo/$PASEO_AGENT_ID.json"
chmod 600 "$HOME/.multica/external-sessions/paseo/$PASEO_AGENT_ID.json"
printf '%s\n' '{"state":"connected","sessionId":"paseo-agent-1"}'
`
	if err := os.WriteFile(filepath.Join(bin, "paseo"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"PASEO_AGENT_ID": "paseo-agent-1",
		"PATH":           bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"CONNECT_SOURCE": source,
		"CONNECT_ARGS":   filepath.Join(home, "rpc-args"),
	}
}

func TestSessionConnectVerifiesActor(t *testing.T) {
	for _, actor := range []string{"agent-1", "another-agent"} {
		t.Run(actor, func(t *testing.T) {
			api := newFakeAPI(t)
			api.mailboxAgentID = actor
			api.agentBody = `{"id":"agent-1","name":"External Implementer","execution_kind":"external","external_session":{"provider":"paseo","host_id":"host-1","session_id":"paseo-agent-1","daemon_id":"daemon-1"}}`
			home := t.TempDir()
			writeHumanProfile(t, home, api.server.URL)
			env := connectProcessEnv(t, home, api.server.URL)
			stdout, stderr, code := api.runCLI(t, home, env, "session", "connect", "--output", "json")
			var report sessionStatusReport
			if err := json.Unmarshal([]byte(stdout), &report); err != nil {
				t.Fatalf("report: %v; stdout=%s stderr=%s", err, stdout, stderr)
			}
			wantValid := actor == "agent-1"
			if report.Valid != wantValid || (code == 0) != wantValid {
				t.Fatalf("valid=%v code=%d stderr=%s", report.Valid, code, stderr)
			}
			args, err := os.ReadFile(env["CONNECT_ARGS"])
			if err != nil {
				t.Fatal(err)
			}
			if string(args) != "plugin\nrpc\npaseo-multica\nsession.connect\n--input-json\n{\"agentId\":\"paseo-agent-1\"}\n--json\n" {
				t.Fatalf("unexpected invocation: %s", args)
			}
			if strings.Contains(stdout+stderr, "mea_") || strings.Contains(stdout+stderr, "mul_owner_token") {
				t.Fatal("credential in output")
			}
			for _, request := range api.recorded() {
				if request.Authorization != "Bearer mea_session_token" {
					t.Fatalf("wrong credential: %s", request.Authorization)
				}
			}
		})
	}
}

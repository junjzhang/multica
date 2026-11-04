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

func TestSessionConnectRefusesOverridesBeforeRPC(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{"missing session", map[string]string{"PASEO_AGENT_ID": ""}, nil, "PASEO_AGENT_ID"},
		{"invalid session", map[string]string{"PASEO_AGENT_ID": "../other"}, nil, "PASEO_AGENT_ID"},
		{"managed agent", map[string]string{"MULTICA_AGENT_ID": "managed-agent"}, nil, "daemon-managed"},
		{"managed port", map[string]string{"MULTICA_DAEMON_PORT": "1234"}, nil, "daemon-managed"},
		{"task root", map[string]string{"MULTICA_TASK_CONFIG_ROOT": "/task-private"}, nil, "daemon-managed"},
		{"profile", nil, []string{"--profile", "owner"}, "--profile"},
		{"server flag", nil, []string{"--server-url", "https://different.test"}, "--server-url"},
		{"workspace flag", nil, []string{"--workspace-id", "other"}, "--workspace-id"},
		{"server env", map[string]string{"MULTICA_SERVER_URL": "https://different.test"}, nil, "MULTICA_SERVER_URL"},
		{"workspace env", map[string]string{"MULTICA_WORKSPACE_ID": "other"}, nil, "MULTICA_WORKSPACE_ID"},
		{"token env", map[string]string{"MULTICA_TOKEN": "mul_owner_secret"}, nil, "MULTICA_TOKEN"},
		{"return env", map[string]string{"MULTICA_RETURN_SESSION": "other"}, nil, "MULTICA_RETURN_SESSION"},
		{"return flag", nil, []string{"--return-session", "other"}, "--return-session"},
		{"foreign file", map[string]string{"MULTICA_EXTERNAL_SESSION_FILE": "/other/session.json"}, nil, "MULTICA_EXTERNAL_SESSION_FILE"},
		{"output", nil, []string{"--output", "yaml"}, "--output"},
		{"unexpected argument", nil, []string{"another-session"}, "unknown command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI(t)
			home := t.TempDir()
			writeHumanProfile(t, home, api.server.URL)
			env := connectProcessEnv(t, home, api.server.URL)
			for key, value := range tc.env {
				env[key] = value
			}
			args := append([]string{"session", "connect"}, tc.args...)
			stdout, stderr, code := api.runCLI(t, home, env, args...)
			if code == 0 || !strings.Contains(stderr, tc.want) {
				t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
			}
			if _, err := os.Stat(env["CONNECT_ARGS"]); !os.IsNotExist(err) {
				t.Fatalf("RPC ran before refusal: %v", err)
			}
			if len(api.recorded()) != 0 {
				t.Fatal("unexpected API request or human fallback")
			}
			if _, err := os.Stat(defaultSessionPath(home, "paseo-agent-1")); !os.IsNotExist(err) {
				t.Fatalf("binding changed: %v", err)
			}
		})
	}
}

func TestSessionConnectRejectsUnconfirmedRPC(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		want           string
	}{
		{"wrong session", `printf '%s' '{"state":"connected","sessionId":"other","token":"mea_secret_value"}'`, "did not confirm"},
		{"malformed", `printf '%s' 'mea_secret_value'`, "did not confirm"},
		{"unverified", `printf '%s' '{"state":"unverified"}'`, "did not confirm"},
		{"plugin missing", `printf '%s' '{"error":{"message":"Plugin is not available: paseo-multica mea_secret_value"}}' >&2; exit 1`, "ensure paseo-multica"},
		{"archived", `printf '%s' '{"error":{"message":"Request failed: [multica:session-connect] This Paseo session is archived. mea_secret_value"}}' >&2; exit 1`, "session is archived"},
		{"conflict", `printf '%s' '{"error":{"message":"[multica:session-connect] foreign workspace mea_secret_value Refusing to rebind this session"}}' >&2; exit 1`, "Refusing to rebind"},
		{"save failure recovery", `printf '%s' '{"error":{"message":"[multica:session-connect] The binding could not be saved. The newly issued credential was revoked; run /multica connect again."}}' >&2; exit 1`, "credential was revoked; run /multica connect again"},
		{"save and revoke failure", `printf '%s' '{"error":{"message":"[multica:session-connect] The binding could not be saved and the credential could not be revoked. Revoke token 33333333-3333-4333-8333-333333333333 for agent 11111111-1111-4111-8111-111111111111 from the owner profile."}}' >&2; exit 1`, "Revoke token 33333333-3333-4333-8333-333333333333 for agent 11111111-1111-4111-8111-111111111111"},
		{"Node warning before envelope", `printf '%s\n' '(node:1) ExperimentalWarning: runtime warning' >&2; printf '%s' '{"error":{"message":"[multica:session-connect] Revoke token 33333333-3333-4333-8333-333333333333 for agent 11111111-1111-4111-8111-111111111111. mea_secret_value"}}' >&2; printf '%s\n' 'another warning' >&2; exit 1`, "Revoke token 33333333-3333-4333-8333-333333333333 for agent 11111111-1111-4111-8111-111111111111"},
		{"bridge missing", `printf '%s' "error: unknown command 'rpc'" >&2; exit 1`, "lacks `plugin rpc`"},
		{"raw failure", `printf '%s' 'mea_secret_value' >&2; exit 1`, "session connect failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI(t)
			home := t.TempDir()
			env := connectProcessEnv(t, home, api.server.URL)
			bin := strings.Split(env["PATH"], string(os.PathListSeparator))[0]
			if err := os.WriteFile(filepath.Join(bin, "paseo"), []byte("#!/bin/sh\n"+tc.response+"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			stdout, stderr, code := api.runCLI(t, home, env, "session", "connect", "--output", "json")
			if code == 0 || !strings.Contains(stderr, tc.want) {
				t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
			}
			if strings.Contains(stdout+stderr, "mea_secret_value") {
				t.Fatal("RPC output leaked a credential")
			}
			if len(api.recorded()) != 0 {
				t.Fatal("unconfirmed RPC reached the API")
			}
		})
	}
}

func TestSessionConnectCanceledBeforeRPC(t *testing.T) {
	api := newFakeAPI(t)
	home := t.TempDir()
	env := connectProcessEnv(t, home, api.server.URL)
	env["MULTICA_TEST_CLI_CANCELED"] = "1"
	_, stderr, code := api.runCLI(t, home, env, "session", "connect")
	if code == 0 || !strings.Contains(stderr, "inspect `multica session status` before retrying") {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if _, err := os.Stat(env["CONNECT_ARGS"]); !os.IsNotExist(err) {
		t.Fatalf("canceled RPC ran: %v", err)
	}
}

func TestSessionConnectMissingPaseo(t *testing.T) {
	api := newFakeAPI(t)
	home := t.TempDir()
	_, stderr, code := api.runCLI(t, home, map[string]string{"PASEO_AGENT_ID": "paseo-agent-1", "PATH": t.TempDir()}, "session", "connect")
	if code == 0 || !strings.Contains(stderr, "Paseo CLI was not found on PATH") {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if len(api.recorded()) != 0 {
		t.Fatal("missing Paseo caused an API fallback")
	}
}

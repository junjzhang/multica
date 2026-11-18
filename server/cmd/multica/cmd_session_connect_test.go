package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode"
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

func TestBusinessCommandRenewsPaseoCredentialOnlyWhenNeeded(t *testing.T) {
	for _, test := range []struct {
		name   string
		ttl    time.Duration
		cached bool
		renew  bool
	}{
		{name: "fresh cache", ttl: time.Hour, cached: true},
		{name: "nearly expired", ttl: 2 * time.Minute, cached: true, renew: true},
		{name: "expired", ttl: -time.Minute, cached: true, renew: true},
		{name: "first use", renew: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.mailboxAgentID = "agent-1"
			home := t.TempDir()
			env := connectProcessEnv(t, home, api.server.URL)
			if test.cached {
				writeSessionBinding(t, defaultSessionPath(home, "paseo-agent-1"), sessionBinding("paseo-agent-1", api.server.URL, map[string]any{
					"expires_at": time.Now().Add(test.ttl).UTC().Format(time.RFC3339),
				}))
			}
			stdout, stderr, code := api.runCLI(t, home, env, "issue", "list", "--output", "json")
			if code != 0 {
				t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
			}
			_, err := os.Stat(env["CONNECT_ARGS"])
			if (err == nil) != test.renew {
				t.Fatalf("renew=%v, RPC marker error=%v", test.renew, err)
			}
			for _, request := range api.recorded() {
				if request.Authorization != "Bearer mea_session_token" {
					t.Fatalf("unexpected credential on %s", request.Path)
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
		{"save failure recovery", `printf '%s' '{"error":{"message":"[multica:session-connect] The binding could not be saved. The newly issued credential was revoked; Run multica session connect."}}' >&2; exit 1`, "credential was revoked; Run multica session connect"},
		{"save and revoke failure", `printf '%s' '{"error":{"message":"[multica:session-connect] The binding could not be saved and the credential could not be revoked. Revoke token 33333333-3333-4333-8333-333333333333 for agent 11111111-1111-4111-8111-111111111111 from the owner profile."}}' >&2; exit 1`, "Revoke token 33333333-3333-4333-8333-333333333333 for agent 11111111-1111-4111-8111-111111111111"},
		{"Node warning before envelope", `printf '%s\n' '(node:1) ExperimentalWarning: runtime warning' >&2; printf '%s' '{"error":{"message":"[multica:session-connect] Revoke token 33333333-3333-4333-8333-333333333333 for agent 11111111-1111-4111-8111-111111111111. mea_secret_value"}}' >&2; printf '%s\n' 'another warning' >&2; exit 1`, "Revoke token 33333333-3333-4333-8333-333333333333 for agent 11111111-1111-4111-8111-111111111111"},
		{"JSON and malformed braces in warnings", `printf '%s\n' 'Warning {bad} {"a":1} and {"error":{"message":"ordinary warning"}}' >&2; printf '%s' '{"error":{"code":"handler_error","message":"Request failed: transport description requestType=plugin.rpc.invoke.request code=handler_error","userMessage":"[multica:session-connect] Revoke token 33333333-3333-4333-8333-333333333333 for agent 11111111-1111-4111-8111-111111111111."}}' >&2; exit 1`, "Revoke token 33333333-3333-4333-8333-333333333333 for agent 11111111-1111-4111-8111-111111111111"},
		{"old daemon", `printf '%s' '{"error":{"code":"DAEMON_UPDATE_REQUIRED","message":"Update the host to invoke plugin RPCs. mea_secret_value"}}' >&2; exit 1`, "update the Paseo host"},
		{"controls split credential", `printf '%s' '{"error":{"message":"[multica:session-connect] credential mea_sec\u0000ret_value"}}' >&2; exit 1`, "credential [redacted]"},
		{"controls before prefix", `printf '%s' '{"error":{"userMessage":"[multica:session-connect] x\u0001mea_secret_value a\u009bmat_ABCDEFGHIJ\u0007\tz xmul_ABCDEFGHIJKL"}}' >&2; exit 1`, "x[redacted] a[redacted]\tz x[redacted]"},
		{"terminal controls", `printf '%s' '{"error":{"message":"transport description code=handler_error","userMessage":"[multica:session-connect] Revoke token 33333333-3333-4333-8333-333333333333 for agent 11111111-1111-4111-8111-111111111111.\u001b]52;c;cHduZWQ=\u0007\u001b[31mRED\u001b[0m\u009b31m\r\n\t下一步 mea_secret_value"}}' >&2; exit 1`, "\n\t下一步 [redacted]"},
		{"structured message ignores transport metadata", `printf '%s' '{"error":{"code":"invalid_request","message":"Do not print this requestType=other.request code=invalid_request","userMessage":"[multica:session-connect] Keep this recovery instruction."}}' >&2; exit 1`, "Keep this recovery instruction."},
		{"empty marker", `printf '%s' '{"error":{"userMessage":"[multica:session-connect] \u0000\t"}}' >&2; exit 1`, "session connect failed"},
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
			if strings.IndexFunc(stdout+stderr, func(r rune) bool { return r != '\n' && r != '\t' && unicode.IsControl(r) }) >= 0 {
				t.Fatal("RPC output contains terminal controls")
			}
			if strings.Contains(stdout+stderr, "requestType=") || strings.Contains(stdout+stderr, "code=handler_error") {
				t.Fatal("RPC metadata reached the user diagnostic")
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

func TestSessionReportsSanitizeRemoteDiagnostics(t *testing.T) {
	for _, operation := range []string{"connect", "status"} {
		for _, output := range []string{"json", "table"} {
			t.Run(operation+"/"+output, func(t *testing.T) {
				api := newFakeAPI(t)
				api.mailboxStatus = 500
				api.mailboxBody = "upstream failure \x1b]52;c;cHduZWQ=\a\x1b[31mRED\x1b[0m token mea_ses\x00sion_token\n\t恢复"
				home := t.TempDir()
				env := connectProcessEnv(t, home, api.server.URL)
				writeSessionBinding(t, defaultSessionPath(home, "paseo-agent-1"), sessionBinding("paseo-agent-1", api.server.URL, nil))
				stdout, stderr, code := api.runCLI(t, home, env, "session", operation, "--output", output)
				if code == 0 {
					t.Fatalf("remote verification failure exited 0: %s", stdout)
				}
				diagnostic := stdout + stderr
				if output == "json" {
					var report sessionStatusReport
					if err := json.Unmarshal([]byte(stdout), &report); err != nil {
						t.Fatal(err)
					}
					if report.Valid || report.Status != "invalid" || report.Remote == nil {
						t.Fatalf("unexpected report: %#v", report)
					}
					diagnostic = report.Error + "\n" + report.Remote.Error
				}
				if !strings.Contains(diagnostic, "upstream failure") || !strings.Contains(diagnostic, "[redacted]") {
					t.Fatalf("diagnostic lost failure detail: %q", diagnostic)
				}
				if strings.Contains(diagnostic, "mea_session_token") {
					t.Fatal("remote response leaked a credential")
				}
				if strings.IndexFunc(diagnostic, func(r rune) bool { return r != '\n' && r != '\t' && unicode.IsControl(r) }) >= 0 {
					t.Fatalf("remote response leaked controls: %q", diagnostic)
				}
			})
		}
	}
}

func TestSessionStatusSanitizesRemoteDisplayName(t *testing.T) {
	for _, output := range []string{"json", "table"} {
		t.Run(output, func(t *testing.T) {
			api := newFakeAPI(t)
			body, err := json.Marshal(map[string]string{"id": "agent-1", "execution_kind": "external", "name": "Visible \x1b[31m mea_ses\x00sion_token"})
			if err != nil {
				t.Fatal(err)
			}
			api.agentBody = string(body)
			api.sessionsBody = `{"sessions":[{"provider":"paseo","host_id":"host-1","session_id":"paseo-agent-1","daemon_id":"daemon-1"}]}`
			home := t.TempDir()
			writeSessionBinding(t, defaultSessionPath(home, "paseo-agent-1"), sessionBinding("paseo-agent-1", api.server.URL, nil))
			stdout, stderr, code := api.runCLI(t, home, map[string]string{"PASEO_AGENT_ID": "paseo-agent-1"}, "session", "status", "--output", output)
			if code != 0 {
				t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
			}
			display := stdout
			if output == "json" {
				var report sessionStatusReport
				if err := json.Unmarshal([]byte(stdout), &report); err != nil {
					t.Fatal(err)
				}
				if !report.Valid || report.Remote == nil {
					t.Fatalf("unexpected report: %#v", report)
				}
				display = report.Remote.AgentName
			}
			if !strings.Contains(display, "Visible") || !strings.Contains(display, "[redacted]") || strings.Contains(display, "mea_session_token") {
				t.Fatalf("unsafe display name: %q", display)
			}
			if strings.IndexFunc(display, func(r rune) bool { return r != '\n' && r != '\t' && unicode.IsControl(r) }) >= 0 {
				t.Fatal("display name retained controls")
			}
		})
	}
}

func TestSessionStatusRejectsUnsafeRemoteMetadataBeforeRendering(t *testing.T) {
	attack := "remote \x1b]52;c;cHduZWQ=\a mea_ses\x00sion_token"
	for _, tc := range []struct{ name, kind, daemon, want string }{
		{"execution kind", attack, "daemon-1", "[redacted]"},
		{"daemon", "external", attack, "[redacted]"},
		{"normalization must not authorize", "ext\x00ernal", "daemon-1", "not external"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI(t)
			agent, err := json.Marshal(map[string]string{"id": "agent-1", "name": "External", "execution_kind": tc.kind})
			if err != nil {
				t.Fatal(err)
			}
			registry, err := json.Marshal(map[string]any{"sessions": []map[string]string{{"provider": "paseo", "host_id": "host-1", "session_id": "paseo-agent-1", "daemon_id": tc.daemon}}})
			if err != nil {
				t.Fatal(err)
			}
			api.agentBody, api.sessionsBody = string(agent), string(registry)
			home := t.TempDir()
			writeSessionBinding(t, defaultSessionPath(home, "paseo-agent-1"), sessionBinding("paseo-agent-1", api.server.URL, nil))
			stdout, stderr, code := api.runCLI(t, home, map[string]string{"PASEO_AGENT_ID": "paseo-agent-1"}, "session", "status", "--output", "json")
			if code == 0 {
				t.Fatalf("invalid raw metadata authorized: %s", stdout)
			}
			var report sessionStatusReport
			if err := json.Unmarshal([]byte(stdout), &report); err != nil {
				t.Fatalf("%v stderr=%s", err, stderr)
			}
			if report.Valid || report.Remote == nil {
				t.Fatalf("unexpected report: %#v", report)
			}
			text := report.Error + report.Remote.Error + report.Remote.ExecutionKind + report.Remote.DaemonID
			if !strings.Contains(text, tc.want) || strings.Contains(text, "mea_session_token") {
				t.Fatalf("unsafe metadata: %q", text)
			}
			if strings.IndexFunc(text, func(r rune) bool { return r != '\n' && r != '\t' && unicode.IsControl(r) }) >= 0 {
				t.Fatal("unsafe controls in report")
			}
		})
	}
}

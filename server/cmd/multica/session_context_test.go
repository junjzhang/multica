package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
)

// The CLI's identity resolution reads process-wide environment, so its
// strongest test is a real CLI process with a fixed environment instead of a
// unit call that has to fake one. These tests re-execute this test binary as
// that process: TestMain hands child mode straight to m.Run() without the
// per-binary HOME isolation, then TestCLIProcessChild runs the real command
// tree with the exact arguments and environment the test chose.
const (
	cliProcessChildEnv     = "MULTICA_TEST_CLI_PROCESS_CHILD"
	cliProcessChildArgsEnv = "MULTICA_TEST_CLI_PROCESS_ARGS"
)

// TestCLIProcessChild is not a test of its own: it is the entry point of the
// child process started by fakeAPI.runCLI.
func TestCLIProcessChild(t *testing.T) {
	if os.Getenv(cliProcessChildEnv) != "1" {
		return
	}
	var args []string
	if raw := strings.TrimSpace(os.Getenv(cliProcessChildArgsEnv)); raw != "" {
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			fmt.Fprintf(os.Stderr, "decode child args: %v\n", err)
			os.Exit(97)
		}
	}
	rootCmd.SetArgs(args)
	if err := rootCmd.Execute(); err != nil {
		if err != errSilent {
			fmt.Fprintln(os.Stderr, cli.FormatError(err, debugFlag))
		}
		os.Exit(cli.ExitCodeFor(err))
	}
	os.Exit(0)
}

type apiRequest struct {
	Method        string
	Path          string
	Query         string
	Authorization string
	WorkspaceID   string
	AgentID       string
	TaskID        string
	ReturnSession string
	Body          string
}

// fakeAPI records every request it receives. Tests assert on what actually
// left the CLI process, which is the only proof that a fallback to the human
// profile did not happen.
type fakeAPI struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []apiRequest

	// agentStatus/agentBody answer GET /api/agents/<id>, used by session status.
	agentStatus int
	agentBody   string
	// mailboxAgentID is the agent the fake credential authenticates. A mailbox
	// request for any other agent returns 403, exactly like the real handler;
	// visibility of an agent is not a credential-identity check.
	mailboxAgentID string
	// mailboxStatus forces the identity check itself to fail.
	mailboxStatus int
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	api := &fakeAPI{mailboxAgentID: "agent-1"}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		api.mu.Lock()
		api.requests = append(api.requests, apiRequest{
			Method:        r.Method,
			Path:          r.URL.Path,
			Query:         r.URL.RawQuery,
			Authorization: r.Header.Get("Authorization"),
			WorkspaceID:   r.Header.Get("X-Workspace-ID"),
			AgentID:       r.Header.Get("X-Agent-ID"),
			TaskID:        r.Header.Get("X-Task-ID"),
			ReturnSession: r.Header.Get("X-Return-Session"),
			Body:          string(body),
		})
		status := http.StatusOK
		response := "{}"
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/mailbox") {
			requested := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/agents/"), "/mailbox")
			switch {
			case api.mailboxStatus != 0:
				status = api.mailboxStatus
				response = `{"error":"identity check failed"}`
			case api.agentStatus == http.StatusUnauthorized || api.agentStatus == http.StatusForbidden:
				status = api.agentStatus
				response = `{"error":"invalid token"}`
			case api.mailboxAgentID != "" && api.mailboxAgentID != requested:
				status = http.StatusForbidden
				response = `{"error":"mailbox credentials may only read their own mailbox"}`
			default:
				response = `{"messages":[],"next_cursor":null}`
			}
		} else if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/agents/") {
			if api.agentStatus != 0 {
				status = api.agentStatus
			}
			if api.agentBody != "" {
				response = api.agentBody
			}
		} else if r.Method == http.MethodGet && r.URL.Path == "/api/issues" {
			response = `{"issues":[],"total":0}`
		} else if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/runtime-profiles") {
			response = `{"runtime_profiles":[]}`
		} else if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/workspaces/") {
			response = `{"id":"ws-external","repos":[]}`
		}
		api.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(api.server.Close)
	return api
}

func (f *fakeAPI) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = nil
}

func (f *fakeAPI) recorded() []apiRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]apiRequest(nil), f.requests...)
}

// runCLI executes one real CLI process with exactly the environment given:
// nothing is inherited except PATH, and HOME points at the test's scratch home.
func (f *fakeAPI) runCLI(t *testing.T, home string, env map[string]string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	return runCLIProcess(t, home, env, args...)
}

func runCLIProcess(t *testing.T, home string, env map[string]string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	argsJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("encode args: %v", err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestCLIProcessChild$", "-test.count=1")
	child.Dir = t.TempDir()
	child.Env = []string{
		cliProcessChildEnv + "=1",
		cliProcessChildArgsEnv + "=" + string(argsJSON),
		"HOME=" + home,
		"USERPROFILE=" + home,
		"PATH=" + os.Getenv("PATH"),
	}
	for key, value := range env {
		child.Env = append(child.Env, key+"="+value)
	}
	var out, errOut bytes.Buffer
	child.Stdout = &out
	child.Stderr = &errOut
	err = child.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run CLI process: %v", err)
		}
		return out.String(), errOut.String(), exitErr.ExitCode()
	}
	return out.String(), errOut.String(), 0
}

// writeHumanProfile plants the owner profile the tests must never fall back to.
// It writes the file directly because the profile path follows the CHILD's
// HOME, which is not this process's HOME.
func writeHumanProfile(t *testing.T, home, serverURL string) {
	t.Helper()
	encoded, err := json.Marshal(cli.CLIConfig{
		ServerURL:   serverURL,
		WorkspaceID: "ws-owner",
		Token:       "mul_owner_token",
	})
	if err != nil {
		t.Fatalf("encode human profile: %v", err)
	}
	path := filepath.Join(home, ".multica", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create human profile dir: %v", err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write human profile: %v", err)
	}
}

func defaultSessionPath(home, sessionID string) string {
	return filepath.Join(home, ".multica", "external-sessions", "paseo", sessionID+".json")
}

func sessionBinding(sessionID, serverURL string, overrides map[string]any) map[string]any {
	binding := map[string]any{
		"version":      1,
		"provider":     "paseo",
		"host_id":      "host-1",
		"session_id":   sessionID,
		"agent_id":     "agent-1",
		"daemon_id":    "daemon-1",
		"server_url":   serverURL,
		"app_url":      "http://app.test",
		"workspace_id": "ws-external",
		"token":        "mea_session_token",
		"token_id":     "token-1",
		"expires_at":   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}
	for key, value := range overrides {
		binding[key] = value
	}
	return binding
}

func writeSessionBinding(t *testing.T, path string, binding map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(binding)
	if err != nil {
		t.Fatalf("encode session binding: %v", err)
	}
	writeSessionBindingRaw(t, path, string(encoded), 0o600)
	return path
}

func writeSessionBindingRaw(t *testing.T, path, content string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create session dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write session binding: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod session binding: %v", err)
	}
	return path
}

const sessionIssueID = "5b1f7d2e-0c1a-4b7e-9f3a-2d6c8e4b1a90"

// TestCLIProcessUsesSessionIdentityForOrdinaryCommands is the contract's first
// gate: with a human profile present in the same HOME, issue, comment and
// wakeup commands must authenticate as the session's external agent.
func TestCLIProcessUsesSessionIdentityForOrdinaryCommands(t *testing.T) {
	humanAPI := newFakeAPI(t)
	externalAPI := newFakeAPI(t)
	home := t.TempDir()
	writeHumanProfile(t, home, humanAPI.server.URL)
	writeSessionBinding(t, defaultSessionPath(home, "paseo-agent-1"), sessionBinding("paseo-agent-1", externalAPI.server.URL, nil))

	cases := []struct {
		name     string
		args     []string
		wantPath string
	}{
		{"issue list", []string{"issue", "list", "--output", "json"}, "/api/issues"},
		{"issue comment add", []string{"issue", "comment", "add", sessionIssueID, "--content", "hello from the session", "--output", "json"}, "/api/issues/" + sessionIssueID + "/comments"},
		{"issue wakeup create", []string{"issue", "wakeup", "create", sessionIssueID, "--event", "comment.created", "--instruction", "watch this issue", "--output", "json"}, ""},
		// These used to re-read the human profile's workspace after the session
		// client was built, splitting identity from scope.
		{"repo list", []string{"repo", "list", "--output", "json"}, "/api/workspaces/ws-external"},
		{"runtime profile list", []string{"runtime", "profile", "list", "--output", "json"}, "/api/workspaces/ws-external/runtime-profiles"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			humanAPI.reset()
			externalAPI.reset()
			_, stderr, exitCode := externalAPI.runCLI(t, home, map[string]string{"PASEO_AGENT_ID": "paseo-agent-1"}, tc.args...)
			if exitCode != 0 {
				t.Fatalf("exit code = %d, stderr:\n%s", exitCode, stderr)
			}
			if got := humanAPI.recorded(); len(got) != 0 {
				t.Fatalf("session command reached the human profile's server: %#v", got)
			}
			got := externalAPI.recorded()
			if len(got) != 2 {
				t.Fatalf("external requests = %d, want identity check + command (%#v)", len(got), got)
			}
			if got[0].Path != "/api/agents/agent-1/mailbox" || got[0].Query != "limit=1" {
				t.Fatalf("first request = %s?%s, want the own-mailbox identity check", got[0].Path, got[0].Query)
			}
			if tc.wantPath != "" && got[1].Path != tc.wantPath {
				t.Fatalf("command request = %s, want %s (session workspace, not the human profile's)", got[1].Path, tc.wantPath)
			}
			for _, req := range got {
				if req.ReturnSession != "paseo-agent-1" {
					t.Errorf("return session = %q, want the credential file's exact session", req.ReturnSession)
				}
				if req.Authorization != "Bearer mea_session_token" {
					t.Errorf("%s Authorization = %q, want the session's mea credential", req.Path, req.Authorization)
				}
				if req.WorkspaceID != "ws-external" {
					t.Errorf("%s X-Workspace-ID = %q, want ws-external", req.Path, req.WorkspaceID)
				}
				// An external session is authorship, never a managed task: it must
				// not borrow the daemon task headers.
				if req.AgentID != "" || req.TaskID != "" {
					t.Errorf("managed task headers leaked into a session request: agent=%q task=%q", req.AgentID, req.TaskID)
				}
			}
		})
	}

	t.Run("matching explicit endpoint and workspace", func(t *testing.T) {
		humanAPI.reset()
		externalAPI.reset()
		env := map[string]string{
			"PASEO_AGENT_ID":       "paseo-agent-1",
			"MULTICA_SERVER_URL":   externalAPI.server.URL + "/",
			"MULTICA_WORKSPACE_ID": "ws-external",
		}
		_, stderr, exitCode := externalAPI.runCLI(t, home, env, "issue", "list", "--output", "json")
		if exitCode != 0 {
			t.Fatalf("exit code = %d, stderr:\n%s", exitCode, stderr)
		}
		if got := humanAPI.recorded(); len(got) != 0 {
			t.Fatalf("matching explicit input fell back to the human server: %#v", got)
		}
		got := externalAPI.recorded()
		if len(got) != 2 {
			t.Fatalf("requests = %d, want identity check + list (%#v)", len(got), got)
		}
		for _, req := range got {
			if req.Authorization != "Bearer mea_session_token" {
				t.Fatalf("session identity not used: %#v", got)
			}
		}
	})

	t.Run("explicit context file", func(t *testing.T) {
		humanAPI.reset()
		externalAPI.reset()
		explicitPath := writeSessionBinding(t, filepath.Join(t.TempDir(), "explicit.json"), sessionBinding("paseo-agent-9", externalAPI.server.URL, map[string]any{
			"token":        "mea_explicit_token",
			"workspace_id": "ws-explicit",
		}))
		_, stderr, exitCode := externalAPI.runCLI(t, home, map[string]string{"MULTICA_EXTERNAL_SESSION_FILE": explicitPath}, "issue", "list", "--output", "json")
		if exitCode != 0 {
			t.Fatalf("exit code = %d, stderr:\n%s", exitCode, stderr)
		}
		if got := humanAPI.recorded(); len(got) != 0 {
			t.Fatalf("explicit context still reached the human server: %#v", got)
		}
		got := externalAPI.recorded()
		if len(got) != 2 {
			t.Fatalf("requests = %d, want identity check + list (%#v)", len(got), got)
		}
		for _, req := range got {
			if req.Authorization != "Bearer mea_explicit_token" || req.WorkspaceID != "ws-explicit" || req.ReturnSession != "paseo-agent-9" {
				t.Fatalf("explicit binding was not used: %#v", got)
			}
		}
	})
}

// TestCLIProcessFailsClosedWithoutBinding proves a Paseo session with no file
// never falls through to the human's default config.
func TestCLIProcessFailsClosedWithoutBinding(t *testing.T) {
	humanAPI := newFakeAPI(t)
	home := t.TempDir()
	writeHumanProfile(t, home, humanAPI.server.URL)

	_, stderr, exitCode := humanAPI.runCLI(t, home, map[string]string{"PASEO_AGENT_ID": "paseo-agent-1"}, "issue", "list")
	if exitCode == 0 {
		t.Fatalf("exit code = 0, want a failure; stderr:\n%s", stderr)
	}
	for _, want := range []string{"no Multica binding", "/multica connect"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
	if got := humanAPI.recorded(); len(got) != 0 {
		t.Fatalf("missing binding fell through to the human profile: %#v", got)
	}
}

func TestCLIProcessRejectsConflictingOrInvalidBindings(t *testing.T) {
	humanAPI := newFakeAPI(t)
	otherAPI := newFakeAPI(t)
	home := t.TempDir()
	writeHumanProfile(t, home, humanAPI.server.URL)

	validPath := func(t *testing.T) string {
		return writeSessionBinding(t, defaultSessionPath(home, "paseo-agent-1"), sessionBinding("paseo-agent-1", otherAPI.server.URL, nil))
	}

	cases := []struct {
		name    string
		prepare func(t *testing.T)
		env     map[string]string
		args    []string
		want    string
	}{
		{
			name:    "profile flag",
			prepare: func(t *testing.T) { validPath(t) },
			args:    []string{"--profile", "dev", "issue", "list"},
			want:    "--profile",
		},
		{
			name:    "owner token in the environment",
			prepare: func(t *testing.T) { validPath(t) },
			env:     map[string]string{"MULTICA_TOKEN": "mul_owner_token"},
			args:    []string{"issue", "list"},
			want:    "MULTICA_TOKEN",
		},
		{
			name:    "server env disagrees",
			prepare: func(t *testing.T) { validPath(t) },
			env:     map[string]string{"MULTICA_SERVER_URL": humanAPI.server.URL},
			args:    []string{"issue", "list"},
			want:    "conflicts with the session's server",
		},
		{
			name:    "server flag disagrees",
			prepare: func(t *testing.T) { validPath(t) },
			args:    []string{"issue", "list", "--server-url", humanAPI.server.URL},
			want:    "conflicts with the session's server",
		},
		{
			name:    "workspace env disagrees",
			prepare: func(t *testing.T) { validPath(t) },
			env:     map[string]string{"MULTICA_WORKSPACE_ID": "ws-owner"},
			args:    []string{"issue", "list"},
			want:    "conflicts with the session's workspace",
		},
		{
			name:    "workspace flag disagrees",
			prepare: func(t *testing.T) { validPath(t) },
			args:    []string{"issue", "list", "--workspace-id", "ws-owner"},
			want:    "conflicts with the session's workspace",
		},
		{
			name: "expired credential",
			prepare: func(t *testing.T) {
				writeSessionBinding(t, defaultSessionPath(home, "paseo-agent-1"), sessionBinding("paseo-agent-1", otherAPI.server.URL, map[string]any{
					"expires_at": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
				}))
			},
			args: []string{"issue", "list"},
			want: "expired",
		},
		{
			name: "wrong provider",
			prepare: func(t *testing.T) {
				writeSessionBinding(t, defaultSessionPath(home, "paseo-agent-1"), sessionBinding("paseo-agent-1", otherAPI.server.URL, map[string]any{
					"provider": "slack",
				}))
			},
			args: []string{"issue", "list"},
			want: "provider",
		},
		{
			name: "owner credential written into the file",
			prepare: func(t *testing.T) {
				writeSessionBinding(t, defaultSessionPath(home, "paseo-agent-1"), sessionBinding("paseo-agent-1", otherAPI.server.URL, map[string]any{
					"token": "mul_owner_token",
				}))
			},
			args: []string{"issue", "list"},
			want: "mea_",
		},
		{
			name: "malformed file",
			prepare: func(t *testing.T) {
				writeSessionBindingRaw(t, defaultSessionPath(home, "paseo-agent-1"), "{not json", 0o600)
			},
			args: []string{"issue", "list"},
			want: "not valid JSON",
		},
		{
			name:    "file readable by other users",
			prepare: func(t *testing.T) { writeSessionBindingRaw(t, defaultSessionPath(home, "paseo-agent-1"), "{}", 0o644) },
			args:    []string{"issue", "list"},
			want:    "readable by other users",
		},
		{
			name:    "session id does not match the environment",
			prepare: func(t *testing.T) { validPath(t) },
			env: map[string]string{
				"PASEO_AGENT_ID":                "another-paseo-agent",
				"MULTICA_EXTERNAL_SESSION_FILE": defaultSessionPath(home, "paseo-agent-1"),
			},
			args: []string{"issue", "list"},
			want: "does not match",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			humanAPI.reset()
			otherAPI.reset()
			if err := os.RemoveAll(filepath.Join(home, ".multica", "external-sessions")); err != nil {
				t.Fatalf("reset session dir: %v", err)
			}
			tc.prepare(t)
			env := map[string]string{"PASEO_AGENT_ID": "paseo-agent-1"}
			for key, value := range tc.env {
				env[key] = value
			}
			_, stderr, exitCode := otherAPI.runCLI(t, home, env, tc.args...)
			if exitCode == 0 {
				t.Fatalf("exit code = 0, want rejection; stderr:\n%s", stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr missing %q:\n%s", tc.want, stderr)
			}
			if got := humanAPI.recorded(); len(got) != 0 {
				t.Fatalf("rejected binding still reached the human server: %#v", got)
			}
			if got := otherAPI.recorded(); len(got) != 0 {
				t.Fatalf("rejected binding still sent a request: %#v", got)
			}
		})
	}
}

// TestCLIProcessKeepsManagedTaskIdentity covers both directions of the
// precedence rule: an inherited PASEO_AGENT_ID never displaces a managed task,
// and an explicit external context file is refused inside one.
func TestCLIProcessKeepsManagedTaskIdentity(t *testing.T) {
	managedAPI := newFakeAPI(t)
	externalAPI := newFakeAPI(t)
	home := t.TempDir()
	writeHumanProfile(t, home, externalAPI.server.URL)
	writeSessionBinding(t, defaultSessionPath(home, "paseo-agent-1"), sessionBinding("paseo-agent-1", externalAPI.server.URL, nil))

	managedEnv := map[string]string{
		"MULTICA_AGENT_ID":              "agent-managed",
		"MULTICA_TASK_ID":               "task-managed",
		"MULTICA_TOKEN":                 "mat_task_token",
		"MULTICA_SERVER_URL":            managedAPI.server.URL,
		"MULTICA_WORKSPACE_ID":          "ws-managed",
		"PASEO_AGENT_ID":                "paseo-agent-1",
		"MULTICA_EXTERNAL_SESSION_FILE": "",
	}
	_, stderr, exitCode := managedAPI.runCLI(t, home, managedEnv, "issue", "list", "--output", "json")
	if exitCode != 0 {
		t.Fatalf("managed task exit code = %d, stderr:\n%s", exitCode, stderr)
	}
	got := managedAPI.recorded()
	if len(got) != 1 {
		t.Fatalf("managed requests = %d, want 1 (%#v)", len(got), got)
	}
	if got[0].Authorization != "Bearer mat_task_token" || got[0].AgentID != "agent-managed" || got[0].TaskID != "task-managed" {
		t.Errorf("managed identity was not preserved: %#v", got[0])
	}
	if got[0].ReturnSession != "" {
		t.Fatalf("managed request inherited external return session %q", got[0].ReturnSession)
	}
	if got := externalAPI.recorded(); len(got) != 0 {
		t.Fatalf("incidental PASEO_AGENT_ID replaced the managed identity: %#v", got)
	}

	// An explicit external context inside a managed task is a contradiction.
	managedAPI.reset()
	managedEnv["MULTICA_EXTERNAL_SESSION_FILE"] = defaultSessionPath(home, "paseo-agent-1")
	_, stderr, exitCode = managedAPI.runCLI(t, home, managedEnv, "issue", "list")
	if exitCode == 0 {
		t.Fatalf("explicit external context inside a managed task was accepted; stderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "daemon-managed task") {
		t.Errorf("stderr missing the managed-task explanation:\n%s", stderr)
	}
	if got := managedAPI.recorded(); len(got) != 0 {
		t.Fatalf("refused context still sent a request: %#v", got)
	}
}

// TestSessionStatusReportsNonSecretState covers the inspection command: it
// reports identity and the original-session return address, never the token,
// and only claims validity after the server confirms the binding.
func TestSessionStatusReportsNonSecretState(t *testing.T) {
	externalAPI := newFakeAPI(t)
	externalAPI.agentBody = `{"id":"agent-1","name":"External Implementer","execution_kind":"external","external_session":{"provider":"paseo","host_id":"host-1","session_id":"paseo-agent-1","display_name":"Impl","url":null,"daemon_id":"daemon-1"}}`
	home := t.TempDir()
	writeSessionBinding(t, defaultSessionPath(home, "paseo-agent-1"), sessionBinding("paseo-agent-1", externalAPI.server.URL, nil))

	stdout, stderr, exitCode := externalAPI.runCLI(t, home, map[string]string{"PASEO_AGENT_ID": "paseo-agent-1"}, "session", "status", "--output", "json")
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr:\n%s", exitCode, stderr)
	}
	var report sessionStatusReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("decode status JSON: %v\n%s", err, stdout)
	}
	if report.Status != "connected" || !report.Valid {
		t.Fatalf("status = %q valid = %t, want connected/valid; report: %#v", report.Status, report.Valid, report)
	}
	if report.AgentID != "agent-1" || report.Remote == nil || report.Remote.AgentName != "External Implementer" {
		t.Errorf("identity not reported from the server: %#v", report)
	}
	if report.Remote != nil && (report.Remote.Provider != "paseo" || report.Remote.HostID != "host-1" || report.Remote.SessionID != "paseo-agent-1" || report.Remote.DaemonID != "daemon-1") {
		t.Errorf("remote binding not reported: %#v", report.Remote)
	}
	if report.ReturnAddress == nil || report.ReturnAddress.URL != "paseo://h/host-1/agent/paseo-agent-1" || report.ReturnAddress.DaemonID != "daemon-1" {
		t.Errorf("return address = %#v", report.ReturnAddress)
	}
	if report.TokenID != "token-1" || report.ExpiresAt == "" {
		t.Errorf("non-secret credential metadata missing: %#v", report)
	}
	if strings.Contains(stdout, "mea_session_token") {
		t.Fatalf("status output leaked the credential:\n%s", stdout)
	}
}

// TestSessionStatusDistinguishesInvalidFromConnected proves a file on disk is
// not treated as authorization: a rejected credential reports invalid, while
// the binding's identity and return address stay readable.
func TestSessionStatusDistinguishesInvalidFromConnected(t *testing.T) {
	externalAPI := newFakeAPI(t)
	externalAPI.agentStatus = http.StatusUnauthorized
	externalAPI.agentBody = `{"error":"invalid token"}`
	home := t.TempDir()
	writeSessionBinding(t, defaultSessionPath(home, "paseo-agent-1"), sessionBinding("paseo-agent-1", externalAPI.server.URL, nil))

	stdout, stderr, exitCode := externalAPI.runCLI(t, home, map[string]string{"PASEO_AGENT_ID": "paseo-agent-1"}, "session", "status", "--output", "json")
	if exitCode == 0 {
		t.Fatalf("invalid binding exited 0; report:\n%s\nstderr:\n%s", stdout, stderr)
	}
	var report sessionStatusReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("decode status JSON: %v\n%s", err, stdout)
	}
	if report.Valid || report.Status != "invalid" {
		t.Fatalf("status = %q valid = %t, want invalid/false", report.Status, report.Valid)
	}
	if report.Remote == nil || !report.Remote.Checked || !strings.Contains(report.Remote.Error, "rejected") {
		t.Errorf("remote check did not report the rejection: %#v", report.Remote)
	}
	if report.ReturnAddress == nil {
		t.Errorf("return address should still be readable: %#v", report)
	}
	if strings.Contains(stdout, "mea_session_token") {
		t.Fatalf("status output leaked the credential:\n%s", stdout)
	}
}

func TestSessionStatusReportsDisconnectedAndMalformed(t *testing.T) {
	api := newFakeAPI(t)
	home := t.TempDir()

	t.Run("no context", func(t *testing.T) {
		stdout, stderr, exitCode := api.runCLI(t, home, nil, "session", "status", "--output", "json")
		if exitCode == 0 {
			t.Fatalf("disconnected binding exited 0; stdout:\n%s\nstderr:\n%s", stdout, stderr)
		}
		var report sessionStatusReport
		if err := json.Unmarshal([]byte(stdout), &report); err != nil {
			t.Fatalf("decode status JSON: %v\n%s", err, stdout)
		}
		if report.Status != "disconnected" || report.Valid || report.Error == "" {
			t.Fatalf("report = %#v, want disconnected with an actionable error", report)
		}
	})

	t.Run("default file removed after connect", func(t *testing.T) {
		stdout, stderr, exitCode := api.runCLI(t, home, map[string]string{"PASEO_AGENT_ID": "paseo-agent-1"}, "session", "status", "--output", "json")
		if exitCode == 0 {
			t.Fatalf("missing default file exited 0; stdout:\n%s\nstderr:\n%s", stdout, stderr)
		}
		var report sessionStatusReport
		if err := json.Unmarshal([]byte(stdout), &report); err != nil {
			t.Fatalf("decode status JSON: %v\n%s", err, stdout)
		}
		if report.Status != "disconnected" || report.Valid || !strings.Contains(report.Error, "no Multica binding") {
			t.Fatalf("report = %#v, want disconnected for a missing default file", report)
		}
		if got := api.recorded(); len(got) != 0 {
			t.Fatalf("missing binding still contacted the server: %#v", got)
		}
	})

	t.Run("malformed file", func(t *testing.T) {
		writeSessionBindingRaw(t, defaultSessionPath(home, "paseo-agent-1"), "{not json", 0o600)
		stdout, stderr, exitCode := api.runCLI(t, home, map[string]string{"PASEO_AGENT_ID": "paseo-agent-1"}, "session", "status", "--output", "json")
		if exitCode == 0 {
			t.Fatalf("invalid binding exited 0; stdout:\n%s\nstderr:\n%s", stdout, stderr)
		}
		var report sessionStatusReport
		if err := json.Unmarshal([]byte(stdout), &report); err != nil {
			t.Fatalf("decode status JSON: %v\n%s", err, stdout)
		}
		if report.Status != "invalid" || report.Valid || !strings.Contains(report.Error, "not valid JSON") {
			t.Fatalf("report = %#v, want invalid with the parse problem", report)
		}
	})
}

// TestCLIProcessRejectsCredentialForAnotherAgent is the regression for a
// binding that names agent B while the token belongs to another external agent
// the same owner can see. Visible-agent reads must not be mistaken for proof:
// the command has to stop at the own-mailbox identity check and never issue the
// business write.
func TestCLIProcessRejectsCredentialForAnotherAgent(t *testing.T) {
	api := newFakeAPI(t)
	api.mailboxAgentID = "agent-other"
	api.agentBody = `{"id":"agent-1","name":"Declared Agent","execution_kind":"external","external_session":{"provider":"paseo","host_id":"host-1","session_id":"paseo-agent-1","daemon_id":"daemon-1"}}`
	home := t.TempDir()
	writeSessionBinding(t, defaultSessionPath(home, "paseo-agent-1"), sessionBinding("paseo-agent-1", api.server.URL, nil))

	_, stderr, exitCode := api.runCLI(t, home, map[string]string{"PASEO_AGENT_ID": "paseo-agent-1"},
		"issue", "comment", "add", sessionIssueID, "--content", "must not be attributed to another agent")
	if exitCode == 0 {
		t.Fatalf("mismatched credential accepted; stderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "does not belong") {
		t.Errorf("stderr does not name the identity mismatch:\n%s", stderr)
	}
	got := api.recorded()
	if len(got) != 1 || got[0].Path != "/api/agents/agent-1/mailbox" {
		t.Fatalf("mismatched credential sent more than the identity check: %#v", got)
	}
	if got[0].Method != http.MethodGet {
		t.Fatalf("identity check must be read-only: %#v", got[0])
	}
}

// TestSessionStatusRejectsAnotherAgentsCredential covers the same boundary for
// the inspection command: the visible agent record matches the binding, but the
// server says the credential is not that agent, so valid stays false.
func TestSessionStatusRejectsAnotherAgentsCredential(t *testing.T) {
	api := newFakeAPI(t)
	api.mailboxAgentID = "agent-other"
	api.agentBody = `{"id":"agent-1","name":"Declared Agent","execution_kind":"external","external_session":{"provider":"paseo","host_id":"host-1","session_id":"paseo-agent-1","daemon_id":"daemon-1"}}`
	home := t.TempDir()
	writeSessionBinding(t, defaultSessionPath(home, "paseo-agent-1"), sessionBinding("paseo-agent-1", api.server.URL, nil))

	stdout, stderr, exitCode := api.runCLI(t, home, map[string]string{"PASEO_AGENT_ID": "paseo-agent-1"}, "session", "status", "--output", "json")
	if exitCode == 0 {
		t.Fatalf("mismatched credential reported success; stdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	var report sessionStatusReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("decode status JSON: %v\n%s", err, stdout)
	}
	if report.Valid || report.Status != "invalid" {
		t.Fatalf("status = %q valid = %t, want invalid/false; report: %#v", report.Status, report.Valid, report)
	}
	if report.Remote == nil || !strings.Contains(report.Remote.Error, "different agent") {
		t.Errorf("remote error does not report the identity mismatch: %#v", report.Remote)
	}
	for _, req := range api.recorded() {
		if req.Method != http.MethodGet {
			t.Fatalf("session status issued a non-read request: %#v", req)
		}
	}
}

// TestSessionStatusRejectsRemoteBindingDrift proves status checks every field
// the server records for the session, not only the agent's existence.
func TestSessionStatusRejectsRemoteBindingDrift(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"provider", `{"id":"agent-1","name":"A","execution_kind":"external","external_session":{"provider":"slack","host_id":"host-1","session_id":"paseo-agent-1","daemon_id":"daemon-1"}}`, "provider"},
		{"host", `{"id":"agent-1","name":"A","execution_kind":"external","external_session":{"provider":"paseo","host_id":"host-other","session_id":"paseo-agent-1","daemon_id":"daemon-1"}}`, "host"},
		{"session", `{"id":"agent-1","name":"A","execution_kind":"external","external_session":{"provider":"paseo","host_id":"host-1","session_id":"paseo-agent-other","daemon_id":"daemon-1"}}`, "session"},
		{"daemon", `{"id":"agent-1","name":"A","execution_kind":"external","external_session":{"provider":"paseo","host_id":"host-1","session_id":"paseo-agent-1","daemon_id":"daemon-other"}}`, "daemon"},
		{"execution kind", `{"id":"agent-1","name":"A","execution_kind":"managed","external_session":{"provider":"paseo","host_id":"host-1","session_id":"paseo-agent-1","daemon_id":"daemon-1"}}`, "execution_kind"},
		{"session removed", `{"id":"agent-1","name":"A","execution_kind":"external","external_session":null}`, "no longer returns an external session"},
		{"agent id", `{"id":"agent-other","name":"A","execution_kind":"external","external_session":{"provider":"paseo","host_id":"host-1","session_id":"paseo-agent-1","daemon_id":"daemon-1"}}`, "not the recorded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.agentBody = tc.body
			home := t.TempDir()
			writeSessionBinding(t, defaultSessionPath(home, "paseo-agent-1"), sessionBinding("paseo-agent-1", api.server.URL, nil))

			stdout, stderr, exitCode := api.runCLI(t, home, map[string]string{"PASEO_AGENT_ID": "paseo-agent-1"}, "session", "status", "--output", "json")
			if exitCode == 0 {
				t.Fatalf("binding drift exited 0; stdout:\n%s\nstderr:\n%s", stdout, stderr)
			}
			var report sessionStatusReport
			if err := json.Unmarshal([]byte(stdout), &report); err != nil {
				t.Fatalf("decode status JSON: %v\n%s", err, stdout)
			}
			if report.Valid || report.Status != "invalid" || !strings.Contains(report.Error, tc.want) {
				t.Fatalf("report = %#v, want invalid with %q", report, tc.want)
			}
		})
	}
}

// TestSessionStatusUsesCommandConflictRules keeps status from advertising a
// binding that an ordinary command in the same process would refuse.
func TestSessionStatusUsesCommandConflictRules(t *testing.T) {
	api := newFakeAPI(t)
	home := t.TempDir()
	writeSessionBinding(t, defaultSessionPath(home, "paseo-agent-1"), sessionBinding("paseo-agent-1", api.server.URL, nil))

	cases := []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{"profile flag", nil, []string{"--profile", "dev"}, "--profile"},
		{"owner token", map[string]string{"MULTICA_TOKEN": "mul_owner_token"}, nil, "MULTICA_TOKEN"},
		{"server env", map[string]string{"MULTICA_SERVER_URL": "http://127.0.0.1:1"}, nil, "conflicts with the session's server"},
		{"workspace flag", nil, []string{"--workspace-id", "ws-owner"}, "conflicts with the session's workspace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api.reset()
			env := map[string]string{"PASEO_AGENT_ID": "paseo-agent-1"}
			for key, value := range tc.env {
				env[key] = value
			}
			args := append([]string{"session", "status", "--output", "json"}, tc.args...)
			stdout, stderr, exitCode := api.runCLI(t, home, env, args...)
			if exitCode == 0 {
				t.Fatalf("conflicting status exited 0; stdout:\n%s\nstderr:\n%s", stdout, stderr)
			}
			var report sessionStatusReport
			if err := json.Unmarshal([]byte(stdout), &report); err != nil {
				t.Fatalf("decode status JSON: %v\n%s", err, stdout)
			}
			if report.Valid || report.Status != "invalid" || !strings.Contains(report.Error, tc.want) {
				t.Fatalf("report = %#v, want invalid with %q", report, tc.want)
			}
			if got := api.recorded(); len(got) != 0 {
				t.Fatalf("conflicting status still contacted the server: %#v", got)
			}
		})
	}
}

// TestCLIProcessFailsClosedWhenActorCheckCannotVerify covers the other half of
// the identity gate: a rejected credential and an unverifiable server must also
// stop before the business request, not fall through to "try anyway".
func TestCLIProcessFailsClosedWhenActorCheckCannotVerify(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		{"rejected credential", http.StatusUnauthorized},
		{"unverifiable server", http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.mailboxStatus = tc.status
			home := t.TempDir()
			writeSessionBinding(t, defaultSessionPath(home, "paseo-agent-1"), sessionBinding("paseo-agent-1", api.server.URL, nil))

			_, stderr, exitCode := api.runCLI(t, home, map[string]string{"PASEO_AGENT_ID": "paseo-agent-1"},
				"issue", "comment", "add", sessionIssueID, "--content", "must not be written")
			if exitCode == 0 {
				t.Fatalf("identity check failure was ignored; stderr:\n%s", stderr)
			}
			got := api.recorded()
			if len(got) != 1 || got[0].Method != http.MethodGet || got[0].Path != "/api/agents/agent-1/mailbox" {
				t.Fatalf("requests = %#v, want only the own-mailbox identity check", got)
			}
		})
	}
}

func TestCLIProcessBoundReturnSessionCannotDrift(t *testing.T) {
	cases := []struct {
		name   string
		env    map[string]string
		args   []string
		wantOK bool
	}{
		{"same flag", nil, []string{"--return-session", "paseo-agent-1"}, true},
		{"same env", map[string]string{"MULTICA_RETURN_SESSION": "paseo-agent-1"}, nil, true},
		{"different flag", nil, []string{"--return-session", "other-session"}, false},
		{"different env", map[string]string{"MULTICA_RETURN_SESSION": "other-session"}, nil, false},
		{"conflicting env cannot be masked", map[string]string{"MULTICA_RETURN_SESSION": "other-session"}, []string{"--return-session", "paseo-agent-1"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI(t)
			home := t.TempDir()
			writeHumanProfile(t, home, api.server.URL)
			writeSessionBinding(t, defaultSessionPath(home, "paseo-agent-1"), sessionBinding("paseo-agent-1", api.server.URL, nil))
			env := map[string]string{"PASEO_AGENT_ID": "paseo-agent-1"}
			for key, value := range tc.env {
				env[key] = value
			}
			args := append([]string{"issue", "comment", "add", sessionIssueID, "--content", "exact return", "--output", "json"}, tc.args...)
			_, stderr, code := api.runCLI(t, home, env, args...)
			if !tc.wantOK {
				if code == 0 || !strings.Contains(stderr, "return session") || len(api.recorded()) != 0 {
					t.Fatalf("conflicting return reached HTTP: exit=%d requests=%d stderr=%s", code, len(api.recorded()), stderr)
				}
				return
			}
			if code != 0 {
				t.Fatalf("same target refused: %s", stderr)
			}
			requests := api.recorded()
			if len(requests) != 2 || requests[1].Method != http.MethodPost {
				t.Fatalf("missing verified business write: %#v", requests)
			}
			for _, req := range requests {
				if req.Authorization != "Bearer mea_session_token" || req.WorkspaceID != "ws-external" || req.ReturnSession != "paseo-agent-1" {
					t.Fatal("credential, workspace and return session did not come from the same binding")
				}
			}
		})
	}
}

func TestCLIProcessOwnerBootstrapIgnoresBusinessSessionBinding(t *testing.T) {
	for _, connected := range []bool{false, true} {
		t.Run(fmt.Sprint(connected), func(t *testing.T) {
			owner := newFakeAPI(t)
			external := newFakeAPI(t)
			home := t.TempDir()
			writeHumanProfile(t, home, owner.server.URL)
			if connected {
				writeSessionBinding(t, defaultSessionPath(home, "paseo-agent-1"), sessionBinding("paseo-agent-1", external.server.URL, nil))
			}
			_, stderr, code := owner.runCLI(t, home, map[string]string{"PASEO_AGENT_ID": "paseo-agent-1"}, "agent", "external", "register", "--name", "Fixture", "--host-id", "host-1", "--session-id", "paseo-agent-1", "--daemon-id", "daemon-1", "--output", "json")
			if code != 0 {
				t.Fatalf("owner bootstrap refused: %s", stderr)
			}
			requests := owner.recorded()
			if len(requests) != 1 || requests[0].Path != "/api/agents/external" || requests[0].Authorization != "Bearer mul_owner_token" || requests[0].WorkspaceID != "ws-owner" || len(external.recorded()) != 0 {
				t.Fatal("owner bootstrap used the session credential or scope")
			}
		})
	}
}

func TestCLIProcessNonPaseoSessionUsesExplicitCredentials(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(fmt.Sprint(manual), func(t *testing.T) {
			api := newFakeAPI(t)
			home := t.TempDir()
			writeHumanProfile(t, home, api.server.URL)
			env := map[string]string{}
			args := []string{"issue", "comment", "add", sessionIssueID, "--content", "non-Paseo caller", "--output", "json"}
			wantToken, wantWorkspace, wantReturn := "Bearer mul_owner_token", "ws-owner", ""
			if manual {
				env = map[string]string{"MULTICA_TOKEN": "mea_manual", "MULTICA_SERVER_URL": api.server.URL, "MULTICA_WORKSPACE_ID": "ws-manual", "MULTICA_RETURN_SESSION": "env-session"}
				args = append(args, "--return-session", "flag-session")
				wantToken, wantWorkspace, wantReturn = "Bearer mea_manual", "ws-manual", "flag-session"
			}
			_, stderr, code := api.runCLI(t, home, env, args...)
			if code != 0 {
				t.Fatalf("non-Paseo command refused: %s", stderr)
			}
			requests := api.recorded()
			if len(requests) != 1 || requests[0].Authorization != wantToken || requests[0].WorkspaceID != wantWorkspace || requests[0].ReturnSession != wantReturn {
				t.Fatalf("non-Paseo identity/return contract changed: %#v", requests)
			}
		})
	}
}

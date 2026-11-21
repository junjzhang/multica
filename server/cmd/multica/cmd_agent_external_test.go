package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

func newAgentExternalTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "test"}
	registerAgentExternalRegisterFlags(cmd)
	return cmd
}

func newAgentTokenCreateTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "test"}
	registerAgentTokenCreateFlags(cmd)
	return cmd
}

func newAgentTokenRevokeTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "test"}
	registerAgentTokenRevokeFlags(cmd)
	return cmd
}

func newAgentMailboxListTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "test"}
	registerAgentMailboxListFlags(cmd)
	return cmd
}

func newAgentMailboxAckTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "test"}
	registerAgentMailboxAckFlags(cmd)
	return cmd
}

func setAgentExternalTestEnv(t *testing.T, serverURL string) {
	t.Helper()
	t.Chdir(t.TempDir())
	setCLITestServerEnv(t, serverURL)
	t.Setenv("MULTICA_AGENT_ID", "")
	t.Setenv("MULTICA_TASK_ID", "")
}

func TestRunAgentExternalRegisterSendsExplicitSession(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/agents/external" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":             "agent-1",
			"name":           "External Reviewer",
			"execution_kind": "external",
			"external_session": map[string]any{
				"id":       "11111111-1111-4111-8111-111111111111",
				"provider": "paseo", "host_id": "host-1", "session_id": "paseo-1",
				"display_name": "Reviewer", "url": nil, "daemon_id": "daemon-1",
			},
		})
	}))
	defer srv.Close()
	setAgentExternalTestEnv(t, srv.URL)

	cmd := newAgentExternalTestCmd()
	for name, value := range map[string]string{
		"name": "External Reviewer", "host-id": "host-1", "session-id": "paseo-1",
		"display-name": "Reviewer", "daemon-id": "daemon-1", "permission-mode": "private",
	} {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	out, err := captureStdout(t, func() error { return runAgentExternalRegister(cmd, nil) })
	if err != nil {
		t.Fatalf("runAgentExternalRegister: %v", err)
	}
	if body["name"] != "External Reviewer" {
		t.Fatalf("name = %v", body["name"])
	}
	session, ok := body["external_session"].(map[string]any)
	if !ok {
		t.Fatalf("external_session = %#v", body["external_session"])
	}
	for key, want := range map[string]string{
		"provider": "paseo", "host_id": "host-1", "session_id": "paseo-1",
		"display_name": "Reviewer", "daemon_id": "daemon-1",
	} {
		if session[key] != want {
			t.Errorf("external_session[%s] = %v, want %s", key, session[key], want)
		}
	}
	if body["permission_mode"] != "private" {
		t.Errorf("permission_mode = %v", body["permission_mode"])
	}
	if !strings.Contains(out, `"execution_kind": "external"`) {
		t.Fatalf("output = %s", out)
	}
	var response struct {
		Session struct {
			ID string `json:"id"`
		} `json:"external_session"`
	}
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		t.Fatal(err)
	}
	if response.Session.ID != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("registered session id = %q; CLI output must retain the binding target", response.Session.ID)
	}
}

func TestRunAgentExternalRegisterRejectsMismatchedPaseoURL(t *testing.T) {
	cmd := newAgentExternalTestCmd()
	for name, value := range map[string]string{
		"name": "Reviewer", "host-id": "host-1", "session-id": "paseo-1",
		"daemon-id": "daemon-1", "url": "paseo://h/other-host/agent/paseo-1",
	} {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := runAgentExternalRegister(cmd, nil); err == nil || !strings.Contains(err.Error(), "host segment") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateExternalSessionURL(t *testing.T) {
	if err := validateExternalSessionURL("paseo://h/host-1/agent/paseo-1", "paseo", "host-1", "paseo-1"); err != nil {
		t.Fatalf("valid URL rejected: %v", err)
	}
	bad := []string{
		"paseo://user@h/host-1/agent/paseo-1",
		"paseo://h/host-1/agent/paseo-1?x=1",
		"paseo://h/host-1/agent/paseo-1#frag",
		"paseo://h/host-1/other/paseo-1",
		"https://h/host-1/agent/paseo-1",
	}
	for _, raw := range bad {
		if err := validateExternalSessionURL(raw, "paseo", "host-1", "paseo-1"); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}

func TestRunAgentTokenCreateSavesSecretWithoutPrintingIt(t *testing.T) {
	const secret = "mea_super_secret_token"
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/agents/agent-1/tokens" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "token-1", "token": secret, "expires_at": "2027-01-01T00:00:00Z",
		})
	}))
	defer srv.Close()
	setAgentExternalTestEnv(t, srv.URL)

	cmd := newAgentTokenCreateTestCmd()
	_ = cmd.Flags().Set("expires-at", time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	_ = cmd.Flags().Set("save-to", "token.json")
	out, err := captureStdout(t, func() error { return runAgentTokenCreate(cmd, []string{"agent-1"}) })
	if err != nil {
		t.Fatalf("runAgentTokenCreate: %v", err)
	}
	if strings.Contains(out, secret) {
		t.Fatalf("stdout leaked the token: %s", out)
	}
	if body["expires_at"] == "" {
		t.Fatalf("expires_at missing from request: %v", body)
	}
	data, err := os.ReadFile("token.json")
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	var saved map[string]any
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved["token"] != secret || saved["agent_id"] != "agent-1" || saved["server_url"] != srv.URL {
		t.Fatalf("saved token file = %v", saved)
	}
	info, err := os.Stat("token.json")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %o, want 600", info.Mode().Perm())
	}
}

func TestRunAgentTokenCreateBindsExternalSession(t *testing.T) {
	const secret = "mea_bound_secret_token"
	const session = "0199a0b1-3333-7333-8333-333333333333"
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/agents/agent-1/tokens" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "token-1", "token": secret, "expires_at": "2027-01-01T00:00:00Z",
			"external_session_id": session,
		})
	}))
	defer srv.Close()
	setAgentExternalTestEnv(t, srv.URL)

	cmd := newAgentTokenCreateTestCmd()
	_ = cmd.Flags().Set("expires-at", time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	_ = cmd.Flags().Set("external-session", session)
	_ = cmd.Flags().Set("save-to", "token.json")
	if _, err := captureStdout(t, func() error { return runAgentTokenCreate(cmd, []string{"agent-1"}) }); err != nil {
		t.Fatalf("runAgentTokenCreate: %v", err)
	}
	if body["external_session_id"] != session {
		t.Fatalf("request external_session_id = %v, want %s", body["external_session_id"], session)
	}
	data, err := os.ReadFile("token.json")
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	var saved map[string]any
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved["external_session_id"] != session {
		t.Fatalf("saved token file = %v", saved)
	}
}

func TestRunAgentTokenCreateRequiresExplicitSink(t *testing.T) {
	cmd := newAgentTokenCreateTestCmd()
	_ = cmd.Flags().Set("expires-at", time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	if err := runAgentTokenCreate(cmd, []string{"agent-1"}); err == nil || !strings.Contains(err.Error(), "explicit secret sink") {
		t.Fatalf("err = %v", err)
	}

	_ = cmd.Flags().Set("save-to", "token.json")
	_ = cmd.Flags().Set("print-token", "true")
	if err := runAgentTokenCreate(cmd, []string{"agent-1"}); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunAgentTokenCreateSavesToProfile(t *testing.T) {
	const secret = "mea_profile_secret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "token-1", "token": secret, "expires_at": "2027-01-01T00:00:00Z",
		})
	}))
	defer srv.Close()
	setAgentExternalTestEnv(t, srv.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	cmd := newAgentTokenCreateTestCmd()
	_ = cmd.Flags().Set("expires-at", time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	_ = cmd.Flags().Set("save-profile", "external-agent")
	out, err := captureStdout(t, func() error { return runAgentTokenCreate(cmd, []string{"agent-1"}) })
	if err != nil {
		t.Fatalf("runAgentTokenCreate: %v", err)
	}
	if strings.Contains(out, secret) {
		t.Fatalf("stdout leaked the token: %s", out)
	}
	cfg, err := cli.LoadCLIConfigForProfile("external-agent")
	if err != nil {
		t.Fatalf("load saved profile: %v", err)
	}
	if cfg.Token != secret || cfg.ServerURL != srv.URL || cfg.WorkspaceID != "ws-1" {
		t.Fatalf("profile = %+v", cfg)
	}
	path, err := cli.CLIConfigPathForProfile("external-agent")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("profile config mode = %o, want 600", info.Mode().Perm())
	}
}

func TestRunAgentTokenCreateRefusesOverwriteWithoutForce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeSecretFile(path, map[string]any{"token": "new"}, false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("err = %v", err)
	}
	if err := writeSecretFile(path, map[string]any{"token": "new"}, true); err != nil {
		t.Fatalf("force write: %v", err)
	}
}

func TestRunAgentTokenCreatePrintTokenIsExplicit(t *testing.T) {
	const secret = "mea_printed_secret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "token-1", "token": secret, "expires_at": "2027-01-01T00:00:00Z",
		})
	}))
	defer srv.Close()
	setAgentExternalTestEnv(t, srv.URL)

	cmd := newAgentTokenCreateTestCmd()
	_ = cmd.Flags().Set("expires-at", time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	_ = cmd.Flags().Set("print-token", "true")
	out, err := captureStdout(t, func() error { return runAgentTokenCreate(cmd, []string{"agent-1"}) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != secret {
		t.Fatalf("stdout = %q, want only the token", out)
	}
}

func TestRunAgentTokenRevokeUsesDelete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/agents/agent-1/tokens/token-1" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	setAgentExternalTestEnv(t, srv.URL)

	cmd := newAgentTokenRevokeTestCmd()
	out, err := captureStdout(t, func() error { return runAgentTokenRevoke(cmd, []string{"agent-1", "token-1"}) })
	if err != nil {
		t.Fatalf("runAgentTokenRevoke: %v", err)
	}
	if !strings.Contains(out, `"revoked": true`) {
		t.Fatalf("output = %s", out)
	}
}

func TestRunAgentMailboxListUsesCursorAndLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/agents/agent-1/mailbox" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("limit") != "25" || r.URL.Query().Get("after") != "cursor-1" {
			t.Fatalf("query = %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"messages":[{"id":"msg-1","delivery_status":"pending","processing_status":"unread","issue_id":"issue-1","created_at":"2026-10-23T09:30:50Z"}],"next_cursor":"cursor-2"}`))
	}))
	defer srv.Close()
	setAgentExternalTestEnv(t, srv.URL)

	cmd := newAgentMailboxListTestCmd()
	_ = cmd.Flags().Set("output", "json")
	_ = cmd.Flags().Set("after", "cursor-1")
	_ = cmd.Flags().Set("limit", "25")
	out, err := captureStdout(t, func() error { return runAgentMailboxList(cmd, []string{"agent-1"}) })
	if err != nil {
		t.Fatalf("runAgentMailboxList: %v", err)
	}
	if !strings.Contains(out, `"id": "msg-1"`) || !strings.Contains(out, `"next_cursor": "cursor-2"`) {
		t.Fatalf("output = %s", out)
	}
}

func TestRunAgentMailboxListTableShowsResultOrderingFacts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"messages":[` +
			`{"id":"msg-1","kind":"delegation_result","payload":{"result_task_id":"run-1","outcome":"completed","contract_sequence":4,"contract_delivery_count":2},"delivery_status":"delivered","processing_status":"unread","issue_id":"issue-1","created_at":"2026-10-23T09:30:50Z"},` +
			`{"id":"msg-2","kind":"delegation_result","payload":{"result_task_id":"run-2","outcome":"failed"},"delivery_status":"pending","processing_status":"unread","issue_id":"issue-2","created_at":"2026-10-23T09:31:50Z"}` +
			`]}`))
	}))
	defer srv.Close()
	setAgentExternalTestEnv(t, srv.URL)

	cmd := newAgentMailboxListTestCmd()
	_ = cmd.Flags().Set("output", "table")
	out, err := captureStdout(t, func() error { return runAgentMailboxList(cmd, []string{"agent-1"}) })
	if err != nil {
		t.Fatalf("runAgentMailboxList: %v", err)
	}
	if !strings.Contains(out, "run=run-1 completed seq=4 deliveries=2") || !strings.Contains(out, "run=run-2 failed") || strings.Contains(out, "run=run-2 failed seq=") {
		t.Fatalf("output = %s", out)
	}
}

func TestRunAgentMailboxAckSendsState(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/agents/agent-1/mailbox/msg-1/ack" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"id":"msg-1","processing_status":"read"}`))
	}))
	defer srv.Close()
	setAgentExternalTestEnv(t, srv.URL)

	cmd := newAgentMailboxAckTestCmd()
	_ = cmd.Flags().Set("state", "read")
	out, err := captureStdout(t, func() error { return runAgentMailboxAck(cmd, []string{"agent-1", "msg-1"}) })
	if err != nil {
		t.Fatalf("runAgentMailboxAck: %v", err)
	}
	if body["state"] != "read" {
		t.Fatalf("body = %v", body)
	}
	if !strings.Contains(out, `"processing_status": "read"`) {
		t.Fatalf("output = %s", out)
	}
}

func TestRunAgentMailboxAckRejectsInvalidState(t *testing.T) {
	cmd := newAgentMailboxAckTestCmd()
	_ = cmd.Flags().Set("state", "done")
	if err := runAgentMailboxAck(cmd, []string{"agent-1", "msg-1"}); err == nil || !strings.Contains(err.Error(), "read or handled") {
		t.Fatalf("err = %v", err)
	}
}

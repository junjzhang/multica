package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
)

func newIdentityTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "issue-create"}
	cmd.Flags().String("server-url", "", "")
	cmd.Flags().String("workspace-id", "", "")
	cmd.Flags().String("profile", "", "")
	cmd.Flags().String("return-session", "", "")
	return cmd
}

// Every business client fails closed when a Paseo session only has the human
// profile: the server would classify it as a member and exempt the delegation
// gate. The explicit owner entry and manual external credentials outside
// Paseo keep working.
func TestAgentSessionBusinessClientRejectsHumanProfile(t *testing.T) {
	// /tmp has no daemon task marker above it; the worktree does, because this
	// suite runs inside a managed task of the authoring session.
	t.Chdir("/tmp")
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "00000000-0000-0000-0000-000000000001")
	t.Setenv("PASEO_AGENT_ID", "paseo-session-1")
	t.Setenv("MULTICA_TOKEN", "mul_human_profile")
	t.Setenv("MULTICA_AGENT_ID", "")
	t.Setenv("MULTICA_TASK_ID", "")

	business := newIdentityTestCmd()
	if _, err := newAPIClient(business); err == nil || !strings.Contains(err.Error(), "Paseo session") {
		t.Fatalf("business command with human profile: err = %v, want a Paseo identity refusal", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("refused client sent %d requests; the refusal must precede the side effect", requests.Load())
	}

	owner := newIdentityTestCmd()
	if _, err := newOwnerAPIClient(owner); err != nil {
		t.Fatalf("explicit owner/credential entry was blocked: %v", err)
	}

	// Manual external credentials outside a file-bound Paseo session remain
	// available, with an explicit return address rather than a profile fallback.
	t.Setenv("PASEO_AGENT_ID", "")
	t.Setenv("MULTICA_RETURN_SESSION", "paseo-session-1")
	t.Setenv("MULTICA_TOKEN", "mea_external_agent")
	client, err := newAPIClient(business)
	if err != nil {
		t.Fatalf("external agent credential was refused: %v", err)
	}
	if client.ReturnSession != "paseo-session-1" {
		t.Fatalf("return session = %q, want the current Paseo session", client.ReturnSession)
	}
}

// Outside a Paseo session the ordinary human flow is unchanged.
func TestHumanClientOutsidePaseoSessionUsesHumanProfile(t *testing.T) {
	t.Chdir("/tmp")
	setCLITestServerEnv(t, "http://127.0.0.1:1")
	t.Setenv("PASEO_AGENT_ID", "")
	t.Setenv("MULTICA_AGENT_ID", "")
	t.Setenv("MULTICA_TASK_ID", "")
	t.Setenv("MULTICA_TOKEN", "mul_human_profile")

	if _, err := newAPIClient(newIdentityTestCmd()); err != nil {
		t.Fatalf("human command outside a Paseo session was blocked: %v", err)
	}
}

func TestManagedTaskIdentityOverridesInheritedSession(t *testing.T) {
	// /tmp has no daemon task marker above it; the worktree does, because this
	// suite runs inside a managed task of the authoring session.
	t.Chdir("/tmp")
	setCLITestServerEnv(t, "http://127.0.0.1:1")
	t.Setenv("PASEO_AGENT_ID", "inherited-paseo-session")
	t.Setenv("MULTICA_TOKEN", "mat_task_token")
	t.Setenv("MULTICA_AGENT_ID", "agent-1")
	t.Setenv("MULTICA_TASK_ID", "task-1")

	client, err := newAPIClient(newIdentityTestCmd())
	if err != nil {
		t.Fatalf("managed task client: %v", err)
	}
	if client.ReturnSession != "" {
		t.Fatalf("managed task inherited a return session %q; its task token is the address", client.ReturnSession)
	}

	explicit := newIdentityTestCmd()
	if err := explicit.Flags().Set("return-session", "paseo-other"); err != nil {
		t.Fatal(err)
	}
	if _, err := newAPIClient(explicit); err == nil || !strings.Contains(err.Error(), "managed task") {
		t.Fatalf("explicit return session inside a managed task: err = %v, want a refusal", err)
	}
}

func TestExternalReturnSessionFlagAndEnv(t *testing.T) {
	// /tmp has no daemon task marker above it; the worktree does, because this
	// suite runs inside a managed task of the authoring session.
	t.Chdir("/tmp")
	setCLITestServerEnv(t, "http://127.0.0.1:1")
	t.Setenv("PASEO_AGENT_ID", "")
	t.Setenv("MULTICA_AGENT_ID", "")
	t.Setenv("MULTICA_TASK_ID", "")
	t.Setenv("MULTICA_RETURN_SESSION", "env-session")

	cmd := newIdentityTestCmd()
	client, err := newAPIClient(cmd)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if client.ReturnSession != "env-session" {
		t.Fatalf("return session = %q, want MULTICA_RETURN_SESSION", client.ReturnSession)
	}

	overridden := newIdentityTestCmd()
	if err := overridden.Flags().Set("return-session", "flag-session"); err != nil {
		t.Fatal(err)
	}
	client, err = newAPIClient(overridden)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if client.ReturnSession != "flag-session" {
		t.Fatalf("return session = %q, want the explicit flag", client.ReturnSession)
	}
}

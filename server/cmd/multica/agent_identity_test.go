package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

func newIdentityTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "issue-create"}
	cmd.Flags().String("server-url", "", "")
	cmd.Flags().String("workspace-id", "", "")
	cmd.Flags().String("profile", "", "")
	cmd.Flags().String("return-session", "", "")
	return cmd
}

// A Paseo session that only has the human profile must not let a business
// command silently authenticate as that human: the server would classify it as
// a member and exempt the delegation gate. Explicit owner commands and a
// session with an agent credential keep working.
func TestAgentBusinessCommandInPaseoSessionRejectsHumanProfile(t *testing.T) {
	// /tmp has no daemon task marker above it; the worktree does, because this
	// suite runs inside a managed task of the authoring session.
	t.Chdir("/tmp")
	setCLITestServerEnv(t, "http://127.0.0.1:1")
	t.Setenv("PASEO_AGENT_ID", "paseo-session-1")
	t.Setenv("MULTICA_TOKEN", "mul_human_profile")
	t.Setenv("MULTICA_AGENT_ID", "")
	t.Setenv("MULTICA_TASK_ID", "")

	business := newIdentityTestCmd()
	cli.MarkAgentBusinessCommand(business)
	if _, err := newAPIClient(business); err == nil || !strings.Contains(err.Error(), "Paseo agent session") {
		t.Fatalf("business command with human profile: err = %v, want a Paseo identity refusal", err)
	}

	owner := newIdentityTestCmd()
	if _, err := newAPIClient(owner); err != nil {
		t.Fatalf("unmarked owner/credential command was blocked: %v", err)
	}

	t.Setenv("MULTICA_TOKEN", "mea_external_agent")
	client, err := newAPIClient(business)
	if err != nil {
		t.Fatalf("external agent credential was refused: %v", err)
	}
	if client.ReturnSession != "paseo-session-1" {
		t.Fatalf("return session = %q, want the current Paseo session", client.ReturnSession)
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

	business := newIdentityTestCmd()
	cli.MarkAgentBusinessCommand(business)
	client, err := newAPIClient(business)
	if err != nil {
		t.Fatalf("managed task client: %v", err)
	}
	if client.ReturnSession != "" {
		t.Fatalf("managed task inherited a return session %q; its task token is the address", client.ReturnSession)
	}

	explicit := newIdentityTestCmd()
	cli.MarkAgentBusinessCommand(explicit)
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

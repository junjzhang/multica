package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

const (
	// PaseoAgentIDEnv names the host session a Paseo agent launch runs in. Its
	// presence means this process is working on behalf of that agent session,
	// even when the person who owns the machine also has a human profile.
	PaseoAgentIDEnv = "PASEO_AGENT_ID"

	// AgentBusinessCommandAnnotation marks commands whose effects are an
	// agent's own work (creating, assigning, commenting, rerunning). When such
	// a command runs inside a Paseo session it must authenticate as the agent;
	// silently using the human profile would turn the delegation gate's
	// actorType check into a bypass.
	AgentBusinessCommandAnnotation = "multica.io/agent-business"

	// ExternalAgentTokenPrefix is the machine credential that authenticates one
	// registered external agent session. Task-scoped mat_ tokens are also agent
	// credentials and are accepted where a task context applies.
	ExternalAgentTokenPrefix = "mea_"
	// AgentTaskTokenPrefix authenticates one daemon-managed task run.
	AgentTaskTokenPrefix = "mat_"
)

// MarkAgentBusinessCommand marks cmd as work performed on behalf of the agent
// session it runs in. Explicit owner/credential-management commands are
// deliberately not marked: they are how a human registers or repairs the
// agent identity this guard depends on.
func MarkAgentBusinessCommand(cmd *cobra.Command) {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[AgentBusinessCommandAnnotation] = "true"
}

func isAgentBusinessCommand(cmd *cobra.Command) bool {
	for current := cmd; current != nil; current = current.Parent() {
		if current.Annotations[AgentBusinessCommandAnnotation] == "true" {
			return true
		}
	}
	return false
}

// RequireAgentBusinessIdentity fails closed when an agent business command runs
// inside a Paseo session without an agent credential. A human profile token
// would make the server see a member actor and exempt the delegation gate, so
// the command must not silently select it. Explicit owner registration and
// credential management commands, and ordinary human commands outside a Paseo
// session, are unaffected.
func RequireAgentBusinessIdentity(cmd *cobra.Command, token string, inPaseoSession, inManagedTask bool) error {
	if inManagedTask || !inPaseoSession || !isAgentBusinessCommand(cmd) {
		return nil
	}
	if strings.HasPrefix(strings.TrimSpace(token), ExternalAgentTokenPrefix) ||
		strings.HasPrefix(strings.TrimSpace(token), AgentTaskTokenPrefix) {
		return nil
	}
	return fmt.Errorf(
		"%s runs inside a Paseo agent session (%s is set) but no agent credential is available; authenticate this session as its external agent instead of silently acting as the human profile (set MULTICA_TOKEN to the session's %s token, or run the explicit agent credential command first)",
		cmd.CommandPath(), PaseoAgentIDEnv, ExternalAgentTokenPrefix,
	)
}

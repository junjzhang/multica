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

	// ExternalAgentTokenPrefix is the machine credential that authenticates one
	// registered external agent session. Task-scoped mat_ tokens are also agent
	// credentials and are accepted where a task context applies.
	ExternalAgentTokenPrefix = "mea_"
	// AgentTaskTokenPrefix authenticates one daemon-managed task run.
	AgentTaskTokenPrefix = "mat_"
)

// RequireAgentSessionIdentity fails closed when a Paseo agent session tries to
// authenticate ordinary business traffic with a human profile. The server
// would classify that caller as a member and exempt the delegation gate, so a
// business client must never silently select the human token.
//
// This is the default rule every business client applies. Explicit owner
// registration, credential management, and bootstrap reads call a separate
// owner client instead of opting out here; ordinary human commands outside a
// Paseo session and managed task runs are unaffected.
func RequireAgentSessionIdentity(cmd *cobra.Command, token string, inPaseoSession, inManagedTask bool) error {
	if inManagedTask || !inPaseoSession {
		return nil
	}
	if strings.HasPrefix(strings.TrimSpace(token), ExternalAgentTokenPrefix) ||
		strings.HasPrefix(strings.TrimSpace(token), AgentTaskTokenPrefix) {
		return nil
	}
	name := "multica"
	if cmd != nil {
		name = cmd.CommandPath()
	}
	return fmt.Errorf(
		"%s runs inside a Paseo agent session (%s is set) but no agent credential is available; authenticate this session as its external agent instead of silently acting as the human profile (set MULTICA_TOKEN to the session's %s token, or run the explicit agent credential command first)",
		name, PaseoAgentIDEnv, ExternalAgentTokenPrefix,
	)
}

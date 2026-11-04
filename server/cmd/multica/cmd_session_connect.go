package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var sessionConnectCmd = &cobra.Command{
	Use:   "connect",
	Short: "Connect the current Paseo session through its installed Multica plugin",
	Args:  cobra.NoArgs,
	RunE:  runSessionConnect,
}

func init() {
	sessionCmd.AddCommand(sessionConnectCmd)
	sessionConnectCmd.Flags().String("output", "table", "Output format: table or json")
}

func runSessionConnect(cmd *cobra.Command, _ []string) error {
	if output := flagString(cmd, "output"); output != "json" && output != "table" {
		return fmt.Errorf("--output must be table or json")
	}
	agentID, err := sessionConnectPreflight(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 60*time.Second)
	defer cancel()
	input, _ := json.Marshal(map[string]string{"agentId": agentID})
	process := exec.CommandContext(ctx, "paseo", "plugin", "rpc", "paseo-multica", "session.connect", "--input-json", string(input), "--json")
	var stdout, stderr bytes.Buffer
	process.Stdout, process.Stderr = &stdout, &stderr
	if err := process.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("Paseo session connect interrupted or timed out; inspect `multica session status` before retrying")
		}
		// Never echo arbitrary subprocess output: RPC failures can include credentials.
		if strings.Contains(stderr.String(), "unknown command 'rpc'") {
			return fmt.Errorf("Paseo CLI lacks `plugin rpc`; update Paseo before connecting")
		}
		var failure struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(stderr.Bytes(), &failure) == nil {
			message := strings.ToLower(failure.Error.Message)
			for _, reason := range []struct{ match, safe string }{
				{"plugin is not available", "Paseo Multica plugin is unavailable; enable paseo-multica on the current host"},
				{"does not contribute rpc", "Paseo Multica plugin lacks session.connect; update the plugin"},
				{"session is archived", "the current Paseo session is archived; restore it before connecting"},
				{"session no longer exists", "the current session does not exist on the selected Paseo host; check the daemon target"},
				{"refusing to rebind", "the existing binding conflicts with the plugin's host, server, workspace or daemon; restore its original configuration before connecting"},
				{"refusing to overwrite", "the existing binding is incompatible; inspect it before connecting"},
			} {
				if strings.Contains(message, reason.match) {
					return fmt.Errorf("%s", reason.safe)
				}
			}
		}
		return fmt.Errorf("Paseo session connect failed; check the current host's paseo-multica plugin and its connection settings")
	}
	var connected struct {
		State     string `json:"state"`
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(stdout.Bytes(), &connected) != nil || connected.State != "connected" || connected.SessionID != agentID {
		return fmt.Errorf("Paseo plugin did not confirm a connection for the current session; inspect `multica session status`")
	}
	return runSessionStatus(cmd, nil)
}

func sessionConnectPreflight(cmd *cobra.Command) (string, error) {
	if inDaemonManagedExecutionContext() || inDaemonTaskIdentityContext() {
		return "", fmt.Errorf("cannot connect an external session inside a daemon-managed task; its task credential is authoritative")
	}
	agentID := strings.TrimSpace(os.Getenv(paseoAgentIDEnv))
	if !externalSessionIDCLIPattern.MatchString(agentID) {
		return "", fmt.Errorf("%s must identify the current Paseo session", paseoAgentIDEnv)
	}
	// The plugin's settings select the endpoint and workspace before issuing a token.
	// Reject overrides before RPC, including when there is no local binding yet.
	for _, flag := range []string{"profile", "server-url", "workspace-id"} {
		if inheritedFlagChanged(cmd, flag) {
			return "", fmt.Errorf("--%s cannot be used with session connect; the Paseo plugin selects its connection", flag)
		}
	}
	for _, key := range []string{"MULTICA_TOKEN", "MULTICA_SERVER_URL", "MULTICA_WORKSPACE_ID"} {
		if strings.TrimSpace(os.Getenv(key)) != "" {
			return "", fmt.Errorf("unset %s before session connect; the Paseo plugin selects its connection", key)
		}
	}
	if inheritedFlagChanged(cmd, "return-session") && flagValue(cmd, "return-session") != agentID {
		return "", fmt.Errorf("--return-session conflicts with the current Paseo session")
	}
	if value := strings.TrimSpace(os.Getenv("MULTICA_RETURN_SESSION")); value != "" && value != agentID {
		return "", fmt.Errorf("MULTICA_RETURN_SESSION conflicts with the current Paseo session")
	}
	dir, err := cli.ProfileDir("")
	if err != nil {
		return "", err
	}
	path, _, _, err := externalSessionFilePath()
	if err != nil {
		return "", err
	}
	if path != filepath.Join(dir, "external-sessions", externalSessionProvider, agentID+".json") {
		return "", fmt.Errorf("%s must name the current session's default binding file before connecting", externalSessionFileEnv)
	}
	return agentID, nil
}

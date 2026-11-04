package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Written by integrations/paseo-multica/server/service.ts after redaction.
const sessionConnectErrorPrefix = "[multica:session-connect] "

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
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("Paseo CLI was not found on PATH; install Paseo with `plugin rpc` support before connecting")
		}
		if ctx.Err() != nil {
			return fmt.Errorf("Paseo session connect interrupted or timed out; inspect `multica session status` before retrying")
		}
		// Unmarked subprocess output remains private, including runtime warnings.
		if strings.Contains(stderr.String(), "unknown command 'rpc'") {
			return fmt.Errorf("Paseo CLI lacks `plugin rpc`; update Paseo before connecting")
		}
		if diagnostic := sessionConnectRPCError(stderr.Bytes()); diagnostic != nil {
			return diagnostic
		}
		return fmt.Errorf("Paseo session connect failed; ensure paseo-multica and its session.connect method are available on the selected host and check its connection settings")
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

func sessionConnectRPCError(data []byte) error {
	for len(data) > 0 {
		start := bytes.IndexByte(data, '{')
		if start < 0 {
			break
		}
		data = data[start:]
		var failure struct {
			Error struct {
				Code        string `json:"code"`
				Message     string `json:"message"`
				UserMessage string `json:"userMessage"`
			} `json:"error"`
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		if decoder.Decode(&failure) != nil {
			data = data[1:]
			continue
		}
		if failure.Error.Code == "DAEMON_UPDATE_REQUIRED" {
			return errors.New("the selected Paseo daemon does not support plugin RPCs; update the Paseo host before connecting")
		}
		message := failure.Error.UserMessage
		if message == "" {
			message = failure.Error.Message
		}
		if marker := strings.Index(message, sessionConnectErrorPrefix); marker >= 0 {
			message = safeSessionDiagnostic(message[marker+len(sessionConnectErrorPrefix):])
			if strings.TrimSpace(message) != "" {
				return errors.New(message)
			}
		}
		// Warnings may contain complete JSON objects before the actual envelope.
		data = data[int(decoder.InputOffset()):]
	}
	return nil
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
	defaultPath, err := defaultExternalSessionFilePath(agentID)
	if err != nil {
		return "", err
	}
	path, _, _, err := externalSessionFilePath()
	if err != nil {
		return "", err
	}
	if path != defaultPath {
		return "", fmt.Errorf("%s must name the current session's default binding file before connecting", externalSessionFileEnv)
	}
	return agentID, nil
}

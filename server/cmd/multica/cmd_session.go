package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var sessionCmd = &cobra.Command{
	Use:   "session",
	Short: "Inspect this process's external session identity",
}

var sessionStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the external session binding, its return address and whether it is still valid",
	Long: `Show the external session binding that ordinary commands in this process use.

The binding is the private file the Paseo plugin writes for the current
PASEO_AGENT_ID, or the file named by MULTICA_EXTERNAL_SESSION_FILE. Status
prints non-secret identity, the original conversation's return address, and
whether the server still accepts the credential.

A file on disk is not proof of authorization: valid is true only after the
server confirms the credential and that the binding still names this session.
The token itself is never printed.`,
	RunE: runSessionStatus,
}

func init() {
	sessionCmd.GroupID = groupAdditional
	sessionCmd.AddCommand(sessionStatusCmd)
	sessionStatusCmd.Flags().String("output", "table", "Output format: table or json")
}

// sessionStatusReport is the non-secret inspection result. It deliberately has
// no field that can hold the token or the owner's human identity.
type sessionStatusReport struct {
	Status      string `json:"status"` // connected, invalid or disconnected
	Valid       bool   `json:"valid"`
	ContextFile string `json:"context_file,omitempty"`
	// ExplicitContextFile reports that MULTICA_EXTERNAL_SESSION_FILE chose the
	// file rather than PASEO_AGENT_ID's default path.
	ExplicitContextFile bool                  `json:"explicit_context_file,omitempty"`
	Provider            string                `json:"provider,omitempty"`
	HostID              string                `json:"host_id,omitempty"`
	SessionID           string                `json:"session_id,omitempty"`
	AgentID             string                `json:"agent_id,omitempty"`
	DaemonID            string                `json:"daemon_id,omitempty"`
	ServerURL           string                `json:"server_url,omitempty"`
	AppURL              string                `json:"app_url,omitempty"`
	WorkspaceID         string                `json:"workspace_id,omitempty"`
	TokenID             string                `json:"token_id,omitempty"`
	ExpiresAt           string                `json:"expires_at,omitempty"`
	ReturnAddress       *sessionReturnAddress `json:"return_address,omitempty"`
	Remote              *sessionRemoteReport  `json:"remote,omitempty"`
	Error               string                `json:"error,omitempty"`
}

// sessionReturnAddress is where a handed-back result must land: the existing
// Paseo conversation, reachable through the authorized Multica daemon.
type sessionReturnAddress struct {
	Provider  string `json:"provider"`
	HostID    string `json:"host_id"`
	SessionID string `json:"session_id"`
	DaemonID  string `json:"daemon_id"`
	URL       string `json:"url"`
}

// sessionRemoteReport is the server's answer about the binding. Checked=false
// means the answer is unknown, which is never presented as valid.
type sessionRemoteReport struct {
	Checked       bool   `json:"checked"`
	AgentName     string `json:"agent_name,omitempty"`
	ExecutionKind string `json:"execution_kind,omitempty"`
	SessionID     string `json:"session_id,omitempty"`
	Error         string `json:"error,omitempty"`
}

// sessionAgentResponse is the part of GET /api/agents/{id} this command reads.
type sessionAgentResponse struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	ExecutionKind   string `json:"execution_kind"`
	ExternalSession *struct {
		Provider  string `json:"provider"`
		HostID    string `json:"host_id"`
		SessionID string `json:"session_id"`
		DaemonID  string `json:"daemon_id"`
	} `json:"external_session"`
}

func runSessionStatus(cmd *cobra.Command, _ []string) error {
	report := inspectExternalSession(context.Background())
	output := flagString(cmd, "output")
	if output == "table" {
		printSessionStatus(report)
		return nil
	}
	if output != "json" {
		return fmt.Errorf("--output must be table or json")
	}
	return cli.PrintJSON(os.Stdout, report)
}

// inspectExternalSession never turns a binding problem into a command error:
// the point of the command is to report the state, including the broken ones.
// Binding problems stay actionable through the error field.
func inspectExternalSession(parent context.Context) sessionStatusReport {
	path, explicit, present, err := externalSessionFilePath()
	if err != nil {
		return sessionStatusReport{Status: "invalid", Error: err.Error()}
	}
	if !present {
		return sessionStatusReport{
			Status: "disconnected",
			Error:  "no external session context in this process; run `/multica connect` in the Paseo session, or pass " + externalSessionFileEnv,
		}
	}
	report := sessionStatusReport{Status: "invalid", ContextFile: path, ExplicitContextFile: explicit}
	if inDaemonManagedExecutionContext() {
		report.Error = "this process runs inside a daemon-managed task, so its task credential is authoritative and the external session binding is not used"
		return report
	}
	binding, err := readExternalSessionContext()
	if err != nil {
		report.Error = err.Error()
		return report
	}
	report.Provider = binding.Provider
	report.HostID = binding.HostID
	report.SessionID = binding.SessionID
	report.AgentID = binding.AgentID
	report.DaemonID = binding.DaemonID
	report.ServerURL = binding.ServerURL
	report.AppURL = binding.AppURL
	report.WorkspaceID = binding.WorkspaceID
	report.TokenID = binding.TokenID
	report.ExpiresAt = binding.ExpiresAt
	report.ReturnAddress = &sessionReturnAddress{
		Provider:  binding.Provider,
		HostID:    binding.HostID,
		SessionID: binding.SessionID,
		DaemonID:  binding.DaemonID,
		URL:       binding.sessionReturnURL(),
	}

	remote := verifySessionBinding(parent, binding)
	report.Remote = &remote
	report.Valid = remote.Error == ""
	if report.Valid {
		report.Status = "connected"
	} else {
		report.Error = remote.Error
	}
	return report
}

// verifySessionBinding asks the server whether this credential still authorizes
// this exact session. A reachable server that names a different session is a
// rebinding, not a success.
func verifySessionBinding(parent context.Context, binding *externalSessionContext) sessionRemoteReport {
	ctx, cancel := cli.APIContext(parent)
	defer cancel()

	client := cli.NewAPIClient(binding.ServerURL, binding.WorkspaceID, binding.Token)
	path := "/api/agents/" + url.PathEscape(binding.AgentID)
	var agent sessionAgentResponse
	if err := client.GetJSON(ctx, path, &agent); err != nil {
		return sessionRemoteReport{Checked: true, Error: describeSessionRemoteError(err)}
	}
	report := sessionRemoteReport{Checked: true, AgentName: agent.Name, ExecutionKind: agent.ExecutionKind}
	if agent.ExternalSession != nil {
		report.SessionID = agent.ExternalSession.SessionID
	}
	switch {
	case agent.ID != binding.AgentID:
		report.Error = fmt.Sprintf("server returned agent %s, not the recorded %s", agent.ID, binding.AgentID)
	case agent.ExecutionKind != "external":
		report.Error = fmt.Sprintf("server reports execution_kind %q, not external", agent.ExecutionKind)
	case agent.ExternalSession == nil:
		report.Error = "server no longer returns an external session for this agent"
	case agent.ExternalSession.SessionID != binding.SessionID:
		report.Error = fmt.Sprintf("server binds agent to session %s, not the recorded %s", agent.ExternalSession.SessionID, binding.SessionID)
	case agent.ExternalSession.HostID != binding.HostID:
		report.Error = fmt.Sprintf("server binds agent to host %s, not the recorded %s", agent.ExternalSession.HostID, binding.HostID)
	}
	return report
}

// describeSessionRemoteError keeps the credential out of the message while
// still telling an expired/revoked token from an unreachable server.
func describeSessionRemoteError(err error) string {
	var httpErr *cli.HTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return "server rejected the session credential; reconnect the session to issue a new one"
		case http.StatusNotFound:
			return "the recorded agent no longer exists in this workspace"
		}
	}
	return fmt.Sprintf("could not verify the binding with the server: %v", err)
}

func printSessionStatus(report sessionStatusReport) {
	fmt.Printf("Status:       %s\n", report.Status)
	fmt.Printf("Valid:        %t\n", report.Valid)
	if report.ContextFile != "" {
		fmt.Printf("Context file: %s\n", report.ContextFile)
	}
	if report.AgentID != "" {
		fmt.Printf("Agent:        %s\n", report.AgentID)
	}
	if report.ReturnAddress != nil {
		fmt.Printf("Return to:    %s (daemon %s)\n", report.ReturnAddress.URL, report.ReturnAddress.DaemonID)
	}
	if report.ServerURL != "" {
		fmt.Printf("Server:       %s\n", report.ServerURL)
	}
	if report.WorkspaceID != "" {
		fmt.Printf("Workspace:    %s\n", report.WorkspaceID)
	}
	if report.ExpiresAt != "" {
		fmt.Printf("Expires:      %s\n", report.ExpiresAt)
	}
	if report.Error != "" {
		fmt.Printf("Problem:      %s\n", strings.TrimSpace(report.Error))
	}
}

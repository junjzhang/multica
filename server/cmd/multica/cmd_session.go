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
server confirms that the credential itself belongs to the recorded agent and
that the recorded provider, host, session and daemon still match. The token
itself is never printed.

The command always writes the complete report. It exits 0 only when status is
connected and valid; invalid and disconnected exit non-zero so a script cannot
mistake a bad binding for a working one.`,
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
	Provider      string `json:"provider,omitempty"`
	HostID        string `json:"host_id,omitempty"`
	SessionID     string `json:"session_id,omitempty"`
	DaemonID      string `json:"daemon_id,omitempty"`
	Error         string `json:"error,omitempty"`
}

// sessionAgentResponse is the part of GET /api/agents/{id} this command reads.
type sessionAgentResponse struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ExecutionKind string `json:"execution_kind"`
}

type sessionRegisteredAddress struct {
	Provider  string `json:"provider"`
	HostID    string `json:"host_id"`
	SessionID string `json:"session_id"`
	DaemonID  string `json:"daemon_id"`
}

func runSessionStatus(cmd *cobra.Command, _ []string) error {
	output := flagString(cmd, "output")
	if output != "json" && output != "table" {
		return fmt.Errorf("--output must be table or json")
	}
	report := inspectExternalSession(cmd.Context(), cmd)
	if output == "table" {
		printSessionStatus(report)
	} else if err := cli.PrintJSON(os.Stdout, report); err != nil {
		return err
	}
	// The report is the answer even when it is bad; the exit code is the
	// script-facing summary. errSilent keeps stderr from repeating it.
	if !report.Valid {
		return errSilent
	}
	return nil
}

// inspectExternalSession never turns a binding problem into a printing error:
// the point of the command is to report the state, including the broken ones.
// Binding problems stay actionable through the error field.
func inspectExternalSession(parent context.Context, cmd *cobra.Command) sessionStatusReport {
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
		// A default path with no file means the session was never connected (or
		// `/multica disconnect` removed it). An explicit file that is missing is
		// an operator mistake, so that stays invalid.
		if errors.Is(err, errExternalSessionFileMissing) && !explicit {
			report.Status = "disconnected"
		}
		report.Error = err.Error()
		return report
	}
	if err := sessionContextConflicts(cmd, binding); err != nil {
		// The same conflict makes ordinary commands refuse, so status must not
		// claim a binding that those commands would not use. Keep the readable
		// identity in the report; only validity and the explanation change.
		report.Error = err.Error()
		report.fillBinding(binding)
		return report
	}
	report.fillBinding(binding)

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

// fillBinding copies the non-secret local fields, so every state that could
// read the file still explains what it read.
func (r *sessionStatusReport) fillBinding(binding *externalSessionContext) {
	r.Provider = binding.Provider
	r.HostID = binding.HostID
	r.SessionID = binding.SessionID
	r.AgentID = binding.AgentID
	r.DaemonID = binding.DaemonID
	r.ServerURL = binding.ServerURL
	r.AppURL = binding.AppURL
	r.WorkspaceID = binding.WorkspaceID
	r.TokenID = binding.TokenID
	r.ExpiresAt = binding.ExpiresAt
	r.ReturnAddress = &sessionReturnAddress{
		Provider:  binding.Provider,
		HostID:    binding.HostID,
		SessionID: binding.SessionID,
		DaemonID:  binding.DaemonID,
		URL:       binding.sessionReturnURL(),
	}
}

// verifySessionBinding checks the credential subject, agent metadata and exact
// registered session tuple. The agent's presentation session is not a route.
func verifySessionBinding(parent context.Context, binding *externalSessionContext) sessionRemoteReport {
	ctx, cancel := cli.APIContext(parent)
	defer cancel()

	client := cli.NewAPIClient(binding.ServerURL, binding.WorkspaceID, binding.Token)
	report := sessionRemoteReport{Checked: true}

	// The own-mailbox read is the identity check. It must come before any
	// claim based on the agent's visible record: GET /api/agents/{id} succeeds
	// by workspace visibility even when the token belongs to a different agent
	// in the same workspace.
	if err := verifyExternalSessionActor(ctx, client, binding.AgentID); err != nil {
		if errors.Is(err, errExternalSessionActorMismatch) {
			report.Error = "the session credential authenticates a different agent than the binding declares; reconnect the session so it issues a credential for this agent"
		} else {
			report.Error = describeSessionRemoteError(err)
		}
		return report
	}

	path := "/api/agents/" + url.PathEscape(binding.AgentID)
	var agent sessionAgentResponse
	if err := client.GetJSON(ctx, path, &agent); err != nil {
		return sessionRemoteReport{Checked: true, Error: describeSessionRemoteError(err)}
	}
	report.AgentName = agent.Name
	report.ExecutionKind = agent.ExecutionKind
	switch {
	case agent.ID != binding.AgentID:
		report.Error = fmt.Sprintf("server returned agent %s, not the recorded %s", agent.ID, binding.AgentID)
		return report
	case agent.ExecutionKind != "external":
		report.Error = fmt.Sprintf("server reports execution_kind %q, not external", agent.ExecutionKind)
		return report
	}
	// The agent DTO shows one presentation session. Only the session registry
	// can verify a file for another conversation belonging to the same agent.
	var registry struct {
		Sessions []sessionRegisteredAddress `json:"sessions"`
	}
	if err := client.GetJSON(ctx, path+"/sessions", &registry); err != nil {
		report.Error = "cannot verify registered session: " + describeSessionRemoteError(err)
		return report
	}
	matches := 0
	for _, session := range registry.Sessions {
		if session.Provider != binding.Provider || session.HostID != binding.HostID || session.SessionID != binding.SessionID {
			continue
		}
		matches++
		report.Provider, report.HostID, report.SessionID, report.DaemonID = session.Provider, session.HostID, session.SessionID, session.DaemonID
	}
	switch {
	case matches == 0:
		report.Error = "the recorded provider/host/session is not registered to this agent"
	case matches > 1:
		report.Error = "the registered provider/host/session is ambiguous"
	case report.DaemonID != binding.DaemonID:
		report.Error = fmt.Sprintf("server routes this session through daemon %s, not the recorded %s", report.DaemonID, binding.DaemonID)
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
	if report.Provider != "" || report.HostID != "" || report.SessionID != "" || report.DaemonID != "" {
		fmt.Printf("Binding:      provider=%s host=%s session=%s daemon=%s\n", report.Provider, report.HostID, report.SessionID, report.DaemonID)
	}
	if report.ReturnAddress != nil {
		fmt.Printf("Return to:    %s (daemon %s)\n", report.ReturnAddress.URL, report.ReturnAddress.DaemonID)
	}
	if report.ServerURL != "" {
		fmt.Printf("Server:       %s\n", report.ServerURL)
	}
	if report.AppURL != "" {
		fmt.Printf("App:          %s\n", report.AppURL)
	}
	if report.WorkspaceID != "" {
		fmt.Printf("Workspace:    %s\n", report.WorkspaceID)
	}
	if report.TokenID != "" {
		fmt.Printf("Token ID:     %s\n", report.TokenID)
	}
	if report.ExpiresAt != "" {
		fmt.Printf("Expires:      %s\n", report.ExpiresAt)
	}
	if report.Remote != nil && report.Remote.Checked {
		fmt.Printf("Remote:       checked (agent %s, %s)\n", report.Remote.AgentName, report.Remote.ExecutionKind)
	}
	if report.Error != "" {
		fmt.Printf("Problem:      %s\n", strings.TrimSpace(report.Error))
	}
}

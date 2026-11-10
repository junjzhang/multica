package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/daemon"
)

var agentExternalCmd = &cobra.Command{
	Use:   "external",
	Short: "Register and inspect external agent sessions",
}

var agentExternalRegisterCmd = &cobra.Command{
	Use:   "register",
	Short: "Register an existing external session as an agent identity",
	RunE:  runAgentExternalRegister,
}

var agentSessionBindCmd = &cobra.Command{
	Use:   "bind <agent-id>",
	Short: "Bind an explicit issue to one of this agent's host sessions",
	Args:  exactArgs(1),
	RunE:  runAgentSessionBind,
}

var agentTokenCmd = &cobra.Command{
	Use:   "token",
	Short: "Provision and revoke external agent credentials",
}

var agentTokenCreateCmd = &cobra.Command{
	Use:   "create <agent-id>",
	Short: "Create a one-time external agent token (secret is emitted only to an explicit sink)",
	Args:  exactArgs(1),
	RunE:  runAgentTokenCreate,
}

var agentTokenRevokeCmd = &cobra.Command{
	Use:   "revoke <agent-id> <token-id>",
	Short: "Revoke an external agent token",
	Args:  exactArgs(2),
	RunE:  runAgentTokenRevoke,
}

var agentMailboxCmd = &cobra.Command{
	Use:   "mailbox",
	Short: "Read and acknowledge this agent's mailbox",
}

var agentMailboxListCmd = &cobra.Command{
	Use:   "list <agent-id>",
	Short: "List messages addressed to an agent",
	Args:  exactArgs(1),
	RunE:  runAgentMailboxList,
}

var agentMailboxAckCmd = &cobra.Command{
	Use:   "ack <agent-id> <message-id>",
	Short: "Advance a mailbox message to read or handled",
	Args:  exactArgs(2),
	RunE:  runAgentMailboxAck,
}

func registerAgentExternalCommandTree() {
	agentCmd.AddCommand(agentExternalCmd)
	agentCmd.AddCommand(agentTokenCmd)
	agentCmd.AddCommand(agentMailboxCmd)
	agentExternalCmd.AddCommand(agentExternalRegisterCmd)
	agentExternalCmd.AddCommand(agentSessionBindCmd)
	agentTokenCmd.AddCommand(agentTokenCreateCmd)
	agentTokenCmd.AddCommand(agentTokenRevokeCmd)
	agentMailboxCmd.AddCommand(agentMailboxListCmd)
	agentMailboxCmd.AddCommand(agentMailboxAckCmd)

	registerAgentExternalRegisterFlags(agentExternalRegisterCmd)
	registerAgentSessionBindFlags(agentSessionBindCmd)
	registerAgentTokenCreateFlags(agentTokenCreateCmd)
	registerAgentTokenRevokeFlags(agentTokenRevokeCmd)
	registerAgentMailboxListFlags(agentMailboxListCmd)
	registerAgentMailboxAckFlags(agentMailboxAckCmd)
}

func registerAgentExternalRegisterFlags(cmd *cobra.Command) {
	cmd.Flags().String("name", "", "Agent display name (required unless --agent-id is set)")
	cmd.Flags().String("agent-id", "", "Attach this session to an existing external agent instead of creating one")
	cmd.Flags().String("provider", "paseo", "External host provider (currently paseo)")
	cmd.Flags().String("host-id", "", "Host server/connection id (not a hostname; required)")
	cmd.Flags().String("session-id", "", "Exact existing host session id (for Paseo, the Paseo agent id; required)")
	cmd.Flags().String("display-name", "", "Session display name; defaults to --name")
	cmd.Flags().String("url", "", "Optional deep link to the existing session")
	cmd.Flags().String("daemon-id", "", "Authorized Multica daemon that delivers to this session (required)")
	cmd.Flags().String("permission-mode", "", "Invocation permission mode: private or public_to")
	cmd.Flags().String("output", "json", "Output format: json or table")
}

func registerAgentSessionBindFlags(cmd *cobra.Command) {
	cmd.Flags().String("issue", "", "Issue id this session should serve (required)")
	cmd.Flags().String("session", "", "Registered external session id (required)")
	cmd.Flags().String("output", "json", "Output format: json or table")
}

func registerAgentTokenCreateFlags(cmd *cobra.Command) {
	cmd.Flags().String("expires-at", "", "Expiry as RFC3339 (required)")
	cmd.Flags().String("external-session", "", "Registered external session id to bind this token to (required when the agent has several sessions)")
	cmd.Flags().String("save-to", "", "Write the token JSON to this file with mode 0600 (required unless --save-profile or --print-token)")
	cmd.Flags().String("save-profile", "", "Store the token as this CLI profile's credential (required unless --save-to or --print-token)")
	cmd.Flags().Bool("print-token", false, "Print the raw token to stdout; prefer --save-to or --save-profile so the secret does not enter terminal or CI logs")
	cmd.Flags().Bool("force", false, "Overwrite --save-to if the file already exists")
	cmd.Flags().String("output", "json", "Output format for non-secret metadata: json or table")
}

func registerAgentTokenRevokeFlags(cmd *cobra.Command) {
	cmd.Flags().String("output", "json", "Output format: json or table")
}

func registerAgentMailboxListFlags(cmd *cobra.Command) {
	cmd.Flags().String("kind", "", "Message kind: comment, delegation_result, or background_wait (external only)")
	cmd.Flags().String("after", "", "Opaque cursor from the previous page")
	cmd.Flags().Int("limit", 50, "Maximum messages to return (1-100)")
	cmd.Flags().String("output", "table", "Output format: table or json")
}

func registerAgentMailboxAckFlags(cmd *cobra.Command) {
	cmd.Flags().String("state", "", "New processing state: read or handled (required)")
	cmd.Flags().String("output", "json", "Output format: json or table")
}

type externalAgentTokenResponse struct {
	ID                string `json:"id"`
	Token             string `json:"token"`
	ExpiresAt         string `json:"expires_at"`
	ExternalSessionID string `json:"external_session_id"`
}

func runAgentExternalRegister(cmd *cobra.Command, _ []string) error {
	name := strings.TrimSpace(flagString(cmd, "name"))
	agentID := strings.TrimSpace(flagString(cmd, "agent-id"))
	if name == "" && agentID == "" {
		return fmt.Errorf("--name is required unless --agent-id is set")
	}
	provider := strings.TrimSpace(flagString(cmd, "provider"))
	if provider == "" {
		provider = "paseo"
	}
	if provider != "paseo" {
		return fmt.Errorf("--provider must be paseo")
	}
	hostID := strings.TrimSpace(flagString(cmd, "host-id"))
	sessionID := strings.TrimSpace(flagString(cmd, "session-id"))
	daemonID := strings.TrimSpace(flagString(cmd, "daemon-id"))
	if hostID == "" || sessionID == "" || daemonID == "" {
		return fmt.Errorf("--host-id, --session-id and --daemon-id are required")
	}
	for flag, value := range map[string]string{"host-id": hostID, "session-id": sessionID, "daemon-id": daemonID} {
		if !externalSessionIDCLIPattern.MatchString(value) {
			return fmt.Errorf("--%s is invalid: use 1-200 letters, digits, '.', '_', ':' or '-'", flag)
		}
	}
	displayName := strings.TrimSpace(flagString(cmd, "display-name"))
	if displayName == "" {
		displayName = name
	}
	rawURL := strings.TrimSpace(flagString(cmd, "url"))
	if rawURL != "" {
		if err := validateExternalSessionURL(rawURL, provider, hostID, sessionID); err != nil {
			return err
		}
	}
	permissionMode := strings.TrimSpace(flagString(cmd, "permission-mode"))
	if permissionMode != "" && permissionMode != "private" && permissionMode != "public_to" {
		return fmt.Errorf("--permission-mode must be private or public_to")
	}

	session := map[string]any{
		"provider":     provider,
		"host_id":      hostID,
		"session_id":   sessionID,
		"display_name": displayName,
		"url":          nil,
		"daemon_id":    daemonID,
	}
	if rawURL != "" {
		session["url"] = rawURL
	}
	body := map[string]any{
		"name":             name,
		"external_session": session,
	}
	if agentID != "" {
		body["agent_id"] = agentID
	}
	if permissionMode != "" {
		body["permission_mode"] = permissionMode
	}

	client, err := newOwnerAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	var result ExternalAgent
	if err := client.PostJSON(ctx, "/api/agents/external", body, &result); err != nil {
		return fmt.Errorf("register external session: %w", err)
	}

	output := flagString(cmd, "output")
	if output == "table" {
		rows := [][]string{{result.ID, result.Name, result.ExecutionKind, externalSessionCell(result.ExternalSession)}}
		cli.PrintTable(os.Stdout, []string{"ID", "NAME", "KIND", "SESSION"}, rows)
		return nil
	}
	return cli.PrintJSON(os.Stdout, result)
}

type agentSessionBindingCLI struct {
	ID        string `json:"id"`
	AgentID   string `json:"agent_id"`
	IssueID   string `json:"issue_id"`
	SessionID string `json:"session_id"`
}

func runAgentSessionBind(cmd *cobra.Command, args []string) error {
	issueID := strings.TrimSpace(flagString(cmd, "issue"))
	sessionID := strings.TrimSpace(flagString(cmd, "session"))
	if issueID == "" || sessionID == "" {
		return fmt.Errorf("--issue and --session are required")
	}
	client, err := newOwnerAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	var binding agentSessionBindingCLI
	path := "/api/agents/" + url.PathEscape(args[0]) + "/session-bindings"
	if err := client.PostJSON(ctx, path, map[string]any{"issue_id": issueID, "session_id": sessionID}, &binding); err != nil {
		return fmt.Errorf("bind external session: %w", err)
	}
	if flagString(cmd, "output") == "table" {
		cli.PrintTable(os.Stdout, []string{"BINDING", "AGENT", "ISSUE", "SESSION"}, [][]string{{binding.ID, binding.AgentID, binding.IssueID, binding.SessionID}})
		return nil
	}
	return cli.PrintJSON(os.Stdout, binding)
}

// ExternalAgent mirrors the additive Agent fields the external endpoint
// returns. It is intentionally local to the CLI: the daemon does not consume
// agent responses.
type ExternalAgent struct {
	ID              string                  `json:"id"`
	Name            string                  `json:"name"`
	ExecutionKind   string                  `json:"execution_kind"`
	ExternalSession *daemon.ExternalSession `json:"external_session"`
}

func externalSessionCell(session *daemon.ExternalSession) string {
	if session == nil {
		return ""
	}
	if session.DisplayName != "" {
		return session.Provider + ":" + session.DisplayName
	}
	return session.Provider + ":" + session.SessionID
}

func runAgentTokenCreate(cmd *cobra.Command, args []string) error {
	expiresRaw := strings.TrimSpace(flagString(cmd, "expires-at"))
	if expiresRaw == "" {
		return fmt.Errorf("--expires-at is required")
	}
	expiresAt, err := time.Parse(time.RFC3339, expiresRaw)
	if err != nil {
		return fmt.Errorf("--expires-at must be RFC3339: %w", err)
	}
	now := time.Now()
	if !expiresAt.After(now.Add(time.Minute)) {
		return fmt.Errorf("--expires-at must be more than one minute in the future")
	}
	if expiresAt.After(now.Add(366 * 24 * time.Hour)) {
		return fmt.Errorf("--expires-at is too far in the future (maximum 366 days)")
	}
	saveTo := strings.TrimSpace(flagString(cmd, "save-to"))
	saveProfile := strings.TrimSpace(flagString(cmd, "save-profile"))
	printToken := flagBool(cmd, "print-token")
	sinks := 0
	for _, set := range []bool{saveTo != "", saveProfile != "", printToken} {
		if set {
			sinks++
		}
	}
	if sinks == 0 {
		return fmt.Errorf("choose an explicit secret sink: --save-to <path> (recommended), --save-profile <name>, or --print-token")
	}
	if sinks > 1 {
		return fmt.Errorf("--save-to, --save-profile and --print-token are mutually exclusive")
	}

	client, err := newOwnerAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	var issued externalAgentTokenResponse
	path := "/api/agents/" + url.PathEscape(args[0]) + "/tokens"
	body := map[string]any{"expires_at": expiresAt.UTC().Format(time.RFC3339)}
	externalSessionID := strings.TrimSpace(flagString(cmd, "external-session"))
	if externalSessionID != "" {
		body["external_session_id"] = externalSessionID
	}
	if err := client.PostJSON(ctx, path, body, &issued); err != nil {
		return fmt.Errorf("create agent token: %w", err)
	}
	if issued.Token == "" {
		return fmt.Errorf("create agent token: server response did not include a token")
	}
	if externalSessionID != "" && issued.ExternalSessionID != "" && issued.ExternalSessionID != externalSessionID {
		return fmt.Errorf("create agent token: server bound the credential to session %s, not %s", issued.ExternalSessionID, externalSessionID)
	}

	if printToken {
		fmt.Fprintln(os.Stderr, "Printing a secret to stdout; prefer --save-to or --save-profile so it does not enter terminal or CI logs.")
		fmt.Fprintln(os.Stdout, issued.Token)
		return nil
	}

	if saveProfile != "" {
		if saveProfile == "." || saveProfile == ".." || filepath.IsAbs(saveProfile) || strings.ContainsAny(saveProfile, `/\\`) {
			return fmt.Errorf("--save-profile must be a plain profile name")
		}
		cfg, err := cli.LoadCLIConfigForProfile(saveProfile)
		if err != nil {
			return fmt.Errorf("token %s was issued but the profile could not be loaded: %w; revoke it with `multica agent token revoke %s %s` if you cannot recover it", issued.ID, err, args[0], issued.ID)
		}
		if cfg.Token != "" && !flagBool(cmd, "force") {
			return fmt.Errorf("profile %q already has a token; pass --force to replace it (token %s was issued and not saved)", saveProfile, issued.ID)
		}
		cfg.Token = issued.Token
		cfg.ServerURL = resolveServerURL(cmd)
		cfg.WorkspaceID = resolveWorkspaceID(cmd)
		if err := cli.SaveCLIConfigForProfile(cfg, saveProfile); err != nil {
			return fmt.Errorf("token %s was issued but the profile could not be saved: %w; revoke it with `multica agent token revoke %s %s` if you cannot recover it", issued.ID, err, args[0], issued.ID)
		}
		metadata := map[string]any{
			"id":               issued.ID,
			"expires_at":       issued.ExpiresAt,
			"saved_to_profile": saveProfile,
		}
		if issued.ExternalSessionID != "" {
			metadata["external_session_id"] = issued.ExternalSessionID
		}
		if flagString(cmd, "output") == "table" {
			fmt.Printf("Token %s saved to profile %s (expires %s)\n", issued.ID, saveProfile, issued.ExpiresAt)
			return nil
		}
		return cli.PrintJSON(os.Stdout, metadata)
	}

	payload := map[string]any{
		"agent_id":     args[0],
		"id":           issued.ID,
		"token":        issued.Token,
		"expires_at":   issued.ExpiresAt,
		"server_url":   resolveServerURL(cmd),
		"workspace_id": resolveWorkspaceID(cmd),
	}
	if issued.ExternalSessionID != "" {
		payload["external_session_id"] = issued.ExternalSessionID
	}
	if err := writeSecretFile(saveTo, payload, flagBool(cmd, "force")); err != nil {
		return fmt.Errorf("token %s was issued but could not be saved: %w; revoke it with `multica agent token revoke %s %s` if you cannot recover it", issued.ID, err, args[0], issued.ID)
	}

	metadata := map[string]any{
		"id":         issued.ID,
		"expires_at": issued.ExpiresAt,
		"saved_to":   saveTo,
	}
	if issued.ExternalSessionID != "" {
		metadata["external_session_id"] = issued.ExternalSessionID
	}
	if flagString(cmd, "output") == "table" {
		fmt.Printf("Token %s saved to %s (expires %s)\n", issued.ID, saveTo, issued.ExpiresAt)
		return nil
	}
	return cli.PrintJSON(os.Stdout, metadata)
}

func runAgentTokenRevoke(cmd *cobra.Command, args []string) error {
	client, err := newOwnerAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	path := "/api/agents/" + url.PathEscape(args[0]) + "/tokens/" + url.PathEscape(args[1])
	if err := client.DeleteJSON(ctx, path); err != nil {
		return fmt.Errorf("revoke agent token: %w", err)
	}
	result := map[string]any{"agent_id": args[0], "token_id": args[1], "revoked": true}
	if flagString(cmd, "output") == "table" {
		fmt.Printf("Token %s revoked.\n", args[1])
		return nil
	}
	return cli.PrintJSON(os.Stdout, result)
}

func runAgentMailboxList(cmd *cobra.Command, args []string) error {
	limit := flagInt(cmd, "limit")
	if limit < 1 || limit > 100 {
		return fmt.Errorf("--limit must be between 1 and 100")
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	params := url.Values{}
	params.Set("limit", fmt.Sprint(limit))
	if kind := flagString(cmd, "kind"); kind != "" {
		params.Set("kind", kind)
	}
	if after := strings.TrimSpace(flagString(cmd, "after")); after != "" {
		params.Set("after", after)
	}
	path := "/api/agents/" + url.PathEscape(args[0]) + "/mailbox?" + params.Encode()
	var result struct {
		Messages   []daemon.MailboxMessage `json:"messages"`
		NextCursor *string                 `json:"next_cursor"`
	}
	if err := client.GetJSON(ctx, path, &result); err != nil {
		return fmt.Errorf("list agent mailbox: %w", err)
	}
	if result.Messages == nil {
		result.Messages = []daemon.MailboxMessage{}
	}

	if flagString(cmd, "output") == "json" {
		return cli.PrintJSON(os.Stdout, result)
	}
	rows := make([][]string, 0, len(result.Messages))
	for _, message := range result.Messages {
		lastError := ""
		if message.LastError != nil {
			lastError = *message.LastError
		}
		rows = append(rows, []string{
			message.ID,
			message.DeliveryStatus,
			message.ProcessingStatus,
			message.IssueID,
			message.CreatedAt,
			lastError,
		})
	}
	cli.PrintTable(os.Stdout, []string{"ID", "DELIVERY", "PROCESSING", "ISSUE", "CREATED", "LAST ERROR"}, rows)
	if result.NextCursor != nil && *result.NextCursor != "" {
		fmt.Fprintf(os.Stderr, "Next cursor: %s\n", *result.NextCursor)
	}
	return nil
}

func runAgentMailboxAck(cmd *cobra.Command, args []string) error {
	state := strings.TrimSpace(flagString(cmd, "state"))
	if state != "read" && state != "handled" {
		return fmt.Errorf("--state must be read or handled")
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	path := "/api/agents/" + url.PathEscape(args[0]) + "/mailbox/" + url.PathEscape(args[1]) + "/ack"
	var message daemon.MailboxMessage
	if err := client.PostJSON(ctx, path, map[string]any{"state": state}, &message); err != nil {
		return fmt.Errorf("ack agent mailbox message: %w", err)
	}
	if flagString(cmd, "output") == "table" {
		fmt.Printf("Message %s marked %s.\n", message.ID, message.ProcessingStatus)
		return nil
	}
	return cli.PrintJSON(os.Stdout, message)
}

var externalSessionIDCLIPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,199}$`)

func validateExternalSessionURL(raw, provider, hostID, sessionID string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("--url is not a valid URL: %w", err)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("--url must not contain credentials, a query, or a fragment")
	}
	if provider != "paseo" {
		return fmt.Errorf("--provider must be paseo")
	}
	if parsed.Scheme != "paseo" || parsed.Host != "h" {
		return fmt.Errorf("paseo --url must have the form paseo://h/<host-id>/agent/<session-id>")
	}
	segments := strings.Split(strings.TrimPrefix(parsed.EscapedPath(), "/"), "/")
	if len(segments) != 3 || segments[1] != "agent" {
		return fmt.Errorf("paseo --url must have exactly /<host-id>/agent/<session-id> after paseo://h")
	}
	decodedHost, err := url.PathUnescape(segments[0])
	if err != nil || decodedHost != hostID {
		return fmt.Errorf("paseo --url host segment must match --host-id")
	}
	decodedSession, err := url.PathUnescape(segments[2])
	if err != nil || decodedSession != sessionID {
		return fmt.Errorf("paseo --url session segment must match --session-id")
	}
	return nil
}

// writeSecretFile is the only path that persists a raw external token. It
// refuses to overwrite by default, restricts the file to the owner, and never
// writes the secret through stdout.
func writeSecretFile(path string, payload map[string]any, force bool) error {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("encode token file: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create token file directory: %w", err)
	}
	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists; pass --force to overwrite it", path)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("check token file %s: %w", path, err)
		}
	}
	tmp, err := os.CreateTemp(dir, ".multica-token-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create token file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("restrict token file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write token file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close token file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("save token file: %w", err)
	}
	return nil
}

func flagBool(cmd *cobra.Command, name string) bool {
	value, _ := cmd.Flags().GetBool(name)
	return value
}

func flagInt(cmd *cobra.Command, name string) int {
	value, _ := cmd.Flags().GetInt(name)
	return value
}

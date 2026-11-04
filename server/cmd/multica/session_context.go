package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/multica-ai/multica/server/internal/cli"
)

// Multica external session binding, shared file contract v1.
//
// A Paseo provider launch exports PASEO_AGENT_ID. The Paseo plugin writes a
// private binding file for that agent; this CLI reads it so ordinary commands
// run as the external agent instead of the human profile that owns the daemon.
// The file is a credential cache, not a queue or task authority: authority
// still comes from the mea_ token and the server's own owner/workspace checks.
//
// The plugin is the only writer. The CLI never rewrites the file, never copies
// its token anywhere else, and never prints it.
const (
	externalSessionFileEnv = "MULTICA_EXTERNAL_SESSION_FILE"
	paseoAgentIDEnv        = "PASEO_AGENT_ID"

	externalSessionVersion     = 1
	externalSessionProvider    = "paseo"
	externalSessionTokenPrefix = "mea_"
	externalSessionConnectHint = "run `multica session connect` in the Paseo session (or `/multica connect` in the composer)"
)

var (
	sessionSecretPattern = regexp.MustCompile(`\b(?:mea|mat|mul)_[A-Za-z0-9._-]{8,}\b`)
	// errExternalSessionFileMissing lets session status report `disconnected`
	// for a default path that was never written (or was removed by
	// `/multica disconnect`) instead of calling that file invalid. Commands
	// still fail closed on it.
	errExternalSessionFileMissing = errors.New("no Multica binding for this Paseo session")
	// errExternalSessionActorMismatch is the server-confirmed case where the
	// credential authenticates a different agent than the binding declares.
	// Visibility of that agent says nothing about who the token belongs to.
	errExternalSessionActorMismatch = errors.New("the session credential does not belong to the agent named in the binding")
)

func safeSessionDiagnostic(message string) string {
	message = strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, message)
	return sessionSecretPattern.ReplaceAllString(message, "[redacted]")
}

// verifyExternalSessionActor proves the credential's own identity before a
// business command runs. GET /api/agents/{id}/mailbox is the existing
// self-identity endpoint: Auth stamps X-Agent-ID from the credential row, and
// the handler answers 403 unless the path id is that same agent. A visibility
// GET of the agent could succeed for a token that belongs to someone else, so
// it is not evidence for this decision.
func verifyExternalSessionActor(ctx context.Context, client *cli.APIClient, agentID string) error {
	path := "/api/agents/" + url.PathEscape(agentID) + "/mailbox?limit=1"
	var page struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := client.GetJSON(ctx, path, &page); err != nil {
		var httpErr *cli.HTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusForbidden {
			return fmt.Errorf("%w: %s", errExternalSessionActorMismatch, agentID)
		}
		return fmt.Errorf("confirm the credential's own agent: %w", err)
	}
	return nil
}

// externalSessionContext mirrors the v1 file. Path and Explicit are where this
// process found the binding, not file content, so they stay unexported: no
// marshalling path can leak the token.
type externalSessionContext struct {
	Version     int    `json:"version"`
	Provider    string `json:"provider"`
	HostID      string `json:"host_id"`
	SessionID   string `json:"session_id"`
	AgentID     string `json:"agent_id"`
	DaemonID    string `json:"daemon_id"`
	ServerURL   string `json:"server_url"`
	AppURL      string `json:"app_url"`
	WorkspaceID string `json:"workspace_id"`
	Token       string `json:"token"`
	TokenID     string `json:"token_id"`
	ExpiresAt   string `json:"expires_at"`

	path     string
	explicit bool
}

// externalSessionFilePath selects the binding file. The explicit env var wins;
// otherwise PASEO_AGENT_ID names the default file, which is what lets an
// already-running Paseo session pick up a binding written after it started
// (no provider reload). present=false means this process is not in an external
// session at all.
func externalSessionFilePath() (path string, explicit, present bool, err error) {
	if raw := strings.TrimSpace(os.Getenv(externalSessionFileEnv)); raw != "" {
		if !filepath.IsAbs(raw) {
			return "", false, false, fmt.Errorf("%s must be an absolute path, got %q", externalSessionFileEnv, raw)
		}
		return filepath.Clean(raw), true, true, nil
	}
	agentID := strings.TrimSpace(os.Getenv(paseoAgentIDEnv))
	if agentID == "" {
		return "", false, false, nil
	}
	// The id becomes a path segment, so it has to be a plain locator.
	if !externalSessionIDCLIPattern.MatchString(agentID) {
		return "", false, false, fmt.Errorf("%s %q is not a usable Paseo agent id", paseoAgentIDEnv, agentID)
	}
	path, err = defaultExternalSessionFilePath(agentID)
	return path, false, true, err
}

func defaultExternalSessionFilePath(agentID string) (string, error) {
	dir, err := cli.ProfileDir("")
	if err != nil {
		return "", fmt.Errorf("resolve external session directory: %w", err)
	}
	return filepath.Join(dir, "external-sessions", externalSessionProvider, agentID+".json"), nil
}

// readExternalSessionContext loads and validates the binding for this process.
// It returns (nil, nil) when the process is not in an external session.
func readExternalSessionContext() (*externalSessionContext, error) {
	path, explicit, present, err := externalSessionFilePath()
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w (expected %s); %s", errExternalSessionFileMissing, path, externalSessionConnectHint)
		}
		return nil, fmt.Errorf("read external session context %s: %w", path, err)
	}
	if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("external session context %s is readable by other users (mode %04o); restrict it with `chmod 600 %s`", path, info.Mode().Perm(), path)
	}
	var ctx externalSessionContext
	if err := json.Unmarshal(data, &ctx); err != nil {
		return nil, fmt.Errorf("external session context %s is not valid JSON: %w; %s", path, err, externalSessionConnectHint)
	}
	ctx.path = path
	ctx.explicit = explicit
	if err := ctx.validate(); err != nil {
		return nil, fmt.Errorf("external session context %s is invalid: %w; %s", path, err, externalSessionConnectHint)
	}
	// Normalize once so an explicit --server-url / MULTICA_SERVER_URL that
	// names the same endpoint in another spelling is not a false conflict.
	ctx.ServerURL = normalizeAPIBaseURL(ctx.ServerURL)
	return &ctx, nil
}

// validate checks the v1 contract. Every failure names the field so a user can
// tell a stale credential from a damaged file.
func (c *externalSessionContext) validate() error {
	if c.Version != externalSessionVersion {
		return fmt.Errorf("version is %d, want %d", c.Version, externalSessionVersion)
	}
	if c.Provider != externalSessionProvider {
		return fmt.Errorf("provider is %q, want %q", c.Provider, externalSessionProvider)
	}
	for _, field := range []struct{ name, value string }{
		{"host_id", c.HostID},
		{"session_id", c.SessionID},
		{"agent_id", c.AgentID},
		{"daemon_id", c.DaemonID},
		{"server_url", c.ServerURL},
		{"workspace_id", c.WorkspaceID},
		{"token", c.Token},
		{"expires_at", c.ExpiresAt},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s is missing", field.name)
		}
	}
	for _, field := range []struct{ name, value string }{
		{"host_id", c.HostID},
		{"session_id", c.SessionID},
		{"agent_id", c.AgentID},
		{"daemon_id", c.DaemonID},
		{"workspace_id", c.WorkspaceID},
	} {
		if !externalSessionIDCLIPattern.MatchString(field.value) {
			return fmt.Errorf("%s %q is not a valid identifier", field.name, field.value)
		}
	}
	// A session id that disagrees with the environment means the file belongs
	// to another Paseo agent; using it would attribute work to the wrong actor.
	if agentID := strings.TrimSpace(os.Getenv(paseoAgentIDEnv)); agentID != "" && agentID != c.SessionID {
		return fmt.Errorf("session_id %q does not match %s %q", c.SessionID, paseoAgentIDEnv, agentID)
	}
	serverURL, err := url.Parse(c.ServerURL)
	if err != nil || (serverURL.Scheme != "http" && serverURL.Scheme != "https") || serverURL.Host == "" || serverURL.User != nil {
		return fmt.Errorf("server_url %q is not an http(s) base URL", c.ServerURL)
	}
	if strings.TrimSpace(c.Token) != c.Token {
		return fmt.Errorf("token has surrounding whitespace")
	}
	if !strings.HasPrefix(c.Token, externalSessionTokenPrefix) {
		return fmt.Errorf("token is not an external agent credential (%s...)", externalSessionTokenPrefix)
	}
	expiresAt, err := time.Parse(time.RFC3339, c.ExpiresAt)
	if err != nil {
		return fmt.Errorf("expires_at %q is not RFC3339", c.ExpiresAt)
	}
	if !time.Now().Before(expiresAt) {
		return fmt.Errorf("the credential expired at %s; reconnect the session to issue a new one", expiresAt.UTC().Format(time.RFC3339))
	}
	return nil
}

// sessionReturnURL is the existing conversation's deep link. The v1 file
// carries the host and session ids, which are exactly the two path segments
// the Paseo link needs.
func (c *externalSessionContext) sessionReturnURL() string {
	return "paseo://h/" + url.PathEscape(c.HostID) + "/agent/" + url.PathEscape(c.SessionID)
}

// externalSessionForCommand returns the active binding, or nil when ordinary
// resolution applies. Precedence is deliberate:
//
//   - A daemon-managed task keeps its task-scoped mat_ identity. An incidental
//     inherited PASEO_AGENT_ID is ignored there, because a managed run must not
//     be rebound by the host environment it happens to inherit.
//   - An explicit MULTICA_EXTERNAL_SESSION_FILE inside a managed task is a
//     contradiction, not a preference, so it is refused instead of resolved.
//   - Inside a Paseo session the binding is required. A missing or broken file
//     fails closed; it never falls through to the human's default profile.
func externalSessionForCommand() (*externalSessionContext, error) {
	if inDaemonManagedExecutionContext() {
		if raw := strings.TrimSpace(os.Getenv(externalSessionFileEnv)); raw != "" {
			return nil, fmt.Errorf("%s is set inside a daemon-managed task; the task's own credential is authoritative and cannot be replaced by an external session", externalSessionFileEnv)
		}
		return nil, nil
	}
	return readExternalSessionContext()
}

// sessionContextConflicts rejects explicit inputs that disagree with the
// binding instead of silently preferring one of them.
func sessionContextConflicts(cmd *cobra.Command, ctx *externalSessionContext) error {
	if inheritedFlagChanged(cmd, "return-session") && strings.TrimSpace(flagValue(cmd, "return-session")) != ctx.SessionID {
		return fmt.Errorf("--return-session conflicts with the Paseo session binding's return session")
	}
	if value := strings.TrimSpace(os.Getenv("MULTICA_RETURN_SESSION")); value != "" && value != ctx.SessionID {
		return fmt.Errorf("MULTICA_RETURN_SESSION conflicts with the Paseo session binding's return session")
	}
	if inheritedFlagChanged(cmd, "profile") {
		return fmt.Errorf("--profile cannot be combined with a Paseo session binding; the binding already selects its credential, server and workspace")
	}
	if v := strings.TrimSpace(os.Getenv("MULTICA_TOKEN")); v != "" {
		return fmt.Errorf("MULTICA_TOKEN is set while a Paseo session binding is active; unset it so the session's own credential is used")
	}
	if inheritedFlagChanged(cmd, "server-url") {
		if v := normalizeAPIBaseURL(flagValue(cmd, "server-url")); v != ctx.ServerURL {
			return fmt.Errorf("--server-url %s conflicts with the session's server %s", v, ctx.ServerURL)
		}
	}
	if v := strings.TrimSpace(os.Getenv("MULTICA_SERVER_URL")); v != "" && normalizeAPIBaseURL(v) != ctx.ServerURL {
		return fmt.Errorf("MULTICA_SERVER_URL %s conflicts with the session's server %s", normalizeAPIBaseURL(v), ctx.ServerURL)
	}
	if inheritedFlagChanged(cmd, "workspace-id") {
		if v := flagValue(cmd, "workspace-id"); v != ctx.WorkspaceID {
			return fmt.Errorf("--workspace-id %s conflicts with the session's workspace %s", v, ctx.WorkspaceID)
		}
	}
	if v := strings.TrimSpace(os.Getenv("MULTICA_WORKSPACE_ID")); v != "" && v != ctx.WorkspaceID {
		return fmt.Errorf("MULTICA_WORKSPACE_ID %s conflicts with the session's workspace %s", v, ctx.WorkspaceID)
	}
	return nil
}

// inheritedFlagChanged reports a value set through a persistent flag on this
// command or any parent, which is how --profile / --server-url / --workspace-id
// reach subcommands.
func inheritedFlagChanged(cmd *cobra.Command, name string) bool {
	for c := cmd; c != nil; c = c.Parent() {
		for _, flags := range []*pflag.FlagSet{c.Flags(), c.PersistentFlags()} {
			if flag := flags.Lookup(name); flag != nil && flag.Changed {
				return true
			}
		}
	}
	return false
}

// flagValue reads an inherited flag without assuming it is local to cmd.
func flagValue(cmd *cobra.Command, name string) string {
	for c := cmd; c != nil; c = c.Parent() {
		for _, flags := range []*pflag.FlagSet{c.Flags(), c.PersistentFlags()} {
			if flag := flags.Lookup(name); flag != nil {
				return strings.TrimSpace(flag.Value.String())
			}
		}
	}
	return ""
}

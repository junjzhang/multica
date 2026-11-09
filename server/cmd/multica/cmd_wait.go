package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/backgroundwait"
	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"github.com/spf13/cobra"
)

func init() {
	wait := &cobra.Command{Use: "wait", Short: "Run a bounded local command in the daemon and notify this Paseo session", GroupID: groupRuntime}
	wait.PersistentFlags().String("daemon-profile", "", "Local daemon profile (does not change the session's API identity)")
	start := &cobra.Command{Use: "start -- <command> [args...]", Short: "Register once and return immediately; keep the returned ID for retries", Args: cobra.MinimumNArgs(1), RunE: runWaitStart}
	start.Flags().Duration("timeout", time.Hour, "Maximum execution duration (up to 168h)")
	start.Flags().String("request-id", "", "Stable UUID for registration retries")
	start.Flags().String("deadline", "", "Fixed RFC3339 deadline for identical registration retries")
	start.Flags().String("cwd", "", "Command directory (defaults to current directory)")
	get := &cobra.Command{Use: "get <id>", Short: "Read the local durable status and Mailbox delivery ID", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return callWait(cmd, http.MethodGet, "/waits/"+url.PathEscape(args[0]), nil, nil)
	}}
	cancel := &cobra.Command{Use: "cancel <id>", Short: "Stop the local wait process; the remote CI continues", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return callWait(cmd, http.MethodDelete, "/waits/"+url.PathEscape(args[0]), nil, nil)
	}}
	wait.AddCommand(start, get, cancel)
	rootCmd.AddCommand(wait)
}

func runWaitStart(cmd *cobra.Command, args []string) error {
	// Resolve the normal command identity first. Managed task credentials must
	// never fall through to the owner's external context or daemon profile.
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(client.Token, "mea_") {
		return fmt.Errorf("wait start requires a connected external Paseo session; managed task credentials are not supported")
	}
	bound, err := readExternalSessionContext()
	if err != nil {
		return err
	}
	if bound == nil {
		return fmt.Errorf("connect the current Paseo session first")
	}
	timeout, _ := cmd.Flags().GetDuration("timeout")
	if timeout <= 0 || timeout > backgroundwait.MaxDuration {
		return fmt.Errorf("timeout must be positive and at most 168h")
	}
	deadline := time.Now().UTC().Add(timeout)
	if raw := flagString(cmd, "deadline"); raw != "" {
		deadline, err = time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return fmt.Errorf("invalid deadline")
		}
	}
	deadline = deadline.UTC()
	id := flagString(cmd, "request-id")
	if id == "" {
		id = uuid.NewString()
	}
	if _, err := uuid.Parse(id); err != nil {
		return fmt.Errorf("request-id must be a UUID")
	}
	cwd := flagString(cmd, "cwd")
	if cwd == "" {
		cwd, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return err
	}
	request := protocol.WaitStart{ID: id, WorkspaceID: bound.WorkspaceID, SessionRef: bound.SessionID, Argv: args, Cwd: cwd, Deadline: deadline}
	fmt.Fprintf(cmd.ErrOrStderr(), "Wait request %s; retry deadline %s\n", id, deadline.Format(time.RFC3339Nano))
	return callWait(cmd, http.MethodPost, "/waits", request, bound)
}

func callWait(cmd *cobra.Command, method, path string, body any, bound *externalSessionContext) error {
	dir, err := cli.ProfileDir(flagString(cmd, "daemon-profile"))
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(dir, "wait.endpoint.json"))
	if err != nil {
		return fmt.Errorf("local daemon wait endpoint unavailable; start an updated daemon for this profile")
	}
	var endpoint protocol.WaitEndpoint
	if json.Unmarshal(data, &endpoint) != nil || endpoint.Secret == "" {
		return fmt.Errorf("invalid local daemon wait endpoint")
	}
	parsed, err := url.Parse(endpoint.URL)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.User != nil || parsed.Path != "" {
		return fmt.Errorf("wait endpoint must be a loopback HTTP address")
	}
	if bound != nil && bound.DaemonID != endpoint.DaemonID {
		return fmt.Errorf("the selected local daemon does not own this session")
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(cmd.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, endpoint.URL+path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+endpoint.Secret)
	req.Header.Set("Content-Type", "application/json")
	if bound != nil {
		req.Header.Set("X-Wait-Actor-Token", bound.Token)
	}
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("local daemon wait request failed; retry with the same request-id and deadline")
	}
	defer response.Body.Close()
	result, err := io.ReadAll(io.LimitReader(response.Body, 32768))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("wait request: %s: %s", response.Status, strings.TrimSpace(string(result)))
	}
	var status protocol.WaitStatus
	if err := json.Unmarshal(result, &status); err != nil {
		return fmt.Errorf("invalid wait response")
	}
	return cli.PrintJSON(os.Stdout, status)
}

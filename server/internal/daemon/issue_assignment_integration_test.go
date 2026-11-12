package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/delivery"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type observedAssignmentClient struct {
	*Client
	results chan delivery.Delivery
}

func (c observedAssignmentClient) RecordDeliveryOutcome(ctx context.Context, workspaceID string, request delivery.OutcomeRequest) (delivery.Delivery, error) {
	result, err := c.Client.RecordDeliveryOutcome(ctx, workspaceID, request)
	if err == nil {
		c.results <- result
	}
	return result, err
}

// Exercises production HTTP auth/assignment/claim/outcome handlers, the daemon
// client and worker, and the real Paseo process adapter against an isolated
// executable. Neither enqueue nor host receipt is mocked.
func TestExternalSquadAssignmentHTTPToHostReceipt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("isolated POSIX host fixture")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	pool, err := pgxpool.New(t.Context(), os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	f := testutil.New(pool, "", "")
	f.UserID = f.User(t, "Isolated external owner", uuid.NewString()+"@assignment.test")
	f.WorkspaceID = f.Workspace(t, "Assignment integration", "assignment-"+uuid.NewString())
	f.Member(t, f.WorkspaceID, f.UserID, "owner")
	daemonID := "assignment-" + uuid.NewString()
	f.Runtime(t, "Isolated delivery worker", testutil.Cols{"daemon_id": daemonID})
	agentID := f.Agent(t, "External integration leader", "", testutil.Cols{"execution_kind": "external"})
	sessionID := f.Insert(t, "external_agent_session", testutil.Cols{
		"workspace_id": f.WorkspaceID, "agent_id": agentID, "owner_user_id": f.UserID,
		"provider": "paseo", "host_id": "isolated-host", "session_id": "chosen-host-session", "daemon_id": daemonID,
	})
	f.Insert(t, "external_agent_session", testutil.Cols{
		"workspace_id": f.WorkspaceID, "agent_id": agentID, "owner_user_id": f.UserID,
		"provider": "paseo", "host_id": "another-host", "session_id": "never-select-this-session", "daemon_id": "other-daemon",
	})
	defaultLeader := f.Agent(t, "Managed integration default", "")
	squadID := f.Squad(t, "Integration squad", defaultLeader, testutil.Cols{"instructions": "Integration squad instruction"})
	f.SquadMember(t, squadID, "agent", agentID)
	memberID := f.Agent(t, "Do not start member", "")
	f.SquadMember(t, squadID, "agent", memberID)
	q := db.New(pool)
	hub := realtime.NewHub()
	go hub.Run()
	h := handler.New(q, pool, hub, events.New(), nil, nil, nil, nil, handler.Config{})
	router := chi.NewRouter()
	router.Use(middleware.Auth(q, nil, nil, nil))
	router.Post("/api/issues", h.CreateIssue)
	router.Put("/api/issues/{id}", h.UpdateIssue)
	router.Post("/api/issues/{id}/comments", h.CreateComment)
	router.Post("/api/daemon/deliveries/claim", h.ClaimDaemonDeliveries)
	router.Post("/api/daemon/deliveries/{deliveryId}/outcome", h.RecordDaemonDeliveryOutcome)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": f.UserID, "email": "integration@assignment.test", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix()}).SignedString(auth.JWTSecret())
	if err != nil {
		t.Fatal(err)
	}
	externalToken, err := auth.GenerateExternalAgentToken()
	if err != nil {
		t.Fatal(err)
	}
	f.Insert(t, "external_agent_token", testutil.Cols{"workspace_id": f.WorkspaceID, "agent_id": agentID, "owner_user_id": f.UserID, "token_hash": auth.HashToken(externalToken), "external_session_id": sessionID, "expires_at": time.Now().Add(time.Hour)})
	requestToken := externalToken
	request := func(method, path string, body any, status int) map[string]any {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+requestToken)
		req.Header.Set("X-Workspace-ID", f.WorkspaceID)
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		contents, _ := io.ReadAll(response.Body)
		if response.StatusCode != status {
			t.Fatalf("%s %s = %d: %s", method, path, response.StatusCode, contents)
		}
		var result map[string]any
		if err := json.Unmarshal(contents, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	created := request(http.MethodPost, "/api/issues", map[string]any{"title": "External integration assignment", "assignee_type": "squad", "assignee_id": squadID, "recipient_session_id": sessionID}, http.StatusCreated)
	issueID := created["id"].(string)
	requestToken = token
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM agent_mailbox_message WHERE issue_id=$1`, issueID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM issue_delegation_return WHERE issue_id=$1`, issueID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM external_agent_session_binding WHERE issue_id=$1`, issueID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM issue WHERE id=$1`, issueID)
	})
	// Reassignment and @squad share the exact issue binding and squad context.
	request(http.MethodPut, "/api/issues/"+issueID, map[string]any{"assignee_type": "member", "assignee_id": f.UserID}, http.StatusOK)
	request(http.MethodPut, "/api/issues/"+issueID, map[string]any{"assignee_type": "squad", "assignee_id": squadID}, http.StatusOK)
	request(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": fmt.Sprintf("[@Integration squad](mention://squad/%s) evaluate", squadID)}, http.StatusCreated)

	prompt := acceptIsolatedAssignmentRequests(t, server.URL, token, f.WorkspaceID, daemonID, 3)
	for _, required := range []string{issueID, squadID, memberID, "Integration squad instruction", "--mailbox-id", "## Squad Operating Protocol"} {
		if !strings.Contains(prompt, required) {
			t.Errorf("host prompt missing %s", required)
		}
	}

	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, issueID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("external handoff fabricated %d tasks: %v", count, err)
	}
	t.Logf("HTTP create/reassign/@squad -> production Mailbox worker -> isolated Paseo process: 3 host receipts, exact session, shared context; no managed tasks")
}

func acceptIsolatedAssignmentRequests(t *testing.T, serverURL, token, workspaceID, daemonID string, expected int) string {
	t.Helper()
	dir := t.TempDir()
	capture := filepath.Join(dir, "received.md")
	t.Setenv("MULTICA_ASSIGNMENT_CAPTURE", capture)
	fake := filepath.Join(dir, "isolated-paseo")
	script := `#!/bin/sh
if [ "$1" = status ]; then printf '{"serverId":"isolated-host"}'; exit 0; fi
if [ "$1" != send ] || [ "$2" != chosen-host-session ]; then exit 17; fi
session="$2"
shift 2
while [ "$#" -gt 0 ]; do
  if [ "$1" = --prompt-file ]; then cat "$2" >> "$MULTICA_ASSIGNMENT_CAPTURE"; shift; fi
  shift
done
printf '{"agentId":"%s","status":"sent"}' "$session"
`
	if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	client := NewClient(serverURL)
	client.SetToken(token)
	client.mailboxDaemonID = daemonID
	observed := observedAssignmentClient{Client: client, results: make(chan delivery.Delivery, expected)}
	host := &paseoDeliveryHost{transport: &paseoCliTransport{bin: fake, run: runPaseoCommand, timeout: time.Second * 3}}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second*20)
	defer cancel()
	worker := delivery.NewWorker(observed, host, func() []string { return []string{workspaceID} }, daemonID, nil)
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	for range expected {
		select {
		case result := <-observed.results:
			if result.DeliveryStatus != "delivered" || !strings.HasPrefix(result.HostReceipt, "paseo:chosen-host-session:") {
				t.Errorf("unexpected receipt: %+v", result)
			}
		case <-ctx.Done():
			t.Error("host receipt timeout")
		}
	}
	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	prompt, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	return string(prompt)
}

// Browser acceptance supplies only a task-owned isolated server and credentials.
// It is opt-in so default tests never connect to a user's running environment.
func TestExternalSquadBrowserAssignmentHostReceipt(t *testing.T) {
	input := os.Getenv("MULTICA_TEST_ASSIGNMENT_INPUT")
	if input == "" {
		t.Skip("browser assignment fixture not requested")
	}
	var fixture struct{ ServerURL, Token, WorkspaceID, DaemonID, IssueID, SquadID string }
	data, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(fixture.ServerURL, "http://localhost:") && !strings.HasPrefix(fixture.ServerURL, "http://127.0.0.1:") {
		t.Fatal("browser fixture must use an isolated local API")
	}
	prompt := acceptIsolatedAssignmentRequests(t, fixture.ServerURL, fixture.Token, fixture.WorkspaceID, fixture.DaemonID, 1)
	for _, required := range []string{fixture.IssueID, fixture.SquadID, "--mailbox-id", "## Squad Operating Protocol"} {
		if !strings.Contains(prompt, required) {
			t.Errorf("host prompt missing %s", required)
		}
	}
	t.Log("browser-selected recipient -> exact isolated host session -> accepted host receipt")
}

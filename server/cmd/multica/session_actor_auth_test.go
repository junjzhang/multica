package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The actor-binding test is the one that must run against real HTTP auth: a
// binding that names agent B is paired with a real mea_ credential. Only the
// server knows which agent that credential authenticates, and the existing
// own-mailbox read is its answer. This test requires an isolated PostgreSQL
// database in DATABASE_URL; it skips (rather than falling back to a shared
// default) when the variable is missing.
type actorBindingEnv struct {
	serverURL   string
	workspaceID string
	agentA      string
	agentB      string
	sessionB    string
	tokenA      string
	tokenB      string
	tokenBID    string
	issueID     string
	fixture     *testutil.Fixture

	mu       sync.Mutex
	requests []string
}

func newActorBindingEnv(t *testing.T) *actorBindingEnv {
	t.Helper()
	dbURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dbURL == "" {
		t.Skip("DATABASE_URL is not set; this real-auth test needs an isolated database")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Skipf("open test database: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("test database not reachable: %v", err)
	}
	t.Cleanup(pool.Close)

	suffix := time.Now().UnixNano()
	base := testutil.New(pool, "", "")
	ownerID := base.User(t, "Actor Binding Owner", fmt.Sprintf("actor-binding-%d@external.test", suffix))
	workspaceID := base.Workspace(t, "Actor Binding", fmt.Sprintf("actor-binding-%d", suffix))
	base.Member(t, workspaceID, ownerID, "owner")
	f := testutil.New(pool, workspaceID, ownerID)

	// Both agents belong to the same owner and are workspace-visible. That is
	// the reported repro: agent B is readable, so a visibility check cannot
	// distinguish B's credential from A's.
	agentA := f.Agent(t, "Actor A", "", testutil.Cols{
		"owner_id": ownerID, "execution_kind": "external", "visibility": "workspace", "permission_mode": "public_to",
	})
	agentB := f.Agent(t, "Actor B", "", testutil.Cols{
		"owner_id": ownerID, "execution_kind": "external", "visibility": "workspace", "permission_mode": "public_to",
	})
	sessionA := fmt.Sprintf("paseo-session-a-%d", suffix)
	f.Insert(t, "external_agent_session", testutil.Cols{
		"workspace_id": workspaceID, "owner_user_id": ownerID, "agent_id": agentA,
		"provider": "paseo", "host_id": "host-a", "session_id": sessionA,
		"display_name": "Actor A", "daemon_id": "daemon-a",
	})
	sessionB := fmt.Sprintf("paseo-session-b-%d", suffix)
	f.Insert(t, "external_agent_session", testutil.Cols{
		"workspace_id": workspaceID, "owner_user_id": ownerID, "agent_id": agentB,
		"provider": "paseo", "host_id": "host-b", "session_id": sessionB,
		"display_name": "Actor B", "daemon_id": "daemon-b",
	})
	tokenA, _ := mintActorToken(t, f, agentA, ownerID, workspaceID)
	tokenB, tokenBID := mintActorToken(t, f, agentB, ownerID, workspaceID)

	issueID := f.Issue(t, "Actor binding issue")
	f.Cleanup(t, `DELETE FROM activity_log WHERE issue_id = $1`, issueID)
	f.Cleanup(t, `DELETE FROM comment WHERE issue_id = $1`, issueID)

	env := &actorBindingEnv{
		workspaceID: workspaceID,
		agentA:      agentA,
		agentB:      agentB,
		sessionB:    sessionB,
		tokenA:      tokenA,
		tokenB:      tokenB,
		tokenBID:    tokenBID,
		issueID:     issueID,
		fixture:     f,
	}

	queries := db.New(pool)
	hub := realtime.NewHub()
	go hub.Run()
	h := handler.New(queries, pool, hub, events.New(), service.NewEmailService(), nil, nil, analytics.NoopClient{}, handler.Config{})
	mux := chi.NewRouter()
	mux.Get("/api/agents/{id}", h.GetAgent)
	mux.Get("/api/agents/{id}/mailbox", h.ListAgentMailbox)
	mux.Get("/api/agents/{id}/sessions", h.ListAgentSessions)
	mux.Post("/api/issues/{id}/comments", h.CreateComment)
	authenticated := middleware.Auth(queries, nil, nil, nil)(mux)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.mu.Lock()
		env.requests = append(env.requests, r.Method+" "+r.URL.Path)
		env.mu.Unlock()
		authenticated.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	env.serverURL = server.URL
	return env
}

func mintActorToken(t *testing.T, f *testutil.Fixture, agentID, ownerID, workspaceID string) (string, string) {
	t.Helper()
	raw, err := auth.GenerateExternalAgentToken()
	if err != nil {
		t.Fatalf("generate external token: %v", err)
	}
	tokenID := f.Insert(t, "external_agent_token", testutil.Cols{
		"agent_id": agentID, "workspace_id": workspaceID, "owner_user_id": ownerID,
		"token_hash": auth.HashToken(raw), "expires_at": time.Now().Add(time.Hour),
	})
	return raw, tokenID
}

func (e *actorBindingEnv) recorded() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.requests...)
}

func (e *actorBindingEnv) reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.requests = nil
}

// writeBinding declares agent B and its session, but writes whichever token
// the caller supplies. The token is the only thing that decides the real actor.
func (e *actorBindingEnv) writeBinding(t *testing.T, home, token, tokenID string) {
	t.Helper()
	path := defaultSessionPath(home, e.sessionB)
	writeSessionBinding(t, path, sessionBinding(e.sessionB, e.serverURL, map[string]any{
		"host_id":      "host-b",
		"session_id":   e.sessionB,
		"agent_id":     e.agentB,
		"daemon_id":    "daemon-b",
		"workspace_id": e.workspaceID,
		"token":        token,
		"token_id":     tokenID,
	}))
}

func TestSessionStatusAndWritesUseTheCredentialsOwnAgent(t *testing.T) {
	env := newActorBindingEnv(t)
	home := t.TempDir()
	cliEnv := map[string]string{"PASEO_AGENT_ID": env.sessionB}

	t.Run("positive: B credential and B context", func(t *testing.T) {
		env.reset()
		env.writeBinding(t, home, env.tokenB, env.tokenBID)
		stdout, stderr, exitCode := runCLIProcess(t, home, cliEnv, "session", "status", "--output", "json")
		if exitCode != 0 {
			t.Fatalf("exit code = %d, stderr:\n%s", exitCode, stderr)
		}
		var report sessionStatusReport
		if err := json.Unmarshal([]byte(stdout), &report); err != nil {
			t.Fatalf("decode status JSON: %v\n%s", err, stdout)
		}
		if !report.Valid || report.Status != "connected" || report.AgentID != env.agentB {
			t.Fatalf("report = %#v, want connected for the declared agent", report)
		}
		if report.Remote == nil || report.Remote.Provider != "paseo" || report.Remote.HostID != "host-b" ||
			report.Remote.SessionID != env.sessionB || report.Remote.DaemonID != "daemon-b" {
			t.Fatalf("remote binding not confirmed: %#v", report.Remote)
		}
		if strings.Contains(stdout, env.tokenB) {
			t.Fatalf("status output leaked the credential:\n%s", stdout)
		}

		env.reset()
		_, stderr, exitCode = runCLIProcess(t, home, cliEnv, "issue", "comment", "add", env.issueID, "--content", "result from agent B", "--output", "json")
		if exitCode != 0 {
			t.Fatalf("comment exit code = %d, stderr:\n%s", exitCode, stderr)
		}
		var authorType, authorID string
		env.fixture.QueryRow(t, `SELECT author_type, author_id FROM comment WHERE issue_id = $1 ORDER BY created_at DESC LIMIT 1`, env.issueID).
			Scan(&authorType, &authorID)
		if authorType != "agent" || authorID != env.agentB {
			t.Fatalf("comment author = %s/%s, want agent/%s", authorType, authorID, env.agentB)
		}
	})

	t.Run("negative: A credential with B context", func(t *testing.T) {
		env.reset()
		env.writeBinding(t, home, env.tokenA, "not-the-recorded-token")
		stdout, stderr, exitCode := runCLIProcess(t, home, cliEnv, "session", "status", "--output", "json")
		if exitCode == 0 {
			t.Fatalf("mismatched credential reported success; stdout:\n%s", stdout)
		}
		var report sessionStatusReport
		if err := json.Unmarshal([]byte(stdout), &report); err != nil {
			t.Fatalf("decode status JSON: %v\n%s", err, stdout)
		}
		if report.Valid || report.Status != "invalid" || report.Remote == nil ||
			!strings.Contains(report.Remote.Error, "different agent") {
			t.Fatalf("report = %#v, want invalid with the identity mismatch", report)
		}
		for _, req := range env.recorded() {
			if !strings.HasPrefix(req, http.MethodGet+" ") {
				t.Fatalf("status issued a write: %s", req)
			}
		}

		before := env.fixture.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1`, env.issueID)
		env.reset()
		_, stderr, exitCode = runCLIProcess(t, home, cliEnv, "issue", "comment", "add", env.issueID, "--content", "must not be written as A", "--output", "json")
		if exitCode == 0 {
			t.Fatalf("business write accepted A's credential under B's context; stderr:\n%s", stderr)
		}
		if !strings.Contains(stderr, "does not belong") {
			t.Errorf("stderr does not name the identity mismatch:\n%s", stderr)
		}
		if after := env.fixture.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1`, env.issueID); after != before {
			t.Fatalf("comments after refused write = %d, want %d", after, before)
		}
		got := env.recorded()
		if len(got) != 1 || got[0] != http.MethodGet+" /api/agents/"+env.agentB+"/mailbox" {
			t.Fatalf("refused write sent %#v, want only the own-mailbox identity check", got)
		}

		var leaked int
		env.fixture.QueryRow(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND author_id = $2`, env.issueID, env.agentA).Scan(&leaked)
		if leaked != 0 {
			t.Fatalf("credential A wrote %d comment(s) under agent A", leaked)
		}
	})
}

func TestSessionStatusUsesExactRegisteredSession(t *testing.T) {
	for _, change := range []string{"valid second session", "missing", "host changed", "daemon changed"} {
		t.Run(change, func(t *testing.T) {
			env := newActorBindingEnv(t)
			var owner string
			env.fixture.QueryRow(t, `SELECT owner_id::text FROM agent WHERE id=$1`, env.agentB).Scan(&owner)
			env.fixture.Insert(t, "external_agent_session", testutil.Cols{
				"workspace_id": env.workspaceID, "owner_user_id": owner, "agent_id": env.agentB,
				"provider": "paseo", "host_id": "older-host", "session_id": "older-" + env.sessionB,
				"display_name": "Earlier conversation", "daemon_id": "older-daemon", "created_at": testutil.Raw("now()-interval '1 day'"),
			})
			switch change {
			case "missing":
				env.fixture.Exec(t, `DELETE FROM external_agent_session WHERE agent_id=$1 AND session_id=$2`, env.agentB, env.sessionB)
			case "host changed":
				env.fixture.Exec(t, `UPDATE external_agent_session SET host_id='new-host' WHERE agent_id=$1 AND session_id=$2`, env.agentB, env.sessionB)
			case "daemon changed":
				env.fixture.Exec(t, `UPDATE external_agent_session SET daemon_id='new-daemon' WHERE agent_id=$1 AND session_id=$2`, env.agentB, env.sessionB)
			}
			home := t.TempDir()
			env.writeBinding(t, home, env.tokenB, env.tokenBID)
			stdout, stderr, code := runCLIProcess(t, home, map[string]string{"PASEO_AGENT_ID": env.sessionB}, "session", "status", "--output", "json")
			var report sessionStatusReport
			if err := json.Unmarshal([]byte(stdout), &report); err != nil {
				t.Fatalf("decode status: %v; %s", err, stderr)
			}
			if change == "valid second session" {
				if code != 0 || !report.Valid || report.Remote.SessionID != env.sessionB || report.Remote.HostID != "host-b" {
					t.Fatalf("legitimate second session was not verified: code=%d report=%+v", code, report)
				}
			} else if code == 0 || report.Valid || report.Error == "" {
				t.Fatalf("stale session was accepted: code=%d report=%+v", code, report)
			}
		})
	}
}

package sessionroute

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type routeFixture struct {
	pool                     *pgxpool.Pool
	workspaceID, ownerID     string
	agentID, sessionA, issue string
}

func newRouteFixture(t *testing.T) routeFixture {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Skipf("database not available: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Skipf("database not reachable: %v", err)
	}
	t.Cleanup(pool.Close)

	ctx := context.Background()
	f := routeFixture{
		pool:        pool,
		workspaceID: uuid.NewString(),
		ownerID:     uuid.NewString(),
		agentID:     uuid.NewString(),
		sessionA:    uuid.NewString(),
		issue:       uuid.NewString(),
	}
	if _, err := pool.Exec(ctx, `INSERT INTO "user" (id, name, email) VALUES ($1,$2,$3)`,
		f.ownerID, "route owner "+f.ownerID, "route-"+f.ownerID+"@test.local"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace (id, name, slug) VALUES ($1,$2,$3)`,
		f.workspaceID, "route "+f.workspaceID, "route-"+f.workspaceID); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1,$2,'owner')`,
		f.workspaceID, f.ownerID); err != nil {
		t.Fatalf("insert member: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agent (id, workspace_id, name, runtime_mode, execution_kind, owner_id)
		VALUES ($1,$2,$3,'cloud','external',$4)`, f.agentID, f.workspaceID, "route agent "+f.agentID, f.ownerID); err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	f.insertSession(t, f.sessionA, "a")
	if _, err := pool.Exec(ctx, `INSERT INTO issue (id, workspace_id, number, title, creator_type, creator_id)
		VALUES ($1,$2,1,$3,'member',$4)`, f.issue, f.workspaceID, "route issue", f.ownerID); err != nil {
		t.Fatalf("insert issue: %v", err)
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		_, _ = pool.Exec(cleanup, `DELETE FROM external_agent_session_binding WHERE workspace_id=$1`, f.workspaceID)
		_, _ = pool.Exec(cleanup, `DELETE FROM agent_mailbox_message WHERE workspace_id=$1`, f.workspaceID)
		_, _ = pool.Exec(cleanup, `DELETE FROM issue WHERE workspace_id=$1`, f.workspaceID)
		_, _ = pool.Exec(cleanup, `DELETE FROM external_agent_session WHERE workspace_id=$1`, f.workspaceID)
		_, _ = pool.Exec(cleanup, `DELETE FROM agent WHERE workspace_id=$1`, f.workspaceID)
		_, _ = pool.Exec(cleanup, `DELETE FROM workspace WHERE id=$1`, f.workspaceID)
		_, _ = pool.Exec(cleanup, `DELETE FROM "user" WHERE id=$1`, f.ownerID)
	})
	return f
}

func (f routeFixture) insertSession(t *testing.T, id, suffix string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO external_agent_session
			(id, workspace_id, owner_user_id, agent_id, provider, host_id, session_id, display_name, daemon_id)
		VALUES ($1,$2,$3,$4,'paseo',$5,$6,$7,$8)`,
		id, f.workspaceID, f.ownerID, f.agentID, "host-"+suffix, "session-"+suffix, "Session "+suffix, "daemon-"+suffix); err != nil {
		t.Fatalf("insert session %s: %v", suffix, err)
	}
}

// An exact key with no binding resolves nothing, even when the agent owns a
// session: there is no implicit default-session fallback.
func TestResolveRequiresExactBinding(t *testing.T) {
	f := newRouteFixture(t)
	ctx := context.Background()
	secondIssue := uuid.NewString()
	if _, err := f.pool.Exec(ctx, `INSERT INTO issue (id, workspace_id, number, title, creator_type, creator_id)
		VALUES ($1,$2,2,$3,'member',$4)`, secondIssue, f.workspaceID, "second route issue", f.ownerID); err != nil {
		t.Fatalf("insert second issue: %v", err)
	}
	queries := db.New(f.pool)
	if _, err := Bind(ctx, queries, BindRequest{
		WorkspaceID: f.workspaceID, AgentID: f.agentID, IssueID: f.issue, SessionID: f.sessionA,
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	bound, err := Resolve(ctx, queries, f.workspaceID, f.agentID, f.issue)
	if err != nil {
		t.Fatalf("resolve bound issue: %v", err)
	}
	if bound.SessionID != f.sessionA {
		t.Fatalf("resolved session = %s, want %s", bound.SessionID, f.sessionA)
	}
	if _, err := Resolve(ctx, queries, f.workspaceID, f.agentID, secondIssue); !errors.Is(err, ErrNotBound) {
		t.Fatalf("unbound issue error = %v, want ErrNotBound", err)
	}
}

// An explicit rebind revokes the previous target, so one exact key never has
// two active rows.
func TestBindReplacesTheActiveTarget(t *testing.T) {
	f := newRouteFixture(t)
	ctx := context.Background()
	secondSession := uuid.NewString()
	f.insertSession(t, secondSession, "b")
	queries := db.New(f.pool)
	for _, sessionID := range []string{f.sessionA, secondSession} {
		if _, err := Bind(ctx, queries, BindRequest{
			WorkspaceID: f.workspaceID, AgentID: f.agentID, IssueID: f.issue, SessionID: sessionID,
		}); err != nil {
			t.Fatalf("bind %s: %v", sessionID, err)
		}
	}
	resolved, err := Resolve(ctx, queries, f.workspaceID, f.agentID, f.issue)
	if err != nil {
		t.Fatalf("resolve after rebind: %v", err)
	}
	if resolved.SessionID != secondSession {
		t.Fatalf("resolved session = %s, want replacement %s", resolved.SessionID, secondSession)
	}
	var active int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM external_agent_session_binding
		WHERE workspace_id=$1 AND agent_id=$2 AND issue_id=$3 AND revoked_at IS NULL`,
		f.workspaceID, f.agentID, f.issue).Scan(&active); err != nil {
		t.Fatalf("count active bindings: %v", err)
	}
	if active != 1 {
		t.Fatalf("active bindings = %d, want 1", active)
	}
}

// Archiving the recipient takes effect immediately: the binding no longer
// resolves, so a producer cannot enqueue work for an archived agent.
func TestResolveRejectsArchivedAgent(t *testing.T) {
	f := newRouteFixture(t)
	ctx := context.Background()
	queries := db.New(f.pool)
	if _, err := Bind(ctx, queries, BindRequest{
		WorkspaceID: f.workspaceID, AgentID: f.agentID, IssueID: f.issue, SessionID: f.sessionA,
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE agent SET archived_at = now() WHERE id=$1`, f.agentID); err != nil {
		t.Fatalf("archive agent: %v", err)
	}
	if _, err := Resolve(ctx, queries, f.workspaceID, f.agentID, f.issue); !errors.Is(err, ErrNotBound) {
		t.Fatalf("archived agent error = %v, want ErrNotBound", err)
	}
}

// A binding may only name a session of the same agent inside the same
// workspace.
func TestBindRejectsForeignTargets(t *testing.T) {
	f := newRouteFixture(t)
	ctx := context.Background()
	queries := db.New(f.pool)

	otherAgent := uuid.NewString()
	if _, err := f.pool.Exec(ctx, `INSERT INTO agent (id, workspace_id, name, runtime_mode, execution_kind, owner_id)
		VALUES ($1,$2,$3,'cloud','external',$4)`, otherAgent, f.workspaceID, "other route agent", f.ownerID); err != nil {
		t.Fatalf("insert other agent: %v", err)
	}
	foreignSession := uuid.NewString()
	if _, err := f.pool.Exec(ctx, `INSERT INTO external_agent_session
		(id, workspace_id, owner_user_id, agent_id, provider, host_id, session_id, display_name, daemon_id)
		VALUES ($1,$2,$3,$4,'paseo','host-x','session-x','Foreign','daemon-x')`,
		foreignSession, f.workspaceID, f.ownerID, otherAgent); err != nil {
		t.Fatalf("insert foreign session: %v", err)
	}
	if _, err := Bind(ctx, queries, BindRequest{
		WorkspaceID: f.workspaceID, AgentID: f.agentID, IssueID: f.issue, SessionID: foreignSession,
	}); !errors.Is(err, ErrInvalidBinding) {
		t.Fatalf("foreign session error = %v, want ErrInvalidBinding", err)
	}

	managedAgent := uuid.NewString()
	if _, err := f.pool.Exec(ctx, `INSERT INTO agent (id, workspace_id, name, runtime_mode, execution_kind, owner_id)
		VALUES ($1,$2,$3,'cloud','managed',$4)`, managedAgent, f.workspaceID, "managed route agent", f.ownerID); err != nil {
		t.Fatalf("insert managed agent: %v", err)
	}
	managedSession := uuid.NewString()
	if _, err := f.pool.Exec(ctx, `INSERT INTO external_agent_session
		(id, workspace_id, owner_user_id, agent_id, provider, host_id, session_id, display_name, daemon_id)
		VALUES ($1,$2,$3,$4,'paseo','host-m','session-m','Managed','daemon-m')`,
		managedSession, f.workspaceID, f.ownerID, managedAgent); err != nil {
		t.Fatalf("insert managed session: %v", err)
	}
	if _, err := Bind(ctx, queries, BindRequest{
		WorkspaceID: f.workspaceID, AgentID: managedAgent, IssueID: f.issue, SessionID: managedSession,
	}); !errors.Is(err, ErrInvalidBinding) {
		t.Fatalf("managed agent error = %v, want ErrInvalidBinding", err)
	}

	otherWorkspace := uuid.NewString()
	if _, err := f.pool.Exec(ctx, `INSERT INTO workspace (id, name, slug) VALUES ($1,$2,$3)`,
		otherWorkspace, "other route workspace", "route-"+otherWorkspace); err != nil {
		t.Fatalf("insert other workspace: %v", err)
	}
	defer f.pool.Exec(context.Background(), `DELETE FROM workspace WHERE id=$1`, otherWorkspace)
	if _, err := Bind(ctx, queries, BindRequest{
		WorkspaceID: otherWorkspace, AgentID: f.agentID, IssueID: f.issue, SessionID: f.sessionA,
	}); !errors.Is(err, ErrInvalidBinding) {
		t.Fatalf("cross-workspace error = %v, want ErrInvalidBinding", err)
	}
}

func TestRevokedSessionCannotResolveOrBind(t *testing.T) {
	f := newRouteFixture(t)
	q := db.New(f.pool)
	req := BindRequest{WorkspaceID: f.workspaceID, AgentID: f.agentID, IssueID: f.issue, SessionID: f.sessionA}
	if _, err := Bind(t.Context(), q, req); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), "UPDATE external_agent_session SET revoked_at=now() WHERE id=$1", f.sessionA); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(t.Context(), q, f.workspaceID, f.agentID, f.issue); !errors.Is(err, ErrNotBound) {
		t.Fatalf("resolve revoked: %v", err)
	}
	if _, err := Bind(t.Context(), q, req); !errors.Is(err, ErrInvalidBinding) {
		t.Fatalf("bind revoked: %v", err)
	}
}

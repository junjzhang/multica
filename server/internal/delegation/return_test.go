package delegation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/sessionroute"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var (
	testPool        *pgxpool.Pool
	testQueries     *db.Queries
	testWorkspaceID string
	testUserID      string
	dbfx            *testutil.Fixture
)

const (
	delegationTestEmail = "delegation-test@multica.ai"
	delegationTestSlug  = "delegation-tests"
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		fmt.Printf("Skipping delegation tests: could not connect: %v\n", err)
		os.Exit(0)
	}
	if err := pool.Ping(ctx); err != nil {
		fmt.Printf("Skipping delegation tests: database not reachable: %v\n", err)
		pool.Close()
		os.Exit(0)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM workspace WHERE slug = $1`, delegationTestSlug); err != nil {
		fmt.Printf("Skipping delegation tests: cleanup: %v\n", err)
		os.Exit(0)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM "user" WHERE email = $1`, delegationTestEmail); err != nil {
		fmt.Printf("Skipping delegation tests: cleanup user: %v\n", err)
		os.Exit(0)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('Delegation Test', $1) RETURNING id`, delegationTestEmail).Scan(&testUserID); err != nil {
		fmt.Printf("Skipping delegation tests: seed user: %v\n", err)
		os.Exit(0)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO workspace (name, slug, description, issue_prefix) VALUES ('Delegation Tests', $1, '', 'DLG') RETURNING id`, delegationTestSlug).Scan(&testWorkspaceID); err != nil {
		fmt.Printf("Skipping delegation tests: seed workspace: %v\n", err)
		os.Exit(0)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`, testWorkspaceID, testUserID); err != nil {
		fmt.Printf("Skipping delegation tests: seed member: %v\n", err)
		os.Exit(0)
	}
	testPool = pool
	testQueries = db.New(pool)
	dbfx = testutil.New(pool, testWorkspaceID, testUserID)

	code := m.Run()
	_, _ = pool.Exec(ctx, `DELETE FROM workspace WHERE slug = $1`, delegationTestSlug)
	_, _ = pool.Exec(ctx, `DELETE FROM "user" WHERE email = $1`, delegationTestEmail)
	pool.Close()
	os.Exit(code)
}

func requireDB(t *testing.T) {
	t.Helper()
	if testPool == nil {
		t.Skip("database not available")
	}
}

func managedRun(t *testing.T, opts managedRunOptions) (agentID, taskID string) {
	t.Helper()
	runtimeID := dbfx.Runtime(t, "Delegation Runtime "+opts.name)
	agentID = dbfx.Agent(t, "Delegation Agent "+opts.name, runtimeID, testutil.Cols{
		"execution_kind": "managed",
		"visibility":     "workspace",
	})
	issueID := opts.issueID
	if issueID == "" {
		issueID = dbfx.Issue(t, "Delegation scope "+opts.name)
	}
	cols := testutil.Cols{
		"agent_id":   agentID,
		"issue_id":   issueID,
		"runtime_id": runtimeID,
		"status":     "running",
	}
	if opts.pinned {
		cols["session_id"] = "provider-session-" + opts.name
		cols["work_dir"] = "/work/" + opts.name
	}
	taskID = dbfx.Task(t, agentID, cols)
	return agentID, taskID
}

type managedRunOptions struct {
	name    string
	issueID string
	pinned  bool
}

func agentTarget(id string) Assignee {
	return Assignee{Type: "agent", ID: id}
}

func externalSession(t *testing.T, name string) (agentID, sessionID, hostSessionID string) {
	t.Helper()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	ownerID := dbfx.User(t, name+" Owner", fmt.Sprintf("delegation-%s-%s@external.test", name, suffix))
	dbfx.Member(t, testWorkspaceID, ownerID, "member")
	agentID = dbfx.Agent(t, name+" "+suffix, "", testutil.Cols{
		"owner_id":        ownerID,
		"execution_kind":  "external",
		"visibility":      "workspace",
		"permission_mode": "public_to",
	})
	hostSessionID = "paseo-session-" + suffix
	sessionID = dbfx.Insert(t, "external_agent_session", testutil.Cols{
		"workspace_id":  testWorkspaceID,
		"owner_user_id": ownerID,
		"agent_id":      agentID,
		"provider":      "paseo",
		"host_id":       "paseo-host-" + suffix,
		"session_id":    hostSessionID,
		"display_name":  name,
		"daemon_id":     "daemon-" + suffix,
	})
	return agentID, sessionID, hostSessionID
}

func TestResolveManagedRequiresAPinnedActiveRun(t *testing.T) {
	requireDB(t)
	agentID, taskID := managedRun(t, managedRunOptions{name: "pinned", pinned: true})
	addr, err := ResolveManaged(context.Background(), testQueries, testWorkspaceID, agentID, taskID)
	if err != nil {
		t.Fatalf("ResolveManaged: %v", err)
	}
	if addr.Kind != KindManagedTask || addr.SourceTaskID != taskID || addr.DispatcherAgentID != agentID {
		t.Fatalf("address = %+v, want managed task %s for agent %s", addr, taskID, agentID)
	}
}

func TestResolveManagedRefusesUnpinnedRunWithoutOldSession(t *testing.T) {
	requireDB(t)
	agentID, taskID := managedRun(t, managedRunOptions{name: "unpinned", pinned: false})
	_, err := ResolveManaged(context.Background(), testQueries, testWorkspaceID, agentID, taskID)
	if !errors.Is(err, ErrReturnNotReady) {
		t.Fatalf("err = %v, want ErrReturnNotReady", err)
	}
}

func TestResolveManagedRejectsForeignAgentAndScope(t *testing.T) {
	requireDB(t)
	agentID, taskID := managedRun(t, managedRunOptions{name: "foreign", pinned: true})
	otherAgent, _ := managedRun(t, managedRunOptions{name: "other", pinned: true})

	if _, err := ResolveManaged(context.Background(), testQueries, testWorkspaceID, otherAgent, taskID); !errors.Is(err, ErrReturnInvalid) {
		t.Fatalf("foreign agent err = %v, want ErrReturnInvalid", err)
	}
	if _, err := ResolveManaged(context.Background(), testQueries, testWorkspaceID, agentID, "00000000-0000-0000-0000-000000000001"); !errors.Is(err, ErrReturnInvalid) {
		t.Fatalf("missing run err = %v, want ErrReturnInvalid", err)
	}
}

// A completed source run is NOT an eligible dispatcher for a new delegation:
// its provider session and pin are still stored, but the run has finished and
// must not keep creating child work. A relationship it already accepted stays
// readable as a stored target (see the handler regression test), which is a
// different question from admitting a new dispatch.
func TestResolveManagedRejectsCompletedSourceForNewDelegation(t *testing.T) {
	requireDB(t)
	agentID, taskID := managedRun(t, managedRunOptions{name: "terminal", pinned: true})
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, taskID)
	if _, err := ResolveManaged(context.Background(), testQueries, testWorkspaceID, agentID, taskID); !errors.Is(err, ErrReturnSourceClosed) {
		t.Fatalf("err = %v, want ErrReturnSourceClosed", err)
	}
}

func TestResolveManagedRefusesCompletedRunBeforePin(t *testing.T) {
	requireDB(t)
	agentID, taskID := managedRun(t, managedRunOptions{name: "terminal-unpinned", pinned: false})
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, taskID)
	if _, err := ResolveManaged(context.Background(), testQueries, testWorkspaceID, agentID, taskID); !errors.Is(err, ErrReturnSourceClosed) {
		t.Fatalf("err = %v, want ErrReturnSourceClosed", err)
	}
}

func TestResolveManagedChatScope(t *testing.T) {
	requireDB(t)
	runtimeID := dbfx.Runtime(t, "Delegation Chat Runtime")
	agentID := dbfx.Agent(t, "Delegation Chat Agent", runtimeID, testutil.Cols{"execution_kind": "managed"})
	chatID := dbfx.ChatSession(t, agentID)
	taskID := dbfx.Task(t, agentID, testutil.Cols{
		"chat_session_id": chatID,
		"runtime_id":      runtimeID,
		"status":          "running",
		"session_id":      "chat-provider-session",
		"work_dir":        "/work/chat",
	})
	if _, err := ResolveManaged(context.Background(), testQueries, testWorkspaceID, agentID, taskID); err != nil {
		t.Fatalf("chat-scoped run: %v", err)
	}
}

func TestResolveExternalMatchesRegisteredSessionOnly(t *testing.T) {
	requireDB(t)
	agentID, sessionID, hostSessionID := externalSession(t, "external")
	for _, ref := range []string{sessionID, hostSessionID} {
		addr, err := ResolveExternal(context.Background(), testQueries, testWorkspaceID, agentID, ref)
		if err != nil {
			t.Fatalf("ResolveExternal(%q): %v", ref, err)
		}
		if addr.Kind != KindExternalSession || addr.ExternalSessionID != sessionID {
			t.Fatalf("address = %+v, want external session %s", addr, sessionID)
		}
	}
	if _, err := ResolveExternal(context.Background(), testQueries, testWorkspaceID, agentID, ""); !errors.Is(err, ErrReturnMissing) {
		t.Fatalf("empty ref err = %v, want ErrReturnMissing", err)
	}
	if _, err := ResolveExternal(context.Background(), testQueries, testWorkspaceID, agentID, "unregistered-session"); !errors.Is(err, ErrReturnInvalid) {
		t.Fatalf("unregistered err = %v, want ErrReturnInvalid", err)
	}
	otherAgent, otherSession, _ := externalSession(t, "external-other")
	if _, err := ResolveExternal(context.Background(), testQueries, testWorkspaceID, otherAgent, sessionID); !errors.Is(err, ErrReturnInvalid) {
		t.Fatalf("foreign session err = %v, want ErrReturnInvalid", err)
	}
	if addr, err := ResolveExternal(context.Background(), testQueries, testWorkspaceID, otherAgent, otherSession); err != nil || addr.ExternalSessionID != otherSession {
		t.Fatalf("own session after foreign attempt = %+v, %v", addr, err)
	}
}

func TestEnsureKeepsFirstAcceptedReturnPerDispatcher(t *testing.T) {
	requireDB(t)
	agentID, sessionID, _ := externalSession(t, "ensure")
	issueID := dbfx.Issue(t, "Delegation ensure")

	first, err := Ensure(context.Background(), testQueries, testWorkspaceID, issueID, Address{
		DispatcherAgentID: agentID, Kind: KindExternalSession, ExternalSessionID: sessionID,
	}, testUserID)
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	if first.ReturnKind != string(KindExternalSession) || first.DispatcherAgentID.Valid == false {
		t.Fatalf("first row = %+v", first)
	}

	// A second physical session for the SAME dispatcher is a conflict, not a
	// silent reuse of the old session and not a silent overwrite.
	secondSession := dbfx.Insert(t, "external_agent_session", testutil.Cols{
		"workspace_id": testWorkspaceID, "owner_user_id": testUserID, "agent_id": agentID,
		"provider": "paseo", "host_id": "ensure-second-host",
		"session_id": "ensure-second-host-session", "display_name": "second",
		"daemon_id": "ensure-second-daemon",
	})
	if _, err := Ensure(context.Background(), testQueries, testWorkspaceID, issueID, Address{
		DispatcherAgentID: agentID, Kind: KindExternalSession, ExternalSessionID: secondSession,
	}, testUserID); !errors.Is(err, ErrReturnConflict) {
		t.Fatalf("different session err = %v, want ErrReturnConflict", err)
	}
	// An explicit takeover moves the binding; the accepted address follows it.
	if _, err := sessionroute.Bind(context.Background(), testQueries, sessionroute.BindRequest{
		WorkspaceID: testWorkspaceID, AgentID: agentID, IssueID: issueID, SessionID: secondSession, CreatedBy: testUserID,
	}); err != nil {
		t.Fatalf("explicit takeover: %v", err)
	}
	takenOver, err := Ensure(context.Background(), testQueries, testWorkspaceID, issueID, Address{
		DispatcherAgentID: agentID, Kind: KindExternalSession, ExternalSessionID: secondSession,
	}, testUserID)
	if err != nil {
		t.Fatalf("Ensure after takeover: %v", err)
	}
	if takenOver.ID != first.ID || util.UUIDToString(takenOver.ExternalSessionID) != secondSession {
		t.Fatalf("takeover did not move the accepted address: %+v", takenOver)
	}

	// A DIFFERENT dispatcher has its own key and its own accepted address.
	otherAgent, otherSession, _ := externalSession(t, "ensure-other")
	second, err := Ensure(context.Background(), testQueries, testWorkspaceID, issueID, Address{
		DispatcherAgentID: otherAgent, Kind: KindExternalSession, ExternalSessionID: otherSession,
	}, testUserID)
	if err != nil {
		t.Fatalf("second dispatcher Ensure: %v", err)
	}
	if second.ID == first.ID || util.UUIDToString(second.DispatcherAgentID) != otherAgent || util.UUIDToString(second.ExternalSessionID) != otherSession {
		t.Fatalf("second dispatcher return = %+v, want its own row for %s/%s", second, otherAgent, otherSession)
	}
	if n := countRowsIn(t, `SELECT count(*) FROM issue_delegation_return WHERE workspace_id = $1 AND issue_id = $2`, testWorkspaceID, issueID); n != 2 {
		t.Fatalf("accepted return rows = %d, want one per dispatcher", n)
	}
}

func TestConcurrentEnsureIsPerDispatcher(t *testing.T) {
	requireDB(t)
	agentA, sessionA, _ := externalSession(t, "race-a")
	agentB, sessionB, _ := externalSession(t, "race-b")
	issueID := dbfx.Issue(t, "Delegation race")

	type result struct {
		row db.IssueDelegationReturn
		err error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for _, candidate := range []Address{
		{DispatcherAgentID: agentA, Kind: KindExternalSession, ExternalSessionID: sessionA},
		{DispatcherAgentID: agentB, Kind: KindExternalSession, ExternalSessionID: sessionB},
	} {
		go func(addr Address) {
			<-start
			row, err := Ensure(context.Background(), testQueries, testWorkspaceID, issueID, addr, testUserID)
			results <- result{row: row, err: err}
		}(candidate)
	}
	close(start)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent Ensure errors: %v / %v", first.err, second.err)
	}
	if first.row.ID == second.row.ID {
		t.Fatalf("distinct dispatchers shared one return row: %s", first.row.ID)
	}
	if n := countRowsIn(t, `SELECT count(*) FROM issue_delegation_return WHERE workspace_id = $1 AND issue_id = $2`, testWorkspaceID, issueID); n != 2 {
		t.Fatalf("accepted return rows = %d, want 2", n)
	}
}

func TestConcurrentEnsureSameDispatcherAcceptsOneRow(t *testing.T) {
	requireDB(t)
	agentID, sessionID, _ := externalSession(t, "race-same")
	issueID := dbfx.Issue(t, "Delegation same-dispatcher race")
	rows := make(chan db.IssueDelegationReturn, 2)
	errs := make(chan error, 2)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			row, err := Ensure(context.Background(), testQueries, testWorkspaceID, issueID, Address{
				DispatcherAgentID: agentID, Kind: KindExternalSession, ExternalSessionID: sessionID,
			}, testUserID)
			rows <- row
			errs <- err
		}()
	}
	close(start)
	first, second := <-rows, <-rows
	if err := <-errs; err != nil {
		t.Fatalf("concurrent Ensure error: %v", err)
	}
	if err := <-errs; err != nil {
		t.Fatalf("concurrent Ensure error: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("same dispatcher accepted two rows: %s vs %s", first.ID, second.ID)
	}
	if n := countRowsIn(t, `SELECT count(*) FROM issue_delegation_return WHERE workspace_id = $1 AND issue_id = $2`, testWorkspaceID, issueID); n != 1 {
		t.Fatalf("accepted return rows = %d, want 1", n)
	}
}

func countRowsIn(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestManagedAcceptedReturnWithoutSnapshotFailsClosed(t *testing.T) {
	taskID := util.MustParseUUID("00000000-0000-0000-0000-000000000001")
	runtimeID := util.MustParseUUID("00000000-0000-0000-0000-000000000002")
	original := db.IssueDelegationReturn{
		ReturnKind: string(KindManagedTask), SourceTaskID: taskID, RuntimeID: runtimeID,
		Provider:  pgtype.Text{String: "source-provider", Valid: true},
		SessionID: pgtype.Text{String: "accepted-session", Valid: true},
		WorkDir:   pgtype.Text{String: "/accepted/work", Valid: true},
	}
	current := db.AgentTaskQueue{ID: taskID, RuntimeID: runtimeID, SessionID: original.SessionID, WorkDir: original.WorkDir}
	runtime := db.AgentRuntime{ID: runtimeID, Provider: original.Provider.String}
	for _, missing := range []string{"runtime", "provider", "session", "work_dir"} {
		t.Run(missing, func(t *testing.T) {
			accepted := original
			switch missing {
			case "runtime":
				accepted.RuntimeID = pgtype.UUID{}
			case "provider":
				accepted.Provider = pgtype.Text{}
			case "session":
				accepted.SessionID = pgtype.Text{}
			case "work_dir":
				accepted.WorkDir = pgtype.Text{}
			}
			// No query handle: a missing snapshot must never consult the mutable
			// source task to reconstruct an address, even for the same task ID.
			_, err := reuseAccepted(context.Background(), nil, accepted, current, runtime, true, sessionroute.Binding{}, false)
			if !errors.Is(err, ErrReturnConflict) {
				t.Fatalf("missing %s accepted: %v", missing, err)
			}
		})
	}
}

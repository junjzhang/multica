package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestCommentMentionsAnyone covers the pure helper that drives the
// "skip leader on @<anyone>" behavior. Routing-style mentions
// (agent/member/squad/all) count; issue cross-references do not. Kept as a
// unit test so it runs without a database connection.
func TestCommentMentionsAnyone(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{name: "empty", content: "", want: false},
		{name: "plain text", content: "please take a look", want: false},
		{name: "literal at sign only", content: "ping @alice", want: false},
		{name: "agent mention", content: "[@A](mention://agent/11111111-1111-1111-1111-111111111111) handle this", want: true},
		{name: "member mention", content: "[@Bob](mention://member/22222222-2222-2222-2222-222222222222)", want: true},
		{name: "squad mention", content: "[@Squad](mention://squad/44444444-4444-4444-4444-444444444444)", want: true},
		{name: "mention all", content: "[@all](mention://all/all)", want: true},
		{name: "issue mention only", content: "see [MUL-1](mention://issue/33333333-3333-3333-3333-333333333333)", want: false},
		{name: "issue + plain text", content: "see [MUL-1](mention://issue/33333333-3333-3333-3333-333333333333) for context", want: false},
		{name: "agent plus member", content: "[@A](mention://agent/11111111-1111-1111-1111-111111111111) cc [@B](mention://member/22222222-2222-2222-2222-222222222222)", want: true},
		{name: "issue plus member", content: "blocks [MUL-1](mention://issue/33333333-3333-3333-3333-333333333333) — [@Bob](mention://member/22222222-2222-2222-2222-222222222222)", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := commentMentionsAnyone(tc.content); got != tc.want {
				t.Fatalf("commentMentionsAnyone(%q) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}

// shouldEnqueueSquadLeaderOnCommentForTest reports whether the shared cascade
// would wake the issue's assigned squad leader.
func shouldEnqueueSquadLeaderOnCommentForTest(ctx context.Context, issue db.Issue, content, authorType, authorID string) bool {
	triggers, _ := testHandler.computeCommentAgentTriggers(ctx, issue, content, nil, authorType, authorID, commentTriggerComputeOptions{})
	return triggersContainIssueAssigneeSquadLeader(triggers)
}

func shouldEnqueueSquadLeaderOnReplyForTest(ctx context.Context, issue db.Issue, content string, parent *db.Comment, authorType, authorID string) bool {
	triggers, _ := testHandler.computeCommentAgentTriggers(ctx, issue, content, parent, authorType, authorID, commentTriggerComputeOptions{})
	return triggersContainIssueAssigneeSquadLeader(triggers)
}

func triggersContainIssueAssigneeSquadLeader(triggers []commentAgentTrigger) bool {
	for _, trigger := range triggers {
		if trigger.Source == commentTriggerSourceIssueAssignee && trigger.Squad != nil {
			return true
		}
	}
	return false
}

// squadCommentTriggerFixture wires a squad assigned to a fresh issue and
// returns the loaded db.Issue plus the leader agent UUID for use in
// cascade integration tests.
type squadCommentTriggerFixture struct {
	Issue    db.Issue
	SquadID  string
	LeaderID string
	OtherID  string // second agent in workspace (with runtime), used as a non-leader @mention target
}

func newSquadCommentTriggerFixture(t *testing.T) squadCommentTriggerFixture {
	t.Helper()
	ctx := context.Background()

	// Reuse the seeded "Handler Test Agent" as the leader — it has a runtime.
	var leaderID string
	if err := testPool.QueryRow(ctx, `
		SELECT id FROM agent WHERE workspace_id = $1 ORDER BY created_at ASC LIMIT 1
	`, testWorkspaceID).Scan(&leaderID); err != nil {
		t.Fatalf("load leader agent: %v", err)
	}

	// Spin up a second agent in the same workspace as a non-leader mention
	// target. createHandlerTestAgent installs a t.Cleanup row deletion.
	otherID := createHandlerTestAgent(t, "Squad Comment Other", nil)

	var squadID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO squad (workspace_id, name, description, leader_id, creator_id)
		VALUES ($1, $2, '', $3, $4)
		RETURNING id
	`, testWorkspaceID, "Squad Comment Trigger", leaderID, testUserID).Scan(&squadID); err != nil {
		t.Fatalf("create squad: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM squad WHERE id = $1`, squadID)
	})

	var issueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, creator_type, creator_id, title, assignee_type, assignee_id)
		VALUES ($1, 'member', $2, $3, 'squad', $4)
		RETURNING id
	`, testWorkspaceID, testUserID, "squad comment trigger", squadID).Scan(&issueID); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
	})

	issue, err := testHandler.Queries.GetIssue(ctx, util.MustParseUUID(issueID))
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}

	return squadCommentTriggerFixture{
		Issue:    issue,
		SquadID:  squadID,
		LeaderID: leaderID,
		OtherID:  otherID,
	}
}

// TestShouldEnqueueSquadLeaderOnComment_SkipsWhenCommentRoutesElsewhere
// pins the cascade: explicit participant mentions do not also wake the assigned
// squad leader. Issue cross-references are not routing and do not suppress the
// leader.
func TestShouldEnqueueSquadLeaderOnComment_SkipsWhenMemberMentionsAnyone(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newSquadCommentTriggerFixture(t)
	ctx := context.Background()

	cases := []struct {
		name        string
		content     string
		authorType  string
		authorID    string
		want        bool
		description string
	}{
		{
			name:        "member plain comment triggers leader",
			content:     "what is the latest on this?",
			authorType:  "member",
			authorID:    testUserID,
			want:        false,
			description: "a plain comment is discussion; it never wakes the leader",
		},
		{
			name:        "member issue cross-reference only triggers leader",
			content:     "blocked by [MUL-1](mention://issue/" + testUserID + ")",
			authorType:  "member",
			authorID:    testUserID,
			want:        false,
			description: "issue references are not routing; nothing wakes the leader",
		},
		{
			name:        "member mentions another member skips leader",
			content:     "[@self](mention://member/" + testUserID + ") please weigh in",
			authorType:  "member",
			authorID:    testUserID,
			want:        false,
			description: "user routed at a human — leader stays out (extended rule)",
		},
		{
			name:        "member mentions non-leader agent skips leader",
			content:     "[@Other](mention://agent/" + fx.OtherID + ") please take this",
			authorType:  "member",
			authorID:    testUserID,
			want:        false,
			description: "user routed at an agent — leader stays out",
		},
		{
			name:        "member mentions leader skips leader on comment path",
			content:     "[@Leader](mention://agent/" + fx.LeaderID + ") your call",
			authorType:  "member",
			authorID:    testUserID,
			want:        false,
			description: "even @leader is dispatched via the mention path; comment path must not double-enqueue",
		},
		{
			name:        "member mention all skips leader",
			content:     "[@all](mention://all/all) heads up",
			authorType:  "member",
			authorID:    testUserID,
			want:        false,
			description: "@all is a broadcast — leader does not need to wake to evaluate routing",
		},
		{
			name:        "member mentions a squad skips leader",
			content:     "handing to [@Other Squad](mention://squad/" + fx.SquadID + ")",
			authorType:  "member",
			authorID:    testUserID,
			want:        false,
			description: "@squad routes the issue to that squad's leader — current leader stays out",
		},
		{
			name:        "agent comment with @agent does not also trigger leader",
			content:     "delegating to [@Other](mention://agent/" + fx.OtherID + ")",
			authorType:  "agent",
			authorID:    fx.OtherID,
			want:        false,
			description: "explicit @agent routes only to the mentioned target; the assigned squad-leader fallback must not also fire (no double-enqueue)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldEnqueueSquadLeaderOnCommentForTest(ctx, fx.Issue, tc.content, tc.authorType, tc.authorID)
			if got != tc.want {
				t.Fatalf("%s\n  content=%q author=%s/%s\n  got=%v want=%v",
					tc.description, tc.content, tc.authorType, tc.authorID, got, tc.want)
			}
		})
	}
}

// TestShouldEnqueueSquadLeaderOnComment_AgentAuthoredWorkerCommentsWakeLeader
// pins the MUL-3879 restored behavior in the new MUL-3794 cascade: an
// agent-authored worker-result comment on a squad-assigned issue wakes the
// assigned squad leader so the leader→worker→leader coordination loop stays
// closed, while the leader's own self-trigger loop stays suppressed.
// TestCreateComment_SquadPlainReplyToMemberParentKeepsRootMentionOwner drives the
// full CreateComment handler to lock the cascade's reply behavior:
//
//   - A member top-level comment that @mentions another agent does NOT
//     enqueue the squad leader (the mentioned agent owns the next step).
//   - A subsequent member reply to that member-authored root with no explicit
//     agent mention continues to the root owner instead of the assignee.
//
// TestCreateComment_DualRoleAgentWorkerCommentWakesLeader pins the MUL-3879
// restored coordination loop at the full-handler level. Scenario:
//
//   - Agent L is the leader of squad S and also runs worker tasks on issues
//     belonging to S.
//   - L is woken in its worker role (is_leader_task=false) and posts a result
//     comment.
//   - A leader-role task IS enqueued so the squad leader can coordinate the
//     next step — the worker result must not silently strand the issue.
//
// TestCreateComment_SquadLeaderMentionTaskDoesNotSelfTriggerAssignedFallback
// pins MUL-4024's direct-mention gap:
//
//   - A member explicitly @mentions the issue's assigned squad leader by agent
//     id, which queues a generic mention task for L (is_leader_task=false,
//     squad_id=NULL).
//   - L posts a plain reply while running that mention task.
//   - The assigned-squad fallback must not treat that generic mention task as a
//     same-squad worker result and queue L again as the leader.
func TestCreateComment_SquadLeaderMentionTaskDoesNotSelfTriggerAssignedFallback(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	fx := newSquadCommentTriggerFixture(t)
	issueID := uuidToString(fx.Issue.ID)

	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
		testPool.Exec(context.Background(), `DELETE FROM comment WHERE issue_id = $1`, issueID)
	})

	postMemberComment := func(body map[string]any) CommentResponse {
		t.Helper()
		w := httptest.NewRecorder()
		r := newRequest("POST", "/api/issues/"+issueID+"/comments", body)
		r = withURLParam(r, "id", issueID)
		testHandler.CreateComment(w, r)
		if w.Code != http.StatusCreated {
			t.Fatalf("CreateComment(member): expected 201, got %d: %s", w.Code, w.Body.String())
		}
		var resp CommentResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode member comment: %v", err)
		}
		return resp
	}
	postAgentComment := func(taskID string, body map[string]any) {
		t.Helper()
		w := httptest.NewRecorder()
		r := newRequest("POST", "/api/issues/"+issueID+"/comments", body)
		r.Header.Set("X-Agent-ID", fx.LeaderID)
		r.Header.Set("X-Task-ID", taskID)
		r = withURLParam(r, "id", issueID)
		testHandler.CreateComment(w, r)
		if w.Code != http.StatusCreated {
			t.Fatalf("CreateComment(agent): expected 201, got %d: %s", w.Code, w.Body.String())
		}
	}
	countQueuedLeaderTasks := func() int {
		t.Helper()
		var n int
		if err := testPool.QueryRow(ctx, `
			SELECT count(*) FROM agent_task_queue
			WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued' AND is_leader_task = TRUE
		`, issueID, fx.LeaderID).Scan(&n); err != nil {
			t.Fatalf("count queued leader tasks: %v", err)
		}
		return n
	}

	trigger := postMemberComment(map[string]any{
		"content": "[@Leader](mention://agent/" + fx.LeaderID + ") can you check this?",
	})

	var mentionTaskID string
	if err := testPool.QueryRow(ctx, `
		SELECT id FROM agent_task_queue
		WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'
		  AND is_leader_task = FALSE AND squad_id IS NULL
		ORDER BY created_at DESC
		LIMIT 1
	`, issueID, fx.LeaderID).Scan(&mentionTaskID); err != nil {
		t.Fatalf("load leader mention task: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE agent_task_queue SET status = 'running' WHERE id = $1`, mentionTaskID); err != nil {
		t.Fatalf("mark mention task running: %v", err)
	}

	postAgentComment(mentionTaskID, map[string]any{
		"content":   "checked, no action needed",
		"parent_id": trigger.ID,
	})

	if got := countQueuedLeaderTasks(); got != 0 {
		t.Fatalf("leader reply from generic mention task queued %d leader tasks, want 0", got)
	}
}

// TestCreateComment_SquadLeaderThreadParentTaskDoesNotSelfTriggerAssignedFallback
// pins the self-trigger guard across the thread-parent path: a member reply to
// the leader's earlier comment continues L in the leader role (MUL-7006), and
// L's reply from THAT task must still not queue L again as the assigned squad
// leader.
// TestCreateComment_ReplyToAgentCommentContinuesAuthoringRole pins MUL-7006: a
// member reply to an agent comment continues the ROLE the replied-to comment
// was written in. A comment authored by a squad-leader run continues as a
// leader task — which is what keeps the squad briefing, the coordinator's
// managed workdir, and its provider session across the turn. A worker-role
// comment, or one with no recorded authoring run, stays a generic direct-agent
// task.
// TestCreateRetryTask_InheritsIsLeaderTask locks the retry-clone contract for
// MUL-2218: auto-retry of a leader-role task must produce a child task that is
// also is_leader_task=true. Without this, MaybeRetryFailedTask silently
// demotes a retried leader task to a worker task, and role-specific claim-time
// briefing/self-mention guards lose the leader provenance.
func TestCreateRetryTask_InheritsIsLeaderTask(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	fx := newSquadCommentTriggerFixture(t)
	issueID := uuidToString(fx.Issue.ID)

	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	})

	var runtimeID string
	if err := testPool.QueryRow(ctx, `SELECT runtime_id FROM agent WHERE id = $1`, fx.LeaderID).Scan(&runtimeID); err != nil {
		t.Fatalf("load runtime: %v", err)
	}

	cases := []struct {
		name     string
		isLeader bool
	}{
		{name: "leader task retry stays leader", isLeader: true},
		{name: "worker task retry stays worker", isLeader: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var parentID string
			if err := testPool.QueryRow(ctx, `
				INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, attempt, max_attempts, is_leader_task)
				VALUES ($1, $2, $3, 'failed', 1, 3, $4)
				RETURNING id
			`, fx.LeaderID, runtimeID, issueID, tc.isLeader).Scan(&parentID); err != nil {
				t.Fatalf("seed parent task: %v", err)
			}
			t.Cleanup(func() {
				testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1 OR parent_task_id = $1`, parentID)
			})

			child, err := testHandler.Queries.CreateRetryTask(ctx, db.CreateRetryTaskParams{ID: util.MustParseUUID(parentID)})
			if err != nil {
				t.Fatalf("CreateRetryTask: %v", err)
			}
			if child.IsLeaderTask != tc.isLeader {
				t.Fatalf("child.IsLeaderTask = %v, want %v (parent role must be inherited)", child.IsLeaderTask, tc.isLeader)
			}
		})
	}
}

// TestCreateComment_SquadMentionPrivateLeaderBlocksPlainMember verifies that
// a plain workspace member cannot trigger a private squad leader via @squad
// mention. This is the regression test for the P1 finding: without the
// canAccessPrivateAgent gate in the squad mention branch, a member could
// bypass the private-agent restriction by mentioning the squad instead of
// the agent directly.
func TestCreateComment_SquadMentionPrivateLeaderBlocksPlainMember(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	// Use privateAgentTestFixture to get a private agent + plain member.
	agentID, _, memberID := privateAgentTestFixture(t)

	// Create a squad with the private agent as leader.
	var squadID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO squad (workspace_id, name, description, leader_id, creator_id)
		VALUES ($1, 'Private Leader Squad', '', $2, $3)
		RETURNING id
	`, testWorkspaceID, agentID, testUserID).Scan(&squadID); err != nil {
		t.Fatalf("create squad: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM squad WHERE id = $1`, squadID)
	})

	// Create an issue.
	var issueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, creator_type, creator_id, title)
		VALUES ($1, 'member', $2, 'private leader squad mention test')
		RETURNING id
	`, testWorkspaceID, memberID).Scan(&issueID); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
		testPool.Exec(context.Background(), `DELETE FROM comment WHERE issue_id = $1`, issueID)
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
	})

	// Plain member posts a comment mentioning the squad.
	w := httptest.NewRecorder()
	r := newRequestAs(memberID, "POST", "/api/issues/"+issueID+"/comments", map[string]any{
		"content": "[@Squad](mention://squad/" + squadID + ") please handle",
	})
	r = withURLParam(r, "id", issueID)
	testHandler.CreateComment(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateComment: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// The private leader must NOT have a queued task — plain member lacks access.
	var count int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'`,
		issueID, agentID,
	).Scan(&count); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if count != 0 {
		t.Fatalf("private leader got %d queued tasks from plain member squad mention; want 0 (access denied)", count)
	}
}

// TestCreateComment_SquadMentionTriggersLeader verifies that @mentioning a
// squad in a comment triggers the squad's leader agent via the mention path,
// even when the issue is NOT assigned to that squad.
func TestCreateComment_SquadMentionTriggersLeader(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	// Create a squad with a leader agent.
	var leaderID string
	if err := testPool.QueryRow(ctx, `
		SELECT id FROM agent WHERE workspace_id = $1 ORDER BY created_at ASC LIMIT 1
	`, testWorkspaceID).Scan(&leaderID); err != nil {
		t.Fatalf("load leader agent: %v", err)
	}

	var squadID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO squad (workspace_id, name, description, leader_id, creator_id)
		VALUES ($1, $2, '', $3, $4)
		RETURNING id
	`, testWorkspaceID, "Mention Trigger Squad", leaderID, testUserID).Scan(&squadID); err != nil {
		t.Fatalf("create squad: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM squad WHERE id = $1`, squadID)
	})

	// Create an issue NOT assigned to the squad (assigned to nobody).
	var issueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, creator_type, creator_id, title)
		VALUES ($1, 'member', $2, 'squad mention trigger test')
		RETURNING id
	`, testWorkspaceID, testUserID).Scan(&issueID); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
		testPool.Exec(context.Background(), `DELETE FROM comment WHERE issue_id = $1`, issueID)
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
	})

	countQueued := func(agentID string) int {
		var n int
		if err := testPool.QueryRow(ctx,
			`SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'`,
			issueID, agentID,
		).Scan(&n); err != nil {
			t.Fatalf("count tasks for %s: %v", agentID, err)
		}
		return n
	}

	// Post a comment that @mentions the squad.
	w := httptest.NewRecorder()
	r := newRequest("POST", "/api/issues/"+issueID+"/comments", map[string]any{
		"content": "[@Squad](mention://squad/" + squadID + ") please handle this",
	})
	r = withURLParam(r, "id", issueID)
	testHandler.CreateComment(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateComment: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// The squad's leader should have a queued task.
	if got := countQueued(leaderID); got != 1 {
		t.Fatalf("after @squad mention: expected 1 leader task, got %d", got)
	}
}

// TestPlainCommentsDoNotWakeManagedAgents pins explicit-mention-only routing
// for the squad-assignment path: neither a member's plain comment nor an
// agent's worker-result reply may wake the leader. An explicit @ does.
func TestPlainCommentsDoNotWakeManagedAgents(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newSquadCommentTriggerFixture(t)
	issueID := uuidToString(fx.Issue.ID)
	leaderTasks := func() int {
		var n int
		dbfx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'`, issueID, fx.LeaderID).Scan(&n)
		return n
	}

	w := httptest.NewRecorder()
	r := withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{
		"content": "what is the latest on this?",
	}), "id", issueID)
	testHandler.CreateComment(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("plain member comment: %d %s", w.Code, w.Body.String())
	}
	if n := leaderTasks(); n != 0 {
		t.Fatalf("plain member comment woke the leader %d time(s), want 0", n)
	}

	parentID := dbfx.Comment(t, issueID, "leader result", testutil.Cols{"author_type": "agent", "author_id": fx.LeaderID})
	w = httptest.NewRecorder()
	r = withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{
		"content":   "worker finished the task",
		"parent_id": parentID,
	}), "id", issueID)
	r.Header.Set("X-Agent-ID", fx.OtherID)
	testHandler.CreateComment(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("worker reply: %d %s", w.Code, w.Body.String())
	}
	if n := leaderTasks(); n != 0 {
		t.Fatalf("plain worker reply woke the leader %d time(s), want 0", n)
	}

	w = httptest.NewRecorder()
	r = withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{
		"content": "[@Leader](mention://agent/" + fx.LeaderID + ") your call",
	}), "id", issueID)
	testHandler.CreateComment(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("explicit mention: %d %s", w.Code, w.Body.String())
	}
	if n := leaderTasks(); n != 1 {
		t.Fatalf("explicit leader mention queued %d tasks, want 1", n)
	}
}

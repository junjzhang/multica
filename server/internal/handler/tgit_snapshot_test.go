package handler

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/vcs"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func snapshotFixture(t *testing.T) (string, string, string) {
	t.Helper()
	conn := seedVCSConnection(t, context.Background(), withVCSBox(t), "tgit", "https://git.test")
	issue := dbfx.Issue(t, "snapshot issue", testutil.Cols{"status": "in_progress"})
	pr := dbfx.Insert(t, "vcs_pull_request", testutil.Cols{
		"workspace_id": testWorkspaceID, "connection_id": conn, "provider": "tgit", "repo_owner": "org", "repo_name": "repo", "pr_number": 42,
		"title": "MR", "state": "open", "html_url": "http://git.test/org/repo/merge_requests/42", "head_sha": "head",
		"pr_created_at": "2026-10-26T06:00:00Z", "pr_updated_at": "2026-10-26T07:00:00Z",
	})
	dbfx.Exec(t, `INSERT INTO issue_vcs_pull_request(issue_id,pull_request_id,linked_by_type) VALUES($1,$2,'member')`, issue, pr)
	t.Cleanup(func() { cleanupVCS(context.Background(), issue) })
	return conn, issue, pr
}

func snapshotResult() (vcs.PullRequestEvent, *vcs.TGitSnapshot) {
	no := false
	ev := vcs.PullRequestEvent{RepoOwner: "org", RepoName: "repo", Number: 42, Title: "MR", State: "open", Branch: "feature/test", HeadSHA: "head", CreatedAt: "2026-10-26T06:00:00Z", UpdatedAt: "2026-10-26T07:00:00Z"}
	snap := &vcs.TGitSnapshot{HeadSHA: "head", TargetSHA: "target", MRUpdatedAt: ev.UpdatedAt,
		CommitGate: vcs.TGitGate{State: "success", Blocked: &no}, CheckAggregate: vcs.TGitGate{State: "success", Blocked: &no}, HasConflicts: &no,
		Checks: []vcs.TGitCheck{{ID: 1, Context: "ci", State: "success"}}, Review: vcs.TGitReview{State: "approving"},
	}
	return ev, snap
}

func TestTGitSnapshotRefreshIsScopedAndThrottled(t *testing.T) {
	ctx := context.Background()
	conn, issue, pr := snapshotFixture(t)
	other := dbfx.Insert(t, "vcs_pull_request", testutil.Cols{
		"workspace_id": testWorkspaceID, "connection_id": conn, "provider": "tgit", "repo_owner": "org", "repo_name": "other", "pr_number": 42,
		"title": "Other MR", "state": "open", "html_url": "https://git.test/org/other/merge_requests/42", "head_sha": "head",
		"pr_created_at": "2026-10-26T06:00:00Z", "pr_updated_at": "2026-10-26T07:00:00Z",
	})
	dbfx.Exec(t, `INSERT INTO issue_vcs_pull_request(issue_id,pull_request_id,linked_by_type) VALUES($1,$2,'member')`, issue, other)
	r := newTGitSnapshotRefresher(testHandler)
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	r.fetch = func(_ context.Context, instance, token, owner, repo string, n int32) (vcs.PullRequestEvent, *vcs.TGitSnapshot, error) {
		calls.Add(1)
		if token != "tok" || instance != "https://git.test" {
			t.Error("wrong connection credentials")
		}
		ev, snap := snapshotResult()
		ev.RepoName = repo
		if repo == "repo" {
			close(entered)
			<-release
		} else {
			snap.Checks[0].State = "failure"
		}
		return ev, snap, nil
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); r.refresh(ctx, parseUUID(pr)) }()
	<-entered
	r.refresh(ctx, parseUUID(pr))
	close(release)
	wg.Wait()
	r.refresh(ctx, parseUUID(other))
	r.refresh(ctx, parseUUID(pr))
	if calls.Load() != 2 {
		t.Fatalf("duplicate refresh escaped throttle: %d", calls.Load())
	}
	got := listIssuePRsForTest(t, issue)
	if len(got.PullRequests) != 2 {
		t.Fatal(got)
	}
	for _, p := range got.PullRequests {
		if p.SnapshotAvailable == nil || !*p.SnapshotAvailable || p.Review == nil || p.Review.State != "approving" {
			t.Fatalf("snapshot missing: %+v", p)
		}
		if p.RepoName == "repo" && (p.ChecksRollup == nil || *p.ChecksRollup != "success" || p.HtmlURL != "http://git.test/org/repo/merge_requests/42") {
			t.Fatalf("wrong successful snapshot: %+v", p)
		}
		if p.RepoName == "other" && (p.ChecksRollup == nil || *p.ChecksRollup != "failure") {
			t.Fatalf("same SHA mixed repositories: %+v", p)
		}
	}
	if issueStatusForTest(t, issue) != "in_progress" {
		t.Fatal("checks passing moved the issue")
	}
}

func TestTGitSnapshotCannotOverwriteConcurrentWebhook(t *testing.T) {
	ctx := context.Background()
	_, issue, pr := snapshotFixture(t)
	r := newTGitSnapshotRefresher(testHandler)
	r.fetch = func(context.Context, string, string, string, string, int32) (vcs.PullRequestEvent, *vcs.TGitSnapshot, error) {
		dbfx.Exec(t, `UPDATE vcs_pull_request SET head_sha='new',pr_updated_at='2026-10-26T08:00:00Z' WHERE id=$1`, pr)
		ev, s := snapshotResult()
		return ev, s, nil
	}
	r.refresh(ctx, parseUUID(pr))
	row, err := testHandler.Queries.GetVCSPullRequestInWorkspace(ctx, dbGetVCS(pr))
	if err != nil {
		t.Fatal(err)
	}
	if row.HeadSha != "new" || row.SnapshotFetchedAt.Valid {
		t.Fatalf("stale snapshot overwrote webhook: %+v", row)
	}
	response := listIssuePRsForTest(t, issue).PullRequests[0]
	if response.SnapshotAvailable == nil || *response.SnapshotAvailable {
		t.Fatal("stale head became available")
	}
}

func TestTGitSnapshotFailureMarksStaleAndRecovers(t *testing.T) {
	ctx := context.Background()
	_, issue, pr := snapshotFixture(t)
	r := newTGitSnapshotRefresher(testHandler)
	r.fetch = func(context.Context, string, string, string, string, int32) (vcs.PullRequestEvent, *vcs.TGitSnapshot, error) {
		ev, s := snapshotResult()
		return ev, s, nil
	}
	r.refresh(ctx, parseUUID(pr))
	dbfx.Exec(t, `UPDATE vcs_pull_request SET snapshot_refresh_after=NULL WHERE id=$1`, pr)
	r.fetch = func(context.Context, string, string, string, string, int32) (vcs.PullRequestEvent, *vcs.TGitSnapshot, error) {
		return vcs.PullRequestEvent{}, nil, vcs.ErrUnauthorized
	}
	originalBus := testHandler.Bus
	testHandler.Bus = events.New()
	t.Cleanup(func() { testHandler.Bus = originalBus })
	notifiedStale := false
	testHandler.Bus.Subscribe(protocol.EventPullRequestUpdated, func(event events.Event) {
		if event.WorkspaceID == testWorkspaceID {
			notifiedStale = listIssuePRsForTest(t, issue).PullRequests[0].SnapshotStale
		}
	})
	r.refresh(ctx, parseUUID(pr))
	if !notifiedStale {
		t.Fatal("open cards were not notified after the snapshot became stale")
	}
	row, err := testHandler.Queries.GetVCSPullRequestInWorkspace(ctx, dbGetVCS(pr))
	if err != nil {
		t.Fatal(err)
	}
	if !row.SnapshotError || time.Until(row.SnapshotRefreshAfter.Time) < 14*time.Minute {
		t.Fatal("permission failure did not back off")
	}
	response := listIssuePRsForTest(t, issue).PullRequests[0]
	if !response.SnapshotStale || response.SnapshotAvailable == nil || !*response.SnapshotAvailable {
		t.Fatal("last snapshot must remain visibly stale")
	}
	dbfx.Exec(t, `UPDATE vcs_pull_request SET snapshot_refresh_after=NULL WHERE id=$1`, pr)
	r.fetch = func(context.Context, string, string, string, string, int32) (vcs.PullRequestEvent, *vcs.TGitSnapshot, error) {
		ev, s := snapshotResult()
		return ev, s, nil
	}
	r.refresh(ctx, parseUUID(pr))
	if listIssuePRsForTest(t, issue).PullRequests[0].SnapshotStale {
		t.Fatal("successful retry remained stale")
	}
}

func TestTGitSnapshotResponseRejectsWrongRevision(t *testing.T) {
	ev, s := snapshotResult()
	raw, _ := json.Marshal(s)
	for _, tc := range []struct{ head, updated string }{{"other", ev.UpdatedAt}, {ev.HeadSHA, "2026-10-26T08:00:00Z"}} {
		response := GitHubPullRequestResponse{Provider: "tgit"}
		applyTGitSnapshotResponse(&response, raw, pgtype.Timestamptz{Time: time.Now(), Valid: true}, false, tc.head, parseGHTime(tc.updated))
		if response.SnapshotAvailable == nil || *response.SnapshotAvailable || response.Review != nil {
			t.Fatal("outdated snapshot exposed")
		}
	}
}

func TestTGitSnapshotRefreshRetriesUnknownFailures(t *testing.T) {
	_, _, pr := snapshotFixture(t)
	r := newTGitSnapshotRefresher(testHandler)
	r.fetch = func(context.Context, string, string, string, string, int32) (vcs.PullRequestEvent, *vcs.TGitSnapshot, error) {
		return vcs.PullRequestEvent{}, nil, errors.New("temporary network error")
	}
	r.refresh(context.Background(), parseUUID(pr))
	row, err := testHandler.Queries.GetVCSPullRequestInWorkspace(context.Background(), dbGetVCS(pr))
	if err != nil {
		t.Fatal(err)
	}
	if !row.SnapshotError || !row.SnapshotRefreshAfter.Valid || time.Until(row.SnapshotRefreshAfter.Time) > 3*time.Minute {
		t.Fatal("unexpected retry deadline")
	}
}

func dbGetVCS(id string) db.GetVCSPullRequestInWorkspaceParams {
	return db.GetVCSPullRequestInWorkspaceParams{ID: parseUUID(id), WorkspaceID: parseUUID(testWorkspaceID)}
}

func TestTGitSnapshotWorkerStopsOnServerShutdown(t *testing.T) {
	_, _, pr := snapshotFixture(t)
	r := newTGitSnapshotRefresher(testHandler)
	entered, stopped := make(chan struct{}), make(chan struct{})
	r.fetch = func(ctx context.Context, _ string, _ string, _ string, _ string, _ int32) (vcs.PullRequestEvent, *vcs.TGitSnapshot, error) {
		close(entered)
		<-ctx.Done()
		close(stopped)
		return vcs.PullRequestEvent{}, nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.ctx = ctx
	r.started.Store(true)
	workerDone := make(chan struct{})
	go func() {
		r.worker()
		close(workerDone)
	}()
	r.Enqueue(parseUUID(pr))
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("snapshot worker did not start")
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("server shutdown did not cancel snapshot HTTP work")
	}
	<-workerDone
}

func TestTGitSnapshotDeadlineRecordsFailure(t *testing.T) {
	_, issue, pr := snapshotFixture(t)
	_, snapshot := snapshotResult()
	raw, _ := json.Marshal(snapshot)
	dbfx.Exec(t, `UPDATE vcs_pull_request SET api_snapshot=$2,snapshot_fetched_at=now() WHERE id=$1`, pr, raw)
	r := newTGitSnapshotRefresher(testHandler)
	r.fetch = func(ctx context.Context, _ string, _ string, _ string, _ string, _ int32) (vcs.PullRequestEvent, *vcs.TGitSnapshot, error) {
		<-ctx.Done()
		return vcs.PullRequestEvent{}, nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	r.refresh(ctx, parseUUID(pr))
	row, err := testHandler.Queries.GetVCSPullRequestInWorkspace(context.Background(), dbGetVCS(pr))
	if err != nil {
		t.Fatal(err)
	}
	if !row.SnapshotError || time.Until(row.SnapshotRefreshAfter.Time) < time.Minute {
		t.Fatal("request timeout did not record retry/staleness")
	}
	if !listIssuePRsForTest(t, issue).PullRequests[0].SnapshotStale {
		t.Fatal("timed-out snapshot still appears current")
	}
}

func TestTGitSnapshotOnlyNotifiesChangesAndStillReconcilesMerge(t *testing.T) {
	_, issue, pr := snapshotFixture(t)
	originalBus := testHandler.Bus
	testHandler.Bus = events.New()
	t.Cleanup(func() { testHandler.Bus = originalBus })
	var notifications int
	testHandler.Bus.Subscribe(protocol.EventPullRequestUpdated, func(event events.Event) {
		if event.WorkspaceID == testWorkspaceID {
			notifications++
		}
	})
	r := newTGitSnapshotRefresher(testHandler)
	ev, snapshot := snapshotResult()
	var fetchErr error
	r.fetch = func(context.Context, string, string, string, string, int32) (vcs.PullRequestEvent, *vcs.TGitSnapshot, error) {
		return ev, snapshot, fetchErr
	}
	refresh := func(want int) {
		t.Helper()
		notifications = 0
		dbfx.Exec(t, `UPDATE vcs_pull_request SET snapshot_refresh_after=NULL WHERE id=$1`, pr)
		r.refresh(context.Background(), parseUUID(pr))
		if notifications != want {
			t.Fatalf("got %d notifications, want %d", notifications, want)
		}
	}
	refresh(1)
	refresh(0)
	refresh(0)
	snapshot.Review.State = "approved"
	refresh(1)
	refresh(0)
	fetchErr = errors.New("temporary network failure")
	refresh(1)
	refresh(0)
	fetchErr = nil
	refresh(1)
	dbfx.Exec(t, `UPDATE vcs_pull_request SET snapshot_fetched_at=now()-interval '3 minutes' WHERE id=$1`, pr)
	refresh(1)
	refresh(0)
	ev.State = "merged"
	ev.UpdatedAt = "2026-10-26T08:00:00Z"
	snapshot.MRUpdatedAt = ev.UpdatedAt
	refresh(1)
	if got := issueStatusForTest(t, issue); got != "done" {
		t.Fatalf("merge did not reconcile linked issue: %s", got)
	}
}

func TestTGitSnapshotResponsePreservesGatesWithoutNamedChecks(t *testing.T) {
	for _, state := range []string{"pending", "failure"} {
		ev, snapshot := snapshotResult()
		snapshot.Checks = nil
		blocked := true
		snapshot.CommitGate = vcs.TGitGate{State: state, Blocked: &blocked}
		raw, _ := json.Marshal(snapshot)
		response := GitHubPullRequestResponse{Provider: "tgit"}
		applyTGitSnapshotResponse(&response, raw, pgtype.Timestamptz{Time: time.Now(), Valid: true}, false, ev.HeadSHA, parseGHTime(ev.UpdatedAt))
		conclusion := "pending"
		if state == "failure" {
			conclusion = "failed"
		}
		if response.ChecksTotal != 0 || response.ChecksRollup == nil || *response.ChecksRollup != state || response.ChecksConclusion == nil || *response.ChecksConclusion != conclusion {
			t.Fatalf("gate %s was lost without named checks: %+v", state, response)
		}
	}
}

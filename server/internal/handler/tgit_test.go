package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestTGitMRCardLifecycle(t *testing.T) {
	ctx := context.Background()
	box := withVCSBox(t)
	connID := seedVCSConnection(t, ctx, box, "tgit", "https://git.test")
	issueID := dbfx.Issue(t, "TGit lifecycle", testutil.Cols{"status": "in_progress"})
	t.Cleanup(func() { cleanupVCS(ctx, issueID) })
	var issue IssueResponse
	testutil.Call(t, testHandler.GetIssue, withURLParam(newRequest("GET", "/api/issues/"+issueID, nil), "id", issueID)).Want(http.StatusOK).JSON(&issue)

	attrs := map[string]any{
		"iid": 42, "title": "WIP: Fix " + issue.Identifier,
		"description": "", "source_branch": "feature/fix",
		"created_at":  "2026-10-26T06:00:00+0000",
		"source":      map[string]any{"web_url": "http://git.test/contributor/fork"},
		"target":      map[string]any{"web_url": "http://git.test/org/sub/widget"},
		"url":         "http://git.test/org/sub/widget/merge_requests/42",
		"last_commit": map[string]any{"id": "head-42"},
	}
	send := func(state, action, updated, token string, code int) {
		t.Helper()
		attrs["state"], attrs["action"], attrs["updated_at"] = state, action, updated
		raw, err := json.Marshal(map[string]any{
			"object_kind": "merge_request", "object_attributes": attrs,
			"user": map[string]any{"username": "event-actor"},
		})
		if err != nil {
			t.Fatal(err)
		}
		testutil.Call(t, testHandler.HandleVCSWebhook, vcsWebhookReq(connID, map[string]string{
			"X-Event": "Merge Request Hook", "X-Token": token,
		}, raw)).Want(code)
	}

	send("opened", "open", "2026-10-26T07:00:00+0000", "wrong", http.StatusUnauthorized)
	if got := listIssuePRsForTest(t, issueID); len(got.PullRequests) != 0 {
		t.Fatal("unauthenticated event created a card")
	}
	for _, step := range []struct{ state, action, updated, want string }{
		{"opened", "open", "2026-10-26T07:00:00+0000", "draft"},
		{"opened", "update", "2026-10-26T07:10:00+0000", "open"},
		{"closed", "close", "2026-10-26T07:20:00+0000", "closed"},
		{"opened", "reopen", "2026-10-26T07:30:00+0000", "open"},
		{"merged", "merge", "2026-10-26T07:40:00+0000", "merged"},
		{"opened", "update", "2026-10-26T07:35:00+0000", "merged"},
		{"opened", "update", "invalid", "merged"},
	} {
		send(step.state, step.action, step.updated, vcsTestSecret, http.StatusAccepted)
		got := listIssuePRsForTest(t, issueID)
		if len(got.PullRequests) != 1 {
			t.Fatalf("expected one MR card, got %+v", got.PullRequests)
		}
		pr := got.PullRequests[0]
		if pr.Provider != "tgit" || pr.State != step.want || pr.RepoOwner != "org/sub" || pr.RepoName != "widget" || pr.Number != 42 {
			t.Fatalf("after %s at %s: %+v", step.action, step.updated, pr)
		}
		if pr.ChecksConclusion != nil || pr.ChecksTotal != 0 || pr.AuthorLogin != nil {
			t.Fatalf("missing checks or MR author must not be invented: %+v", pr)
		}
		attrs["title"] = "Fix " + issue.Identifier
	}
	manualIssueID := dbfx.Issue(t, "TGit manual link")
	t.Cleanup(func() { cleanupVCS(ctx, manualIssueID) })
	for _, link := range []string{
		"https://git.test/org/sub/widget/-/merge_requests/42",
		"http://git.test/org/sub/widget/merge_requests/42",
	} {
		req := withURLParam(newRequest("POST", "/api/issues/"+manualIssueID+"/pull-requests", map[string]any{"url": link}), "id", manualIssueID)
		var linked issuePullRequestsBodyForTest
		testutil.Call(t, testHandler.LinkIssuePullRequest, req).Want(http.StatusOK).JSON(&linked)
		if len(linked.PullRequests) != 1 || linked.PullRequests[0].Number != 42 {
			t.Fatalf("URL alias did not resolve the existing MR: %s", link)
		}
	}
	for _, link := range []string{
		"https://other.test/org/sub/widget/-/merge_requests/42",
		"https://git.test/org/sub/other/-/merge_requests/42",
	} {
		req := withURLParam(newRequest("POST", "/api/issues/"+manualIssueID+"/pull-requests", map[string]any{"url": link}), "id", manualIssueID)
		testutil.Call(t, testHandler.LinkIssuePullRequest, req).Want(http.StatusNotFound)
	}
	if _, found, err := testHandler.findPullRequestForLink(ctx, parseUUID("00000000-0000-0000-0000-000000000000"), LinkIssuePullRequestRequest{
		URL: "https://git.test/org/sub/widget/-/merge_requests/42",
	}); err != nil || found {
		t.Fatalf("TGit lookup crossed the workspace boundary: found=%v err=%v", found, err)
	}
}

func TestTGitURLAliasesDoNotChangeGitLabMatching(t *testing.T) {
	ctx := context.Background()
	connID := seedVCSConnection(t, ctx, withVCSBox(t), "gitlab", "https://gitlab.test")
	t.Cleanup(func() { cleanupVCS(ctx, "") })
	dbfx.Insert(t, "vcs_pull_request", testutil.Cols{
		"workspace_id": testWorkspaceID, "connection_id": connID, "provider": "gitlab",
		"repo_owner": "org", "repo_name": "widget", "pr_number": 42,
		"title": "GitLab MR", "state": "open", "html_url": "http://gitlab.test/org/widget/merge_requests/42",
		"pr_created_at": "2026-10-26T06:00:00Z", "pr_updated_at": "2026-10-26T06:00:00Z",
	})
	for _, tc := range []struct {
		url   string
		found bool
	}{
		{"http://gitlab.test/org/widget/merge_requests/42", true},
		{"https://gitlab.test/org/widget/-/merge_requests/42", false},
	} {
		_, found, err := testHandler.findPullRequestForLink(ctx, parseUUID(testWorkspaceID), LinkIssuePullRequestRequest{URL: tc.url})
		if err != nil || found != tc.found {
			t.Fatalf("GitLab URL matching changed for %s: found=%v err=%v", tc.url, found, err)
		}
	}
}

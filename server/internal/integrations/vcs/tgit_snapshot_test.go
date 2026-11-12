package vcs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type tgitFixture struct {
	missingChecks bool
	gate          string
	blocked       *bool
	checkState    string
	empty         bool
	sourceMoves   bool
	targetMoves   bool
	duplicatePage bool
	wrongConflict bool
}

func boolRef(v bool) *bool { return &v }

func tgitSnapshotServer(t *testing.T, f *tgitFixture) *httptest.Server {
	t.Helper()
	mrReads, branchReads := 0, 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "token" {
			t.Error("missing token")
		}
		w.Header().Set("Content-Type", "application/json")
		var out any
		switch {
		case strings.HasSuffix(r.URL.Path, "/merge_request/iid/42"):
			mrReads++
			head := "head"
			if f.sourceMoves && mrReads > 1 {
				head = "new-head"
			}
			out = map[string]any{"id": 99, "iid": 42, "title": "Fix WORK-47", "description": "", "state": "opened", "source_commit": head, "source_branch": "feature/test", "target_branch": "release/main", "created_at": "2026-10-26T06:00:00+0000", "updated_at": "2026-10-26T07:00:00+0000", "commit_check_state": f.gate, "commit_check_block": f.blocked}
		case strings.Contains(r.URL.Path, "/repository/branches/"):
			branchReads++
			head := "target"
			if f.targetMoves && branchReads > 1 {
				head = "new-target"
			}
			out = map[string]any{"commit": map[string]any{"id": head}}
		case strings.HasSuffix(r.URL.Path, "/commits/head/status"):
			if f.missingChecks {
				w.WriteHeader(404)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "404 commit check data not found"})
				return
			}
			if r.URL.Query().Get("target_branch") != "release/main" {
				t.Error("checks not scoped to target branch")
			}
			checks := []map[string]any{}
			total := 3
			if f.empty {
				total = 0
			} else if r.URL.Query().Get("page") == "1" {
				checks = append(checks, map[string]any{"id": 1, "context": "ci/shared", "state": f.checkState, "target_branches": []string{"release/main"}, "target_url": "https://ci.test/1"}, map[string]any{"id": 2, "context": "ci/shared", "state": "success", "target_branches": []string{"release/main"}, "target_url": "javascript:bad()"})
			} else {
				id := 3
				if f.duplicatePage {
					id = 1
				}
				checks = append(checks, map[string]any{"id": id, "context": "another-target", "state": "failure", "target_branches": []string{"other"}})
			}
			out = map[string]any{"sha": "head", "state": "success", "block": false, "total_count": total, "statuses": checks}
		case strings.HasSuffix(r.URL.Path, "/merge_request/99/review"):
			out = map[string]any{"state": "approving", "approver_rule": 1, "necessary_approver_rule": 1, "reviewers": []map[string]any{{"username": "reviewer", "type": "necessary", "review_state": "approved"}}}
		case strings.HasSuffix(r.URL.Path, "/merge_request/99/merge_conflict_check"):
			head := "head"
			if f.wrongConflict {
				head = "other-head"
			}
			out = map[string]any{"has_conflicts": false, "base_commit": head, "target_commit": "target"}
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(404)
			return
		}
		if err := json.NewEncoder(w).Encode(out); err != nil {
			t.Error(err)
		}
	}))
}

func TestTGitSnapshotCIAndReviewAreIndependent(t *testing.T) {
	for _, tc := range []struct {
		name, gate, state, want string
		blocked                 *bool
		empty                   bool
	}{
		{"passed checks but MR gate blocks", "success", "success", "pending", boolRef(true), false},
		{"pending gate", "pending", "success", "pending", boolRef(false), false},
		{"missing gate", "", "success", "pending", nil, false},
		{"pipeline approval", "success", "need_approve", "pending", boolRef(false), false},
		{"failed check", "success", "failure", "failure", boolRef(false), false},
		{"all checks and gate pass", "success", "success", "success", boolRef(false), false},
		{"no checks", "success", "success", "", boolRef(false), true},
		{"empty but blocked", "success", "success", "pending", boolRef(true), true},
		{"empty but failed", "failure", "success", "failure", boolRef(false), true},
		{"empty unknown gate", "", "success", "pending", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &tgitFixture{gate: tc.gate, blocked: tc.blocked, checkState: tc.state, empty: tc.empty}
			srv := tgitSnapshotServer(t, f)
			defer srv.Close()
			_, snap, err := FetchTGitSnapshot(context.Background(), srv.URL, "token", "group/sub", "repo", 42)
			if err != nil {
				t.Fatal(err)
			}
			if snap.CheckState() != tc.want {
				t.Fatalf("CI=%q want=%q", snap.CheckState(), tc.want)
			}
			if snap.Review.State != "approving" || len(snap.Review.Reviewers) != 1 {
				t.Fatalf("approval must use API aggregate, not the displayed reviewer vote: %+v", snap.Review)
			}
			if snap.HasConflicts == nil || *snap.HasConflicts {
				t.Fatal("CI/review block must not imply a merge conflict")
			}
			if !tc.empty && (len(snap.Checks) != 2 || snap.Checks[0].Context != snap.Checks[1].Context || snap.Checks[1].TargetURL != "") {
				t.Fatalf("lost checks, mixed target branches or unsafe URL: %+v", snap.Checks)
			}
			if snap.HeadSHA != "head" || snap.TargetSHA != "target" || snap.MRUpdatedAt != "2026-10-26T07:00:00Z" {
				t.Fatalf("bad revision binding: %+v", snap)
			}
		})
	}
}

func TestTGitSnapshotRejectsRacesAndIncompletePages(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fixture  tgitFixture
		revision bool
	}{
		{"source push", tgitFixture{sourceMoves: true}, true},
		{"target push", tgitFixture{targetMoves: true}, true},
		{"stale conflict computation", tgitFixture{wrongConflict: true}, true},
		{"pagination moved", tgitFixture{duplicatePage: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.fixture
			f.gate = "success"
			f.blocked = boolRef(false)
			f.checkState = "success"
			srv := tgitSnapshotServer(t, &f)
			defer srv.Close()
			_, snap, err := FetchTGitSnapshot(context.Background(), srv.URL, "token", "group/sub", "repo", 42)
			if err == nil || snap != nil {
				t.Fatalf("inconsistent response was accepted: %+v", snap)
			}
			if tc.revision && !errors.Is(err, ErrTGitRevisionChanged) {
				t.Fatalf("wrong error: %v", err)
			}
		})
	}
}

func TestTGitSnapshotWithoutCIStillHasReviewAndConflict(t *testing.T) {
	f := &tgitFixture{gate: "success", blocked: boolRef(false), missingChecks: true}
	srv := tgitSnapshotServer(t, f)
	defer srv.Close()
	_, s, err := FetchTGitSnapshot(context.Background(), srv.URL, "token", "group/sub", "repo", 42)
	if err != nil {
		t.Fatal(err)
	}
	if s.CheckState() != "" || len(s.Checks) != 0 || s.Review.State != "approving" || s.HasConflicts == nil {
		t.Fatalf("bad no-CI snapshot: %+v", s)
	}
}

func TestTGitSnapshotPreservesRateLimitDelay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	_, _, err := FetchTGitSnapshot(context.Background(), srv.URL, "token", "org", "repo", 42)
	var limited *TGitRateLimitError
	if !errors.As(err, &limited) || limited.RetryAfter != 10*time.Minute {
		t.Fatalf("rate limit lost: %v", err)
	}
}

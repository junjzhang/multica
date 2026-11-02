package vcs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"
)

var ErrTGitRevisionChanged = errors.New("tgit: MR or target revision changed during refresh")

type TGitRateLimitError struct{ RetryAfter time.Duration }

func (e *TGitRateLimitError) Error() string { return "tgit: API rate limit reached" }

type TGitCheck struct {
	ID             int64    `json:"id"`
	Context        string   `json:"context"`
	State          string   `json:"state"`
	TargetURL      string   `json:"target_url"`
	TargetBranches []string `json:"target_branches,omitempty"`
}

type TGitReviewer struct {
	Username    string `json:"username"`
	Type        string `json:"type"`
	ReviewState string `json:"review_state"`
}

type TGitReview struct {
	State                 string         `json:"state"`
	Reviewers             []TGitReviewer `json:"reviewers"`
	ApproverRule          int            `json:"approver_rule"`
	NecessaryApproverRule int            `json:"necessary_approver_rule"`
}

type TGitGate struct {
	State   string `json:"state"`
	Blocked *bool  `json:"blocked"`
}

type TGitSnapshot struct {
	CommitGate     TGitGate    `json:"commit_gate"`
	CheckAggregate TGitGate    `json:"check_aggregate"`
	HeadSHA        string      `json:"head_sha"`
	TargetSHA      string      `json:"target_sha"`
	MRUpdatedAt    string      `json:"mr_updated_at"`
	Checks         []TGitCheck `json:"checks"`
	Review         TGitReview  `json:"review"`
	HasConflicts   *bool       `json:"has_conflicts"`
}

type tgitSnapshotMR struct {
	ID               int64  `json:"id"`
	IID              int32  `json:"iid"`
	Title            string `json:"title"`
	Description      string `json:"description"`
	State            string `json:"state"`
	WorkInProgress   bool   `json:"work_in_progress"`
	SourceCommit     string `json:"source_commit"`
	SourceBranch     string `json:"source_branch"`
	TargetBranch     string `json:"target_branch"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
	CommitCheckState string `json:"commit_check_state"`
	CommitCheckBlock *bool  `json:"commit_check_block"`
}

type tgitHTTPError struct {
	status  int
	message string
}

func (e *tgitHTTPError) Error() string { return fmt.Sprintf("tgit: HTTP %d", e.status) }

type tgitClient struct{ base, token string }

func (c tgitClient) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v3"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	req.Header.Set("Accept", "application/json")
	client := *httpClient
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("tgit: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		delay := 2 * time.Minute
		if seconds, err := time.ParseDuration(resp.Header.Get("Retry-After") + "s"); err == nil && seconds > 0 {
			delay = seconds
		} else if until, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil && time.Until(until) > 0 {
			delay = time.Until(until)
		}
		return &TGitRateLimitError{RetryAfter: delay}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		var body struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 512)).Decode(&body)
		return &tgitHTTPError{status: resp.StatusCode, message: body.Message}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

func (c tgitClient) branchHead(ctx context.Context, project, branch string) (string, error) {
	var data struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	if err := c.get(ctx, project+"/repository/branches/"+url.PathEscape(branch), &data); err != nil {
		return "", err
	}
	if data.Commit.ID == "" {
		return "", errors.New("tgit: target branch has no commit")
	}
	return data.Commit.ID, nil
}

// FetchTGitSnapshot only reads an already-known MR. Every result is bound to a
// stable source/target pair; the MR detail's target_commit can be an old base.
func FetchTGitSnapshot(ctx context.Context, instanceURL, token, owner, repo string, number int32) (PullRequestEvent, *TGitSnapshot, error) {
	u, err := url.Parse(instanceURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return PullRequestEvent{}, nil, errors.New("tgit: invalid instance URL")
	}
	c := tgitClient{base: NormalizeInstanceURL(instanceURL), token: token}
	project := "/projects/" + url.PathEscape(owner+"/"+repo)
	mrPath := fmt.Sprintf("%s/merge_request/iid/%d", project, number)
	var mr tgitSnapshotMR
	if err := c.get(ctx, mrPath, &mr); err != nil {
		return PullRequestEvent{}, nil, err
	}
	if mr.ID <= 0 || mr.IID != number || mr.SourceCommit == "" || mr.TargetBranch == "" {
		return PullRequestEvent{}, nil, errors.New("tgit: incomplete MR response")
	}
	target, err := c.branchHead(ctx, project, mr.TargetBranch)
	if err != nil {
		return PullRequestEvent{}, nil, err
	}
	checks, aggregate, blocked, err := c.checks(ctx, project, mr.SourceCommit, mr.TargetBranch)
	if err != nil {
		return PullRequestEvent{}, nil, err
	}
	var review TGitReview
	if err := c.get(ctx, fmt.Sprintf("%s/merge_request/%d/review", project, mr.ID), &review); err != nil {
		return PullRequestEvent{}, nil, err
	}
	var conflict struct {
		HasConflicts *bool  `json:"has_conflicts"`
		BaseCommit   string `json:"base_commit"`
		TargetCommit string `json:"target_commit"`
	}
	if mr.State == "opened" || mr.State == "reopened" {
		if err := c.get(ctx, fmt.Sprintf("%s/merge_request/%d/merge_conflict_check", project, mr.ID), &conflict); err != nil {
			return PullRequestEvent{}, nil, err
		}
		if conflict.BaseCommit != mr.SourceCommit || conflict.TargetCommit != target {
			return PullRequestEvent{}, nil, ErrTGitRevisionChanged
		}
	}
	var after tgitSnapshotMR
	if err := c.get(ctx, mrPath, &after); err != nil {
		return PullRequestEvent{}, nil, err
	}
	targetAfter, err := c.branchHead(ctx, project, mr.TargetBranch)
	if err != nil {
		return PullRequestEvent{}, nil, err
	}
	if !reflect.DeepEqual(mr, after) || target != targetAfter {
		return PullRequestEvent{}, nil, ErrTGitRevisionChanged
	}
	state, err := tgitMRState(mr.State, mr.Title, mr.WorkInProgress)
	if err != nil {
		return PullRequestEvent{}, nil, err
	}
	created, err := parseTGitTime(mr.CreatedAt)
	if err != nil {
		return PullRequestEvent{}, nil, err
	}
	updated, err := parseTGitTime(mr.UpdatedAt)
	if err != nil {
		return PullRequestEvent{}, nil, err
	}
	pr := PullRequestEvent{RepoOwner: owner, RepoName: repo, Number: number, Title: mr.Title, Body: mr.Description,
		State: state, HTMLURL: c.base + "/" + owner + "/" + repo + fmt.Sprintf("/-/merge_requests/%d", number),
		Branch: mr.SourceBranch, HeadSHA: mr.SourceCommit, CreatedAt: created, UpdatedAt: updated}
	return pr, &TGitSnapshot{
		HeadSHA: mr.SourceCommit, TargetSHA: target, MRUpdatedAt: updated,
		Checks: checks, CommitGate: TGitGate{State: mr.CommitCheckState, Blocked: mr.CommitCheckBlock},
		CheckAggregate: TGitGate{State: aggregate, Blocked: blocked}, Review: review, HasConflicts: conflict.HasConflicts,
	}, nil
}

func (c tgitClient) checks(ctx context.Context, project, head, target string) ([]TGitCheck, string, *bool, error) {
	checks := []TGitCheck{}
	seen := map[int64]bool{}
	total, state := -1, ""
	var blocked *bool
	for page := 1; page <= 100; page++ {
		var data struct {
			SHA      string      `json:"sha"`
			State    string      `json:"state"`
			Block    *bool       `json:"block"`
			Total    *int        `json:"total_count"`
			Statuses []TGitCheck `json:"statuses"`
		}
		path := fmt.Sprintf("%s/commits/%s/status?target_branch=%s&per_page=100&page=%d", project, url.PathEscape(head), url.QueryEscape(target), page)
		if err := c.get(ctx, path, &data); err != nil {
			var apiErr *tgitHTTPError
			if page == 1 && errors.As(err, &apiErr) && apiErr.status == 404 && strings.TrimPrefix(apiErr.message, "404 ") == "commit check data not found" {
				return checks, "", nil, nil
			}
			return nil, "", nil, err
		}
		if data.SHA != head || data.Total == nil || *data.Total < 0 {
			return nil, "", nil, errors.New("tgit: incomplete or mismatched checks response")
		}
		if page == 1 {
			total, state, blocked = *data.Total, data.State, data.Block
		}
		if total != *data.Total || state != data.State || !reflect.DeepEqual(blocked, data.Block) {
			return nil, "", nil, ErrTGitRevisionChanged
		}
		for _, check := range data.Statuses {
			if check.ID <= 0 || seen[check.ID] {
				return nil, "", nil, errors.New("tgit: duplicate or invalid check identity")
			}
			seen[check.ID] = true
			if len(check.TargetBranches) > 0 && !containsTGitBranch(check.TargetBranches, target) {
				continue
			}
			if parsed, err := url.Parse(check.TargetURL); err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil {
				check.TargetURL = ""
			}
			checks = append(checks, check)
		}
		if len(seen) == total {
			return checks, state, blocked, nil
		}
		if len(seen) > total || len(data.Statuses) == 0 {
			return nil, "", nil, errors.New("tgit: incomplete checks pagination")
		}
	}
	return nil, "", nil, errors.New("tgit: checks pagination limit exceeded")
}

func containsTGitBranch(branches []string, target string) bool {
	for _, branch := range branches {
		if branch == target {
			return true
		}
	}
	return false
}

func (s TGitSnapshot) CheckState() string {
	checks, aggregate, blocked, gate, gateBlocked := s.Checks, s.CheckAggregate.State, s.CheckAggregate.Blocked, s.CommitGate.State, s.CommitGate.Blocked
	failed, pending := false, false
	for _, c := range checks {
		switch c.State {
		case "success":
		case "failure", "failed", "error", "canceled", "cancelled":
			failed = true
		default:
			pending = true
		}
	}
	if failed || gate == "failure" || gate == "failed" || gate == "error" || aggregate == "failure" || aggregate == "error" {
		return "failure"
	}
	if len(checks) == 0 && gate == "success" && gateBlocked != nil && !*gateBlocked && aggregate == "" && blocked == nil {
		return ""
	}
	if pending || blocked == nil || *blocked || gateBlocked == nil || *gateBlocked || gate != "success" || aggregate != "success" {
		return "pending"
	}
	if len(checks) == 0 {
		return ""
	}
	return "success"
}

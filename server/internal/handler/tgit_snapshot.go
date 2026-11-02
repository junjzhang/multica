package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/vcs"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type TGitSnapshotRefresher struct {
	h       *Handler
	ctx     context.Context
	queue   chan pgtype.UUID
	once    sync.Once
	started atomic.Bool
	fetch   func(context.Context, string, string, string, string, int32) (vcs.PullRequestEvent, *vcs.TGitSnapshot, error)
}

func newTGitSnapshotRefresher(h *Handler) *TGitSnapshotRefresher {
	return &TGitSnapshotRefresher{h: h, queue: make(chan pgtype.UUID, 256), fetch: vcs.FetchTGitSnapshot}
}

func (r *TGitSnapshotRefresher) Start(ctx context.Context) {
	if r == nil || r.h.VCSSecretBox == nil || !r.h.isVCSAvailable() {
		return
	}
	r.once.Do(func() {
		r.ctx = ctx
		r.started.Store(true)
		for range 4 {
			go r.worker()
		}
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			r.sweep(ctx)
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					r.sweep(ctx)
				}
			}
		}()
	})
}

func (r *TGitSnapshotRefresher) Enqueue(id pgtype.UUID) {
	if r == nil || !r.started.Load() || r.ctx.Err() != nil {
		return
	}
	select {
	case r.queue <- id:
	default:
	}
}

func (r *TGitSnapshotRefresher) sweep(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ids, err := r.h.Queries.ListTGitSnapshotCandidates(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("tgit: snapshot sweep failed", "err", err)
		}
		return
	}
	for _, id := range ids {
		r.Enqueue(id)
	}
}

func (r *TGitSnapshotRefresher) worker() {
	for {
		select {
		case <-r.ctx.Done():
			return
		case id := <-r.queue:
			ctx, cancel := context.WithTimeout(r.ctx, 40*time.Second)
			r.refresh(ctx, id)
			cancel()
		}
	}
}

func (r *TGitSnapshotRefresher) refresh(ctx context.Context, id pgtype.UUID) {
	pr, err := r.h.Queries.ClaimTGitSnapshot(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	if err != nil {
		slog.Warn("tgit: snapshot claim failed", "err", err)
		return
	}
	err = r.apply(ctx, pr)
	if err == nil || errors.Is(ctx.Err(), context.Canceled) || (r.ctx != nil && r.ctx.Err() != nil) {
		return
	}
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	delay := 2 * time.Minute
	var rateLimit *vcs.TGitRateLimitError
	switch {
	case errors.As(err, &rateLimit):
		delay = rateLimit.RetryAfter
	case errors.Is(err, vcs.ErrTGitRevisionChanged):
		delay = 5 * time.Second
	case errors.Is(err, vcs.ErrUnauthorized):
		delay = 15 * time.Minute
	}
	changed, saveErr := r.h.Queries.FailTGitSnapshot(saveCtx, db.FailTGitSnapshotParams{
		ID: pr.ID, ClaimUntil: pr.SnapshotRefreshAfter, RetryAt: pgtype.Timestamptz{Time: time.Now().Add(delay), Valid: true},
	})
	if saveErr != nil {
		slog.Warn("tgit: snapshot failure record failed", "err", saveErr)
	} else if changed > 0 && !pr.SnapshotError {
		r.h.publishTGitSnapshotChanged(saveCtx, pr)
	}
	slog.Warn("tgit: snapshot refresh failed", "pr_id", uuidToString(pr.ID), "err", err)
}

func (r *TGitSnapshotRefresher) apply(ctx context.Context, pr db.VcsPullRequest) error {
	conn, err := r.h.Queries.GetVCSConnectionByID(ctx, pr.ConnectionID)
	if err != nil {
		return err
	}
	if conn.Provider != "tgit" || conn.WorkspaceID != pr.WorkspaceID {
		return errors.New("tgit: connection scope mismatch")
	}
	token, err := r.h.openVCSSecret(conn.AccessTokenEncrypted)
	if err != nil {
		return err
	}
	event, snapshot, err := r.fetch(ctx, conn.InstanceUrl, token, pr.RepoOwner, pr.RepoName, pr.PrNumber)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	updated, err := r.h.Queries.ApplyTGitSnapshot(ctx, db.ApplyTGitSnapshotParams{
		ID: pr.ID, Snapshot: raw, HeadSha: snapshot.HeadSHA, RemoteUpdatedAt: parseGHTime(snapshot.MRUpdatedAt),
		Title: event.Title, State: event.State, Branch: ptrToText(strPtrOrNil(event.Branch)),
		ExpectedHead: pr.HeadSha, ExpectedUpdatedAt: pr.PrUpdatedAt, ClaimUntil: pr.SnapshotRefreshAfter,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return vcs.ErrTGitRevisionChanged
	}
	if err != nil {
		return err
	}
	metadataChanged := pr.Title != updated.Title || pr.State != updated.State || pr.HeadSha != updated.HeadSha ||
		pr.Branch != updated.Branch || !pr.PrUpdatedAt.Time.Equal(updated.PrUpdatedAt.Time)
	if metadataChanged {
		event.HTMLURL = pr.HtmlUrl
		r.h.reconcileVCSPullRequest(ctx, conn, event, updated, pr.State)
		return nil
	}
	var previous, current vcs.TGitSnapshot
	if !pr.SnapshotFetchedAt.Valid || pr.SnapshotError || time.Since(pr.SnapshotFetchedAt.Time) > 2*time.Minute ||
		json.Unmarshal(pr.ApiSnapshot, &previous) != nil || json.Unmarshal(raw, &current) != nil || !reflect.DeepEqual(previous, current) {
		r.h.publishTGitSnapshotChanged(ctx, updated)
	}
	return nil
}

func (h *Handler) publishTGitSnapshotChanged(ctx context.Context, pr db.VcsPullRequest) {
	issueIDs, err := h.Queries.ListIssueIDsForVCSPullRequest(ctx, pr.ID)
	if err != nil {
		slog.Warn("tgit: snapshot notification failed", "err", err)
		return
	}
	for _, issueID := range issueIDs {
		h.publishIssuePullRequestsChanged(uuidToString(pr.WorkspaceID), issueID)
	}
}

func applyTGitSnapshotResponse(resp *GitHubPullRequestResponse, raw []byte, fetched pgtype.Timestamptz, failed bool, head string, updated pgtype.Timestamptz) {
	if resp.Provider != "tgit" {
		return
	}
	available := false
	resp.SnapshotAvailable = &available
	var snapshot vcs.TGitSnapshot
	if !fetched.Valid || json.Unmarshal(raw, &snapshot) != nil || snapshot.HeadSHA == "" || snapshot.HeadSHA != head {
		return
	}
	remoteUpdated := parseGHTime(snapshot.MRUpdatedAt)
	if !remoteUpdated.Valid || !updated.Valid || !remoteUpdated.Time.Truncate(time.Microsecond).Equal(updated.Time) {
		return
	}
	available = true
	resp.SnapshotFetchedAt = timestampToPtr(fetched)
	resp.SnapshotStale = failed || time.Since(fetched.Time) > 2*time.Minute
	checkState := snapshot.CheckState()
	resp.ChecksRollup = strPtrOrNil(checkState)
	resp.ChecksTotal = int64(len(snapshot.Checks))
	resp.ChecksPassed, resp.ChecksFailed, resp.ChecksPending = 0, 0, 0
	resp.FailedCheckNames = []string{}
	for _, check := range snapshot.Checks {
		switch check.State {
		case "success":
			resp.ChecksPassed++
		case "failure", "failed", "error", "canceled", "cancelled":
			resp.ChecksFailed++
			resp.FailedCheckNames = append(resp.FailedCheckNames, check.Context)
		default:
			resp.ChecksPending++
		}
	}
	resp.ChecksRunning = resp.ChecksPending
	resp.ChecksConclusion = nil
	switch checkState {
	case "success":
		resp.ChecksConclusion = strPtrOrNil("passed")
	case "pending":
		resp.ChecksConclusion = strPtrOrNil("pending")
	case "failure":
		resp.ChecksConclusion = strPtrOrNil("failed")
	}
	if snapshot.HasConflicts != nil {
		value := "mergeable"
		if *snapshot.HasConflicts {
			value = "conflicting"
		}
		resp.Mergeable = &value
	}
	resp.Review = &snapshot.Review
	resp.CheckDetails = snapshot.Checks
	resp.CommitGate = &snapshot.CommitGate
}

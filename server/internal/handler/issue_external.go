package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// IssueCoordinatorInput is the explicit coordinator change accepted by
// UpdateIssue. Mentions and reassignment never produce one.
type IssueCoordinatorInput struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// MergeAuthorizationInput is the human-only merge-policy grant/revoke.
type MergeAuthorizationInput struct {
	Allowed       bool    `json:"allowed"`
	RepositoryURL *string `json:"repository_url"`
	TargetBranch  *string `json:"target_branch"`
}

var mergeTargetBranchPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,254}$`)

// validateCoordinator resolves the requested coordinator inside the issue's
// workspace. A coordinator is an identity, not a permission grant.
func (h *Handler) validateCoordinator(ctx context.Context, workspaceID string, input IssueCoordinatorInput) (pgtype.Text, pgtype.UUID, string) {
	switch input.Type {
	case "member":
		memberID, err := util.ParseUUID(strings.TrimSpace(input.ID))
		if err != nil {
			return pgtype.Text{}, pgtype.UUID{}, "coordinator.id is not a valid uuid"
		}
		wsUUID, err := util.ParseUUID(workspaceID)
		if err != nil {
			return pgtype.Text{}, pgtype.UUID{}, "invalid workspace"
		}
		// A member coordinator is addressed by USER id, matching the default
		// (the creating member's user id) and the CLI's --coordinator value.
		// Resolve membership by (user, workspace); a member row id is a
		// different identity and would store a value clients cannot resolve.
		if _, err := h.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
			UserID:      memberID,
			WorkspaceID: wsUUID,
		}); err != nil {
			return pgtype.Text{}, pgtype.UUID{}, "coordinator is not a member of this workspace"
		}
		return pgtype.Text{String: "member", Valid: true}, memberID, ""
	case "agent":
		agentID, err := util.ParseUUID(strings.TrimSpace(input.ID))
		if err != nil {
			return pgtype.Text{}, pgtype.UUID{}, "coordinator.id is not a valid uuid"
		}
		wsUUID, err := util.ParseUUID(workspaceID)
		if err != nil {
			return pgtype.Text{}, pgtype.UUID{}, "invalid workspace"
		}
		if _, err := h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{
			ID:          agentID,
			WorkspaceID: wsUUID,
		}); err != nil {
			return pgtype.Text{}, pgtype.UUID{}, "coordinator is not an agent in this workspace"
		}
		return pgtype.Text{String: "agent", Valid: true}, agentID, ""
	default:
		return pgtype.Text{}, pgtype.UUID{}, "coordinator.type must be 'member' or 'agent'"
	}
}

// lockCoordinatorAgent rejects an archived coordinator under the shared Agent
// reference lock. It runs inside the writing issue transaction, so an archive
// cannot commit between the handler's validation and this write.
func lockCoordinatorAgent(ctx context.Context, q *db.Queries, issue db.Issue) error {
	if issue.CoordinatorType.String != "agent" || !issue.CoordinatorID.Valid {
		return nil
	}
	agent, err := q.LockAgentForSharedWrite(ctx, db.LockAgentForSharedWriteParams{
		ID:          issue.CoordinatorID,
		WorkspaceID: issue.WorkspaceID,
	})
	if err != nil {
		return service.ErrCoordinatorAgentArchived
	}
	if agent.ArchivedAt.Valid {
		return service.ErrCoordinatorAgentArchived
	}
	return nil
}

// validateMergeAuthorization validates the stored policy. This fork has no
// merge executor: the API records and projects the human decision only and
// makes no claim to block arbitrary external `gh` shell operations.
func validateMergeAuthorization(input MergeAuthorizationInput) (repositoryURL, targetBranch string, msg string) {
	if !input.Allowed {
		return "", "", ""
	}
	rawURL := ""
	if input.RepositoryURL != nil {
		rawURL = strings.TrimSpace(*input.RepositoryURL)
	}
	if rawURL == "" {
		return "", "", "merge_authorization.repository_url is required when allowed"
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil {
		return "", "", "merge_authorization.repository_url must be an http(s) repository URL without credentials"
	}
	branch := ""
	if input.TargetBranch != nil {
		branch = strings.TrimSpace(*input.TargetBranch)
	}
	if branch == "" {
		return "", "", "merge_authorization.target_branch is required when allowed"
	}
	if !mergeTargetBranchPattern.MatchString(branch) || strings.Contains(branch, "..") {
		return "", "", "merge_authorization.target_branch is invalid"
	}
	return rawURL, branch, ""
}

// applyExternalIssueUpdate validates and stages coordinator/merge changes onto
// the native UpdateIssue statement so they share its revision and activity
// semantics. It returns false when it has already written an error response.
func (h *Handler) applyExternalIssueUpdate(w http.ResponseWriter, r *http.Request, req *UpdateIssueRequest, rawFields map[string]json.RawMessage, params *db.UpdateIssueParams, userID, workspaceID string) bool {
	if raw, present := rawFields["coordinator"]; present && string(raw) != "null" {
		if req.Coordinator == nil {
			writeError(w, http.StatusBadRequest, "coordinator must be an object with type and id")
			return false
		}
		coordinatorType, coordinatorID, msg := h.validateCoordinator(r.Context(), workspaceID, *req.Coordinator)
		if msg != "" {
			writeError(w, http.StatusBadRequest, msg)
			return false
		}
		params.CoordinatorType = coordinatorType
		params.CoordinatorID = coordinatorID
	}

	if raw, present := rawFields["merge_authorization"]; present && string(raw) != "null" {
		if req.MergeAuthorization == nil {
			writeError(w, http.StatusBadRequest, "merge_authorization must be an object")
			return false
		}
		// Granting the right to merge is a human decision. Machine
		// credentials (task tokens, external agent tokens, cloud nodes) are
		// refused even when they act for a human.
		if isMachineCredentialActor(r) {
			writeError(w, http.StatusForbidden, "merge authorization can only be granted by a human actor")
			return false
		}
		repositoryURL, targetBranch, msg := validateMergeAuthorization(*req.MergeAuthorization)
		if msg != "" {
			writeError(w, http.StatusBadRequest, msg)
			return false
		}
		params.MergeAuthorizationSet = true
		params.MergeAllowed = req.MergeAuthorization.Allowed
		params.MergeRepositoryUrl = pgtype.Text{String: repositoryURL, Valid: repositoryURL != ""}
		params.MergeTargetBranch = pgtype.Text{String: targetBranch, Valid: targetBranch != ""}
		// granted_by/at are server-set: the caller can never claim another
		// person granted the policy.
		params.MergeGrantedBy = parseUUID(userID)
		params.MergeGrantedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	}
	return true
}

// projectIssueExternalState fills the server-computed participant and mailbox
// projections on a detail/create/update response. Failed projections stay
// omitted rather than falsely clearing previously loaded client state.
func (h *Handler) projectIssueExternalState(ctx context.Context, issue db.Issue, resp *IssueResponse) {
	if resp == nil {
		return
	}
	if participants, err := h.Queries.ListIssueExternalParticipants(ctx, db.ListIssueExternalParticipantsParams{
		WorkspaceID: issue.WorkspaceID,
		IssueID:     issue.ID,
	}); err == nil {
		ids := make([]pgtype.UUID, 0, len(participants))
		for _, a := range participants {
			ids = append(ids, a.ID)
		}
		sessions, sessionCounts := h.externalSessionsByAgent(ctx, ids)
		out := make([]AgentResponse, 0, len(participants))
		for _, a := range participants {
			ar := h.agentToResponse(a)
			ar.ExternalSession = sessions[ar.ID]
			ar.ExternalSessionCount = sessionCounts[ar.ID]
			out = append(out, ar)
		}
		resp.ExternalParticipants = &out
	}
	if deliveries, err := h.Queries.ListIssueMailboxDeliveries(ctx, db.ListIssueMailboxDeliveriesParams{
		IssueID:     issue.ID,
		WorkspaceID: issue.WorkspaceID,
	}); err == nil {
		sessions := h.externalSessionsByID(ctx, mailboxDeliverySessionIDs(deliveries))
		out := make([]MailboxMessageResponse, 0, len(deliveries))
		for _, delivery := range deliveries {
			out = append(out, mailboxMessageToResponse(
				delivery,
				sessions[uuidToString(delivery.SourceSessionID)],
				sessions[uuidToString(delivery.TargetSessionID)],
			))
		}
		resp.MailboxDeliveries = &out
	}
	if bindings, err := h.Queries.ListIssueActiveSessionBindings(ctx, db.ListIssueActiveSessionBindingsParams{
		IssueID:     issue.ID,
		WorkspaceID: issue.WorkspaceID,
	}); err == nil {
		out := make([]AgentSessionBindingViewResponse, 0, len(bindings))
		for _, binding := range bindings {
			out = append(out, sessionBindingViewToResponse(sessionBindingView{
				BindingID:     binding.BindingID,
				AgentID:       binding.AgentID,
				IssueID:       binding.IssueID,
				SessionID:     binding.SessionID,
				CreatedAt:     binding.CreatedAt,
				Provider:      binding.Provider,
				HostID:        binding.HostID,
				HostSessionID: binding.HostSessionID,
				DisplayName:   binding.DisplayName,
				URL:           binding.Url,
				DaemonID:      binding.DaemonID,
				AgentArchived: binding.AgentArchived,
				OwnerIsMember: binding.OwnerIsMember,
			}))
		}
		resp.ExternalSessionBindings = &out
	}
}

// recordIssueExternalUpdateActivity records coordinator and merge-policy
// changes in the issue timeline. Best-effort: the write already committed.
func (h *Handler) recordIssueExternalUpdateActivity(ctx context.Context, prev, issue db.Issue, actorType, actorID string) {
	if coordinatorResponse(prev.CoordinatorType, prev.CoordinatorID) == nil &&
		coordinatorResponse(issue.CoordinatorType, issue.CoordinatorID) == nil {
		// no coordinator change and none recorded
	} else if uuidToString(prev.CoordinatorID) != uuidToString(issue.CoordinatorID) ||
		prev.CoordinatorType.String != issue.CoordinatorType.String {
		h.recordIssueActivity(ctx, issue, actorType, actorID, "issue_coordinator_changed", map[string]any{
			"from": coordinatorResponse(prev.CoordinatorType, prev.CoordinatorID),
			"to":   coordinatorResponse(issue.CoordinatorType, issue.CoordinatorID),
		})
	}
	if prev.MergeAllowed != issue.MergeAllowed ||
		uuidToString(prev.MergeGrantedBy) != uuidToString(issue.MergeGrantedBy) ||
		prev.MergeRepositoryUrl.String != issue.MergeRepositoryUrl.String ||
		prev.MergeTargetBranch.String != issue.MergeTargetBranch.String {
		h.recordIssueActivity(ctx, issue, actorType, actorID, "issue_merge_authorization_changed", map[string]any{
			"allowed":        issue.MergeAllowed,
			"repository_url": textToPtr(issue.MergeRepositoryUrl),
			"target_branch":  textToPtr(issue.MergeTargetBranch),
		})
	}
}

func (h *Handler) recordIssueActivity(ctx context.Context, issue db.Issue, actorType, actorID, action string, details map[string]any) {
	payload, _ := json.Marshal(details)
	activity, err := h.Queries.CreateActivity(ctx, db.CreateActivityParams{
		ActorSessionID: func() pgtype.UUID {
			actor, _ := ctx.Value(wakeupActorKey{}).(wakeupActor)
			id, _ := util.ParseUUID(actor.session)
			return id
		}(),
		ID:          dbid.NewV7(),
		WorkspaceID: issue.WorkspaceID,
		IssueID:     issue.ID,
		ActorType:   strToText(actorType),
		ActorID:     optionalActorUUID(actorID),
		Action:      action,
		Details:     payload,
	})
	if err != nil {
		return
	}
	h.publish(protocol.EventActivityCreated, uuidToString(issue.WorkspaceID), actorType, actorID, map[string]any{
		"issue_id": uuidToString(issue.ID),
		"entry": map[string]any{
			"type":       "activity",
			"id":         uuidToString(activity.ID),
			"actor_type": actorType,
			"actor_id":   actorID,
			"action":     activity.Action,
			"details":    json.RawMessage(payload),
			"created_at": timestampToString(activity.CreatedAt),
		},
	})
}

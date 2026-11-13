package handler

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/multica-ai/multica/server/internal/delegation"
	"github.com/multica-ai/multica/server/internal/issuecontract"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Contract maintenance belongs to the original requester, never a mutable
// coordinator or a return address. The note records agreement; it grants no API
// capability. Human membership and dedicated merge grants keep their own gates.
func (h *Handler) canMaintainContract(ctx context.Context, r *http.Request, q *db.Queries, issue db.Issue, state issuecontract.State, basis json.RawMessage) bool {
	t, id := h.resolveActor(r, requestUserID(r), uuidToString(issue.WorkspaceID))
	actor := contractActor(r, t, id)
	if sameContractEntity(actor, issuecontract.Actor{Type: issue.AssigneeType.String, ID: uuidToString(issue.AssigneeID)}) {
		return false
	}
	if version := state.Current(); version != nil && sameContractEntity(actor, version.Executor) {
		return false
	}
	if representative := contractRepresentative(basis); representative != nil && sameContractEntity(actor, *representative) {
		return false
	}
	for _, version := range state.Versions {
		if representative := contractRepresentative(version.Basis); representative != nil && sameContractEntity(actor, *representative) {
			return false
		}
	}
	if actor.Type == "member" {
		return !isMachineCredentialActor(r)
	}
	if actor.Type != "agent" || issue.CreatorType != actor.Type || uuidToString(issue.CreatorID) != actor.ID {
		return false
	}
	agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: issue.CreatorID, WorkspaceID: issue.WorkspaceID})
	if err != nil || agent.ArchivedAt.Valid {
		return false
	}
	originator := h.invokeOriginatorFromRequest(r, actor.Type, actor.ID)
	userID, err := util.ParseUUID(originator)
	if err != nil {
		return false
	}
	if _, err := q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: userID, WorkspaceID: issue.WorkspaceID}); err != nil {
		return false
	}
	switch r.Header.Get("X-Actor-Source") {
	case "external_agent":
		if agent.ExecutionKind != "external" || agent.OwnerID != userID || actor.SessionID == "" {
			return false
		}
		if issue.CreatorSessionID.Valid && actor.SessionID != uuidToString(issue.CreatorSessionID) {
			return false
		}
		for _, version := range state.Versions {
			if version.Author.Type == actor.Type && version.Author.ID == actor.ID && version.Author.SessionID != "" && version.Author.SessionID != actor.SessionID {
				return false
			}
		}
		if !issue.CreatorSessionID.Valid {
			pinned := false
			for _, version := range state.Versions {
				if version.Author.ID == actor.ID && version.Author.Type == actor.Type && version.Author.SessionID == actor.SessionID {
					pinned = true
				}
			}
			if !pinned && !contractActorMatches(ctx, q, issue, actor, issuecontract.Actor{Type: actor.Type, ID: actor.ID}) {
				return false
			}
		}
		return true
	case "task_token":
		if agent.ExecutionKind != "managed" || !issue.OriginID.Valid || (issue.OriginType.String != "agent_create" && issue.OriginType.String != "quick_create") {
			return false
		}
		taskID, err := util.ParseUUID(r.Header.Get("X-Task-ID"))
		if err != nil {
			return false
		}
		current, err := q.GetAgentTask(ctx, taskID)
		if err != nil || current.AgentID != agent.ID || isTerminalTaskStatus(current.Status) || current.OriginatorUserID != userID {
			return false
		}
		original, err := q.GetAgentTask(ctx, issue.OriginID)
		if err != nil || original.AgentID != agent.ID || !original.OriginatorUserID.Valid || original.OriginatorUserID != current.OriginatorUserID {
			return false
		}
		if current.ID == original.ID {
			if current.IssueID.Valid || current.ChatSessionID.Valid || current.AutopilotRunID.Valid {
				return delegation.ValidateTaskScope(ctx, q, issue.WorkspaceID, current) == nil
			}
			return true
		}
		// Scopeless quick-create runs do not share authority merely because
		// their three nullable business pointers happen to be empty.
		if !original.IssueID.Valid && !original.ChatSessionID.Valid && !original.AutopilotRunID.Valid {
			return false
		}
		if current.IssueID != original.IssueID || current.ChatSessionID != original.ChatSessionID || current.AutopilotRunID != original.AutopilotRunID {
			return false
		}
		return delegation.ValidateTaskScope(ctx, q, issue.WorkspaceID, current) == nil
	default:
		return false
	}
}

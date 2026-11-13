package handler

import (
	"errors"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueleader"
	"net/http"

	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func writeRecipientSessionError(w http.ResponseWriter, err error) bool {
	var recipientErr *service.RecipientSessionError
	if !errors.As(err, &recipientErr) {
		return false
	}
	status := http.StatusBadRequest
	if recipientErr.Conflict {
		status = http.StatusConflict
	}
	if recipientErr.Forbidden {
		status = http.StatusForbidden
	}
	writeError(w, status, recipientErr.Error())
	return true
}

func validateRecipientSessionInput(id *string, assignmentInput bool) error {
	if id != nil && !assignmentInput {
		return &service.RecipientSessionError{Message: "recipient_session_id requires an explicit assignee"}
	}
	return service.ValidateRecipientSessionID(id)
}

func (h *Handler) externalAssignmentInTx(r *http.Request, q *db.Queries, issue db.Issue, sessionID *string, dispatch bool, actorType, actorID string) error {
	if issue.AssigneeType.String == "squad" {
		squad, err := q.LockSquadForAutopilotAssignment(r.Context(), db.LockSquadForAutopilotAssignmentParams{ID: issue.AssigneeID, WorkspaceID: issue.WorkspaceID})
		if err != nil {
			return err
		}
		actor, session := pgtype.UUID{}, pgtype.UUID{}
		if isExternalAgentActor(r) {
			session, _ = externalActorSessionID(r)
			actor = parseUUID(r.Header.Get("X-Agent-ID"))
		}
		leader, err := issueleader.Ensure(r.Context(), q, issue, squad, actor, session, requestUserID(r))
		if err != nil {
			return &service.RecipientSessionError{Message: err.Error(), Conflict: errors.Is(err, issueleader.ErrConflict)}
		}
		if leader.SessionID.Valid {
			id := uuidToString(leader.SessionID)
			if sessionID != nil && *sessionID != id {
				return &service.RecipientSessionError{Message: "recipient session differs from task leader; use explicit handoff", Conflict: true}
			}
			sessionID = &id
		}
	}
	target, err := service.ResolveAssignmentTarget(r.Context(), q, issue)
	if err != nil {
		return err
	}
	if target.Agent.ID.Valid && !h.canInvokeAgent(r.Context(), target.Agent, actorType, actorID, h.invokeOriginatorFromRequest(r, actorType, actorID), uuidToString(issue.WorkspaceID)) {
		return &service.RecipientSessionError{Message: "you do not have permission to assign work to this executor", Forbidden: true}
	}
	return service.ExternalAssignment(r.Context(), q, issue, target, sessionID, dispatch && issue.Status != "backlog" && !issue.TriageState.Valid, actorType, actorID, requestUserID(r), h.cfg.AppURL)
}

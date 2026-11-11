package handler

import (
	"encoding/json"
	"github.com/multica-ai/multica/server/internal/issueleader"
	"net/http"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// A delivered request proves the external role without manufacturing a run.
// The current leader check matches managed evaluation's rotation boundary.
func (h *Handler) recordExternalSquadEvaluation(w http.ResponseWriter, r *http.Request, issue db.Issue, mailboxID, outcome, reason string) {
	if r.Header.Get("X-Actor-Source") != "external_agent" {
		writeError(w, http.StatusForbidden, "mailbox evaluations require the connected external agent identity")
		return
	}
	actorSession, ok := h.requireExternalActorSession(w, r)
	if !ok {
		return
	}
	id, ok := parseUUIDOrBadRequest(w, mailboxID, "mailbox_id (use --mailbox-id from the delivered request)")
	if !ok {
		return
	}
	message, err := h.Queries.GetAgentMailboxMessage(r.Context(), db.GetAgentMailboxMessageParams{ID: id, WorkspaceID: issue.WorkspaceID})
	if err != nil || uuidToString(message.RecipientAgentID) != r.Header.Get("X-Agent-ID") || message.TargetSessionID != actorSession {
		writeError(w, http.StatusForbidden, "mailbox request does not belong to this session")
		return
	}
	var payload protocol.ExternalIssueRequest
	if message.IssueID != issue.ID || message.DeliveryStatus != "delivered" ||
		(message.Kind != "issue_assignment" && message.Kind != "comment_mention") ||
		json.Unmarshal(message.Payload, &payload) != nil || payload.SquadID == "" || payload.Briefing == "" {
		writeError(w, http.StatusBadRequest, "a delivered squad leader request for this issue is required")
		return
	}
	squadID, ok := parseUUIDOrBadRequest(w, payload.SquadID, "squad_id")
	if !ok {
		return
	}
	squad, err := h.Queries.GetSquadInWorkspace(r.Context(), db.GetSquadInWorkspaceParams{ID: squadID, WorkspaceID: issue.WorkspaceID})
	leader, leaderErr := issueleader.ResolveIssueLeader(r.Context(), h.Queries, issue, squad)
	if err != nil || leaderErr != nil || leader.Agent.ID != message.RecipientAgentID || leader.SessionID != actorSession {
		writeError(w, http.StatusForbidden, "only the current squad leader can record evaluations")
		return
	}
	h.recordSquadEvaluation(w, r, issue, message.RecipientAgentID, map[string]string{
		"squad_id": payload.SquadID, "mailbox_id": mailboxID, "outcome": outcome, "reason": reason,
	})
}

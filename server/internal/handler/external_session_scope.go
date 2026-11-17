package handler

import (
	"context"
	"github.com/multica-ai/multica/server/internal/issueleader"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
)

// ActorSessionHeader carries the registered session a session-bound external
// credential acts for. It is set by the auth middleware from the token row and
// stripped from every client request, so handler code can trust it.
const ActorSessionHeader = "X-Actor-Session-ID"

func isExternalAgentActor(r *http.Request) bool {
	return r.Header.Get("X-Actor-Source") == "external_agent"
}

// externalActorSessionID returns the session stamped by auth, if any. A legacy
// token that predates session binding has none: it is a wide credential over
// every session of its agent and must not be used for session-sensitive work.
func externalActorSessionID(r *http.Request) (pgtype.UUID, bool) {
	raw := strings.TrimSpace(r.Header.Get(ActorSessionHeader))
	if raw == "" {
		return pgtype.UUID{}, false
	}
	id, err := util.ParseUUID(raw)
	if err != nil {
		return pgtype.UUID{}, false
	}
	return id, true
}

// requireExternalActorSession refuses session-sensitive operations performed
// with an external credential that is not bound to a registered session. Human
// and managed-task actors are not constrained here.
func (h *Handler) requireExternalActorSession(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	if !isExternalAgentActor(r) {
		return pgtype.UUID{}, true
	}
	sessionID, ok := externalActorSessionID(r)
	if !ok {
		writeError(w, http.StatusForbidden, "this credential is not bound to a session; reconnect the session to issue a session credential")
		return pgtype.UUID{}, false
	}
	return sessionID, true
}

func (h *Handler) issueSessionConflict(r *http.Request, q *db.Queries, issue db.Issue) (bool, error) {
	if !isExternalAgentActor(r) {
		return false, nil
	}
	session, ok := externalActorSessionID(r)
	if !ok {
		return true, nil
	}
	conflict, err := q.IssueActorSessionConflict(r.Context(), db.IssueActorSessionConflictParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, AgentID: parseUUID(r.Header.Get("X-Agent-ID")), SessionID: session})
	return conflict, err
}

func (h *Handler) checkIssueSessionWrite(w http.ResponseWriter, r *http.Request, issue db.Issue) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	if strings.Contains(r.URL.Path, "/comments") {
		return true
	}
	if isExternalAgentActor(r) {
		if _, ok := externalActorSessionID(r); !ok {
			writeError(w, 403, "session-bound credential required")
			return false
		}
	}
	conflict, err := h.issueSessionConflict(r, h.Queries, issue)
	if err != nil {
		writeError(w, 500, "load task session failed")
		return false
	}
	if conflict {
		writeError(w, 409, "task belongs to another session; request an explicit human handoff")
		return false
	}
	return true
}

func (h *Handler) checkCommentSession(w http.ResponseWriter, r *http.Request, issue db.Issue, content string) bool {
	conflict, err := h.issueSessionConflict(r, h.Queries, issue)
	if err != nil {
		writeError(w, 500, "load task session failed")
		return false
	}
	if conflict && (strings.Contains(content, "mention://agent/") || strings.Contains(content, "mention://agent-notify/") || strings.Contains(content, "mention://squad/")) {
		writeError(w, 409, "this session may post ordinary comments; dispatch requires an explicit human handoff")
		return false
	}
	return true
}

// leaderMatches is shared by managed claim and leader activity authorization.
func leaderMatches(ctx context.Context, q *db.Queries, issue db.Issue, squad db.Squad, agentID string) bool {
	leader, err := issueleader.ResolveIssueLeader(ctx, q, issue, squad)
	return err == nil && uuidToString(leader.Agent.ID) == agentID
}

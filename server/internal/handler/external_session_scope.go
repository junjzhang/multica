package handler

import (
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

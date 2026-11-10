package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// ExternalAgentTemplateResponse is the host identity a session belongs to.
// provider is the integration name and host_id is the integration's own server
// instance id, never a hostname or display alias.
type ExternalAgentTemplateResponse struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	HostID   string `json:"host_id"`
	Name     string `json:"name"`
}

func externalTemplateToResponse(agent db.Agent) *ExternalAgentTemplateResponse {
	if !agent.ExternalProvider.Valid || !agent.ExternalHostID.Valid {
		return nil
	}
	return &ExternalAgentTemplateResponse{
		ID:       uuidToString(agent.ID),
		Provider: agent.ExternalProvider.String,
		HostID:   agent.ExternalHostID.String,
		Name:     agent.Name,
	}
}

var (
	errExternalTemplateArchived     = errors.New("the host template for this session is archived; restore it or register the host again")
	errExternalTemplateNameConflict = errors.New("could not allocate a template name in this workspace")
)

// resolveExternalTemplate finds or creates the template for one host instance.
// The key is (workspace, owner, provider, host id): a host label, hostname, or
// display name never participates. Concurrent first connects are settled by the
// unique template index; a loser re-reads the winner's row instead of minting a
// second identity.
func (h *Handler) resolveExternalTemplate(
	ctx context.Context,
	q *db.Queries,
	wsUUID, ownerID pgtype.UUID,
	provider, hostID, name, permissionMode, visibility string,
) (db.Agent, bool, error) {
	key := db.GetExternalAgentTemplateByKeyParams{
		WorkspaceID:      wsUUID,
		OwnerID:          ownerID,
		ExternalProvider: pgtype.Text{String: provider, Valid: true},
		ExternalHostID:   pgtype.Text{String: hostID, Valid: true},
	}
	existing, err := q.GetExternalAgentTemplateByKey(ctx, key)
	if err == nil {
		if existing.ArchivedAt.Valid {
			return db.Agent{}, false, errExternalTemplateArchived
		}
		return existing, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return db.Agent{}, false, fmt.Errorf("load external template: %w", err)
	}

	for attempt := 0; attempt < 3; attempt++ {
		candidate := name
		if attempt > 0 {
			candidate = externalTemplateNameWithSuffix(name, uuidToString(dbid.NewV7()))
		}
		created, createErr := q.CreateExternalAgentTemplate(ctx, db.CreateExternalAgentTemplateParams{
			WorkspaceID:      wsUUID,
			Name:             candidate,
			Visibility:       visibility,
			PermissionMode:   permissionMode,
			OwnerID:          ownerID,
			ExternalProvider: key.ExternalProvider,
			ExternalHostID:   key.ExternalHostID,
		})
		if createErr == nil {
			return created, true, nil
		}
		if !isUniqueViolation(createErr) {
			return db.Agent{}, false, fmt.Errorf("create external template: %w", createErr)
		}
		// Either the template key (a concurrent first connect won) or the
		// workspace-unique name collided. The key is the identity, so a
		// re-read settles the race; no row means the name was taken.
		if winner, readErr := q.GetExternalAgentTemplateByKey(ctx, key); readErr == nil {
			if winner.ArchivedAt.Valid {
				return db.Agent{}, false, errExternalTemplateArchived
			}
			return winner, false, nil
		} else if !errors.Is(readErr, pgx.ErrNoRows) {
			return db.Agent{}, false, fmt.Errorf("re-read external template: %w", readErr)
		}
	}
	return db.Agent{}, false, errExternalTemplateNameConflict
}

// externalTemplateNameWithSuffix appends a short, unique suffix so a display-name
// collision cannot force the caller to pick a different host identity.
func externalTemplateNameWithSuffix(name, id string) string {
	suffix := strings.ReplaceAll(id, "-", "")
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	candidate := strings.TrimSpace(name) + " · " + suffix
	if utf8Len := len([]rune(candidate)); utf8Len > maxExternalAgentNameLength {
		base := []rune(strings.TrimSpace(name))
		limit := maxExternalAgentNameLength - (utf8Len - len(base))
		if limit < 0 {
			limit = 0
		}
		candidate = string(base[:limit]) + " · " + suffix
	}
	return strings.TrimSpace(candidate)
}

func (h *Handler) writeExternalTemplateError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errExternalTemplateArchived):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, errExternalTemplateNameConflict):
		writeError(w, http.StatusConflict, err.Error())
	default:
		slog.Error("external template resolution failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to resolve external template")
	}
}

// resolveTokenSession binds a new credential to exactly one live registered
// session. An explicit session id wins after ownership checks. Without one the
// server may infer only when the agent has exactly one live session; a legacy
// agent with several sessions gets a conflict instead of a wide credential that
// could act as any of them.
func (h *Handler) resolveTokenSession(w http.ResponseWriter, r *http.Request, agent db.Agent, requested string) (pgtype.UUID, bool) {
	requested = strings.TrimSpace(requested)
	if requested != "" {
		sessionID, ok := parseUUIDOrBadRequest(w, requested, "external_session_id")
		if !ok {
			return pgtype.UUID{}, false
		}
		session, err := h.Queries.GetExternalAgentSessionByID(r.Context(), db.GetExternalAgentSessionByIDParams{
			ID:          sessionID,
			WorkspaceID: agent.WorkspaceID,
		})
		if err != nil || session.AgentID != agent.ID || session.OwnerUserID != agent.OwnerID || session.RevokedAt.Valid {
			writeError(w, http.StatusNotFound, "external session not found for this agent")
			return pgtype.UUID{}, false
		}
		return session.ID, true
	}

	sessions, err := h.Queries.ListExternalAgentSessionsByAgentIDs(r.Context(), []pgtype.UUID{agent.ID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load external sessions")
		return pgtype.UUID{}, false
	}
	live := make([]db.ExternalAgentSession, 0, len(sessions))
	for _, session := range sessions {
		if session.WorkspaceID == agent.WorkspaceID && session.OwnerUserID == agent.OwnerID && !session.RevokedAt.Valid {
			live = append(live, session)
		}
	}
	switch len(live) {
	case 0:
		writeError(w, http.StatusBadRequest, "external agent has no registered session to bind a credential to")
		return pgtype.UUID{}, false
	case 1:
		return live[0].ID, true
	default:
		writeError(w, http.StatusConflict, "external_session_id is required for an agent with multiple sessions")
		return pgtype.UUID{}, false
	}
}

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuecontract"
	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// archiveBlockerSampleLimit bounds how many exact ids the preview carries per
// category. The count is the true one; the sample is what a human can act on
// without paging an unbounded reference list.
const archiveBlockerSampleLimit = 5

// ArchiveBlocker is one category of in-flight reference found by the archive
// preflight. Blocking references must be resolved before the archive write;
// non-blocking ones (a pending notification) are withdrawn by the archive
// transaction itself and are reported so the preview is honest about them.
type ArchiveBlocker struct {
	Code      string   `json:"code"`
	Blocking  bool     `json:"blocking"`
	Count     int      `json:"count"`
	SampleIDs []string `json:"sample_ids"`
}

type agentArchivePreflightResponse struct {
	AgentID    string           `json:"agent_id"`
	Archivable bool             `json:"archivable"`
	Blockers   []ArchiveBlocker `json:"blockers"`
}

// archiveBlockerOrder is the stable presentation order: responsibilities and
// evidence that may already exist first, configuration references (default
// leader, membership) after them, and the non-blocking pending notifications
// last.
var archiveBlockerOrder = []struct {
	code     string
	blocking bool
}{
	{"in_flight_tasks", true},
	{"active_waits", true},
	{"unaccepted_contracts", true},
	{"unverified_messages", true},
	{"current_returns", true},
	{"assigned_issues", true},
	{"task_leaderships", true},
	{"coordinated_issues", true},
	{"squad_default_leader", true},
	{"squad_memberships", true},
	{"pending_messages", false},
}

// agentArchiveBlockers is the read-only reference scan shared by the preview
// and the archive write. It must run in the same transaction as the write to
// mean anything, because a reference can appear between preview and archive.
func (h *Handler) agentArchiveBlockers(ctx context.Context, q *db.Queries, agent db.Agent) ([]ArchiveBlocker, error) {
	rows, err := q.ListAgentArchiveBlockers(ctx, agent.ID)
	if err != nil {
		return nil, err
	}
	refs := map[string][]string{}
	for _, row := range rows {
		refs[row.Code] = append(refs[row.Code], row.RefID)
	}
	contracts, err := q.ListAgentOpenContractCandidates(ctx, db.ListAgentOpenContractCandidatesParams{
		WorkspaceID: agent.WorkspaceID,
		AgentID:     uuidToString(agent.ID),
	})
	if err != nil {
		return nil, err
	}
	agentID := uuidToString(agent.ID)
	for _, row := range contracts {
		var state issuecontract.State
		if err := json.Unmarshal(row.Data, &state); err != nil {
			// A malformed row cannot be proven safe to archive around, so it is
			// reported instead of silently skipped.
			refs["unaccepted_contracts"] = append(refs["unaccepted_contracts"], uuidToString(row.IssueID))
			continue
		}
		current := state.Current()
		if current == nil || len(state.Remaining()) == 0 {
			continue
		}
		if contractReferencesAgent(current, agentID) {
			refs["unaccepted_contracts"] = append(refs["unaccepted_contracts"], uuidToString(row.IssueID))
		}
	}

	blockers := make([]ArchiveBlocker, 0, len(archiveBlockerOrder))
	for _, entry := range archiveBlockerOrder {
		ids := refs[entry.code]
		if len(ids) == 0 {
			continue
		}
		sort.Strings(ids)
		sample := ids
		if len(sample) > archiveBlockerSampleLimit {
			sample = sample[:archiveBlockerSampleLimit]
		}
		blockers = append(blockers, ArchiveBlocker{
			Code:      entry.code,
			Blocking:  entry.blocking,
			Count:     len(ids),
			SampleIDs: sample,
		})
	}
	return blockers, nil
}

func contractReferencesAgent(version *issuecontract.Version, agentID string) bool {
	if version.Executor.ID == agentID || version.Acceptor.ID == agentID {
		return true
	}
	var basis map[string]issuecontract.Actor
	if err := json.Unmarshal(version.Basis, &basis); err != nil {
		return false
	}
	representative, ok := basis["executor_representative"]
	return ok && representative.ID == agentID
}

func blockingArchiveBlockers(blockers []ArchiveBlocker) []ArchiveBlocker {
	out := make([]ArchiveBlocker, 0, len(blockers))
	for _, blocker := range blockers {
		if blocker.Blocking {
			out = append(out, blocker)
		}
	}
	return out
}

// ArchiveAgentPreflight answers what archiving this external agent would leave
// stranded. It is read-only and never archives.
func (h *Handler) ArchiveAgentPreflight(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	agent, ok := h.loadAgentForUser(w, r, id)
	if !ok {
		return
	}
	if !h.canManageAgent(w, r, agent) {
		return
	}
	if agent.ExecutionKind != "external" {
		writeError(w, http.StatusBadRequest, "archive preflight applies to external agents")
		return
	}
	if agent.ArchivedAt.Valid {
		writeError(w, http.StatusConflict, "agent is already archived")
		return
	}
	blockers, err := h.agentArchiveBlockers(r.Context(), h.Queries, agent)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to scan archive references")
		return
	}
	writeJSON(w, http.StatusOK, agentArchivePreflightResponse{
		AgentID:    id,
		Archivable: len(blockingArchiveBlockers(blockers)) == 0,
		Blockers:   blockers,
	})
}

// archiveExternalAgent re-checks the preflight under a row lock and only then
// writes the archive and revokes every live credential. A reference that lands
// between the preview and this call either commits before the lock (and is
// seen) or is refused after the archive commits by the existing archived-agent
// checks in the assignment and delivery paths.
func (h *Handler) archiveExternalAgent(w http.ResponseWriter, r *http.Request, agent db.Agent) {
	userID := requestUserID(r)
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to archive agent")
		return
	}
	defer tx.Rollback(r.Context())
	queries := h.Queries.WithTx(tx)
	locked, err := queries.LockAgentForArchive(r.Context(), db.LockAgentForArchiveParams{
		ID:          agent.ID,
		WorkspaceID: agent.WorkspaceID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "agent not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to lock agent for archive")
		return
	}
	if locked.ArchivedAt.Valid {
		writeError(w, http.StatusConflict, "agent is already archived")
		return
	}
	blockers, err := h.agentArchiveBlockers(r.Context(), queries, locked)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to scan archive references")
		return
	}
	if blocking := blockingArchiveBlockers(blockers); len(blocking) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"code":     "agent_has_in_flight_references",
			"error":    "resolve the listed references before archiving this agent",
			"blockers": blockers,
		})
		return
	}
	archived, err := queries.ArchiveAgent(r.Context(), db.ArchiveAgentParams{
		ID:         agent.ID,
		ArchivedBy: parseUUID(userID),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to archive agent")
		return
	}
	revoked, err := queries.RevokeExternalAgentTokens(r.Context(), db.RevokeExternalAgentTokensParams{
		AgentID:     agent.ID,
		WorkspaceID: agent.WorkspaceID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to revoke external credentials")
		return
	}
	if _, err := queries.WithdrawAgentDeliveries(r.Context(), db.WithdrawAgentDeliveriesParams{
		AgentIds: []pgtype.UUID{agent.ID},
		Reason:   pgtype.Text{String: "Agent archived before the notification was accepted", Valid: true},
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to withdraw deliveries")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to archive agent")
		return
	}
	slog.Info("external agent archived", append(logger.RequestAttrs(r), "agent_id", uuidToString(agent.ID), "revoked_tokens", revoked)...)
	h.completeAgentArchive(w, r, archived)
}

// completeAgentArchive runs the post-commit side effects shared by every
// archive path: cancel outstanding tasks, return the archived row, and publish
// the archive event.
func (h *Handler) completeAgentArchive(w http.ResponseWriter, r *http.Request, archived db.Agent) {
	id := uuidToString(archived.ID)
	if _, err := h.TaskService.CancelTasksForArchivedAgent(r.Context(), archived.ID); err != nil {
		slog.Warn("cancel agent tasks on archive failed", append(logger.RequestAttrs(r), "error", err, "agent_id", id)...)
	}
	wsID := uuidToString(archived.WorkspaceID)
	slog.Info("agent archived", append(logger.RequestAttrs(r), "agent_id", id, "workspace_id", wsID)...)
	resp := h.agentToResponse(archived)
	if err := h.attachAgentSkills(r.Context(), &resp, archived.ID); err != nil {
		slog.Warn("load agent skills after archive failed", append(logger.RequestAttrs(r), "error", err, "agent_id", id)...)
		writeError(w, http.StatusInternalServerError, "failed to load agent skills")
		return
	}
	userID := requestUserID(r)
	actorType, actorID := h.resolveActor(r, userID, wsID)
	h.publish(protocol.EventAgentArchived, wsID, actorType, actorID, map[string]any{"agent": broadcastAgentResponse(resp)})
	redactAgentResponseForActor(&resp, actorType)
	writeJSON(w, http.StatusOK, resp)
}

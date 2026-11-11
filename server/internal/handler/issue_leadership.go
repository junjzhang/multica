package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueleader"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/sessionroute"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type leadershipRequest struct {
	ClientRequestID       string `json:"client_request_id"`
	ExpectedRevision      int64  `json:"expected_revision"`
	ExpectedIssueRevision int64  `json:"expected_issue_revision"`
	PreviousAgentID       string `json:"previous_agent_id"`
	PreviousSessionID     string `json:"previous_session_id"`
	AgentID               string `json:"agent_id"`
	SessionID             string `json:"session_id"`
	Reason                string `json:"reason"`
}

func (h *Handler) ClaimIssueLeadership(w http.ResponseWriter, r *http.Request) {
	r = h.withWakeupActor(r)
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	if !isExternalAgentActor(r) {
		writeError(w, 403, "claim requires an external member session")
		return
	}
	session, ok := h.requireExternalActorSession(w, r)
	if !ok {
		return
	}
	var req leadershipRequest
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		writeError(w, 400, "invalid leadership claim")
		return
	}
	if issue.AssigneeType.String != "squad" {
		writeError(w, 409, "assign a squad before claiming leadership")
		return
	}
	tx, err := h.beginWakeupWrite(r.Context())
	if err != nil {
		writeError(w, 500, "begin claim failed")
		return
	}
	defer tx.Rollback(r.Context())
	q := h.Queries.WithTx(tx)
	squad, err := q.LockSquadForAutopilotAssignment(r.Context(), db.LockSquadForAutopilotAssignmentParams{ID: issue.AssigneeID, WorkspaceID: issue.WorkspaceID})
	if err != nil {
		writeError(w, 409, "squad unavailable")
		return
	}
	issue, err = q.LockIssueForDescriptionUpdate(r.Context(), db.LockIssueForDescriptionUpdateParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID})
	if err != nil {
		writeError(w, 409, "issue changed")
		return
	}
	if issue.Revision != req.ExpectedIssueRevision {
		writeError(w, 409, "issue changed")
		return
	}
	actor := parseUUID(r.Header.Get("X-Agent-ID"))
	agent, err := q.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{ID: actor, WorkspaceID: issue.WorkspaceID})
	if err != nil || !h.canInvokeAgent(r.Context(), agent, "agent", uuidToString(actor), requestUserID(r), uuidToString(issue.WorkspaceID)) {
		writeError(w, 403, "cannot invoke task leader")
		return
	}
	leader, err := issueleader.Claim(r.Context(), q, issue, squad, actor, session, req.ExpectedRevision, requestUserID(r))
	if err != nil {
		writeError(w, 409, err.Error())
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		writeError(w, 409, "claim conflicted")
		return
	}
	writeJSON(w, 200, map[string]any{"root_issue_id": uuidToString(leader.RootID), "agent_id": uuidToString(leader.Agent.ID), "session_id": uuidToString(leader.SessionID), "revision": leader.Revision})
}

// HandoffIssueLeadership changes current responsibility, never accepted returns,
// authors, running tasks, or the targets and receipts of existing envelopes.
func (h *Handler) HandoffIssueLeadership(w http.ResponseWriter, r *http.Request) {
	if isMachineCredentialActor(r) {
		writeError(w, 403, "leadership handoff requires a human owner/admin")
		return
	}
	r = h.withWakeupActor(r)
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	member, ok := h.requireWorkspaceMember(w, r, uuidToString(issue.WorkspaceID), "workspace not found")
	if !ok {
		return
	}
	if !roleAllowed(member.Role, "owner", "admin") {
		writeError(w, 403, "leadership handoff requires a human owner/admin")
		return
	}
	var req leadershipRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req) != nil || strings.TrimSpace(req.Reason) == "" {
		writeError(w, 400, "handoff requires a reason")
		return
	}
	requestID, ok := parseUUIDOrBadRequest(w, req.ClientRequestID, "client_request_id")
	if !ok {
		return
	}
	agentID, ok := parseUUIDOrBadRequest(w, req.AgentID, "agent_id")
	if !ok {
		return
	}
	previous, ok := parseUUIDOrBadRequest(w, req.PreviousAgentID, "previous_agent_id")
	if !ok {
		return
	}
	var sessionID, previousSession pgtype.UUID
	if req.SessionID != "" {
		sessionID, ok = parseUUIDOrBadRequest(w, req.SessionID, "session_id")
		if !ok {
			return
		}
	}
	if req.PreviousSessionID != "" {
		previousSession, ok = parseUUIDOrBadRequest(w, req.PreviousSessionID, "previous_session_id")
		if !ok {
			return
		}
	}
	raw, _ := json.Marshal(req)
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	tx, err := h.beginWakeupWrite(r.Context())
	if err != nil {
		writeError(w, 500, "begin handoff failed")
		return
	}
	defer tx.Rollback(r.Context())
	q := h.Queries.WithTx(tx)
	if issue.AssigneeType.String == "agent" {
		h.handoffExternalExecutor(w, r, q, tx, issue, req, requestID, agentID, previous, sessionID, previousSession, member)
		return
	}
	root, err := q.GetIssueLeadershipRoot(r.Context(), db.GetIssueLeadershipRootParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID})
	if err != nil || !root.LeadershipSquadID.Valid {
		writeError(w, 409, "task has no leadership root")
		return
	}
	squad, err := q.LockSquadForUpdate(r.Context(), db.LockSquadForUpdateParams{ID: root.LeadershipSquadID, WorkspaceID: issue.WorkspaceID})
	if err != nil || squad.ArchivedAt.Valid {
		writeError(w, 409, "squad unavailable")
		return
	}
	agent, err := q.LockAgentForContract(r.Context(), db.LockAgentForContractParams{ID: agentID, WorkspaceID: issue.WorkspaceID})
	if err != nil || agent.ArchivedAt.Valid || !h.memberCanWireAgent(r.Context(), member, agent, uuidToString(issue.WorkspaceID)) {
		writeError(w, 403, "target agent is unavailable or not callable")
		return
	}
	eligible, err := q.IsSquadMember(r.Context(), db.IsSquadMemberParams{SquadID: squad.ID, MemberType: "agent", MemberID: agentID})
	if err != nil || !eligible {
		writeError(w, 400, "target must be a squad member")
		return
	}
	issues, err := q.ListLeadershipIssues(r.Context(), db.ListLeadershipIssuesParams{WorkspaceID: issue.WorkspaceID, RootID: root.ID})
	if err != nil {
		writeError(w, 409, "task chain is changing; reload")
		return
	}
	for _, row := range issues {
		if row.ID == root.ID {
			root = row
		}
	}
	if prior, err := q.GetActivity(r.Context(), requestID); err == nil {
		var data map[string]any
		_ = json.Unmarshal(prior.Details, &data)
		if prior.WorkspaceID != issue.WorkspaceID || prior.IssueID != root.ID || prior.ActorID != member.UserID || data["request_digest"] != digest {
			writeError(w, 409, "request id reused with different content")
			return
		}
		writeJSON(w, 200, data)
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 500, "load handoff failed")
		return
	}
	if root.LeaderAgentID != previous || root.LeaderExternalSessionID != previousSession {
		writeError(w, 409, "previous leader changed")
		return
	}
	if (agent.ExecutionKind == "external") != sessionID.Valid {
		writeError(w, 400, "external targets require a session; managed targets do not accept one")
		return
	}
	updated, err := q.HandoffIssueLeadership(r.Context(), db.HandoffIssueLeadershipParams{ID: root.ID, WorkspaceID: root.WorkspaceID, AgentID: agentID, SessionID: sessionID, ExpectedRevision: req.ExpectedRevision, ExpectedIssueRevision: req.ExpectedIssueRevision})
	if err != nil {
		writeError(w, 409, "leadership or issue revision changed")
		return
	}
	contracts := []string{}
	for _, row := range issues {
		if _, err = q.RevokeActiveAgentSessionBindings(r.Context(), db.RevokeActiveAgentSessionBindingsParams{WorkspaceID: row.WorkspaceID, AgentID: previous, IssueID: row.ID}); err != nil {
			writeError(w, 500, "revoke old executor binding failed")
			return
		}
		if sessionID.Valid {
			if _, err = sessionroute.Bind(r.Context(), q, sessionroute.BindRequest{WorkspaceID: uuidToString(row.WorkspaceID), IssueID: uuidToString(row.ID), AgentID: req.AgentID, SessionID: req.SessionID, CreatedBy: requestUserID(r)}); err != nil {
				writeError(w, 409, err.Error())
				return
			}
		}
		if err = q.WithdrawLeadershipDeliveries(r.Context(), db.WithdrawLeadershipDeliveriesParams{WorkspaceID: row.WorkspaceID, IssueID: row.ID, AgentID: previous}); err != nil {
			writeError(w, 500, "withdraw old dispatch failed")
			return
		}
		if err = q.CancelLeadershipQueuedTasks(r.Context(), db.CancelLeadershipQueuedTasksParams{IssueID: row.ID, AgentID: previous}); err != nil {
			writeError(w, 500, "cancel queued dispatch failed")
			return
		}
		if has, err := q.IssueHasContract(r.Context(), db.IssueHasContractParams{IssueID: row.ID, WorkspaceID: row.WorkspaceID}); err != nil {
			writeError(w, 500, "load affected contracts failed")
			return
		} else if has {
			contracts = append(contracts, uuidToString(row.ID))
		}
	}
	if updated.Status != "backlog" && !updated.TriageState.Valid {
		target := service.AssignmentTarget{Agent: agent, Squad: &squad}
		var recipient *string
		if sessionID.Valid {
			recipient = &req.SessionID
		}
		if err = service.ExternalAssignment(r.Context(), q, updated, target, recipient, true, "member", requestUserID(r), requestUserID(r)); err != nil {
			writeError(w, 409, err.Error())
			return
		}
		if agent.ExecutionKind == "managed" {
			if _, _, err = h.TaskService.EnqueueSquadLeaderTaskInTx(r.Context(), q, updated, agentID, squad.ID, pgtype.UUID{}, service.OriginDerived, nil, nil); err != nil {
				writeError(w, 409, err.Error())
				return
			}
		}
	}
	affected, err := q.ListLeadershipAffectedContracts(r.Context(), db.ListLeadershipAffectedContractsParams{WorkspaceID: root.WorkspaceID, RootID: root.ID})
	if err != nil {
		writeError(w, 500, "list affected contracts failed")
		return
	}
	contracts = contracts[:0]
	for _, id := range affected {
		contracts = append(contracts, uuidToString(id))
	}
	response := map[string]any{"root_issue_id": uuidToString(root.ID), "revision": updated.LeadershipRevision, "issue_revision": updated.Revision, "previous_agent_id": req.PreviousAgentID, "previous_session_id": req.PreviousSessionID, "agent_id": req.AgentID, "session_id": req.SessionID, "reason": req.Reason, "request_digest": digest, "stale_contract_ids": contracts, "historical_results": "explicit_result_takeover_required"}
	details, _ := json.Marshal(response)
	if _, err = q.CreateActivity(r.Context(), db.CreateActivityParams{ID: requestID, WorkspaceID: root.WorkspaceID, IssueID: root.ID, ActorType: pgtype.Text{String: "member", Valid: true}, ActorID: member.UserID, Action: "leadership_handoff", Details: details}); err != nil {
		writeError(w, 500, "record handoff failed")
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		writeError(w, 409, "handoff conflicted")
		return
	}
	writeJSON(w, 200, response)
}

// External executor recovery uses its binding and issue CAS; it does not
// manufacture a squad leadership root for an ordinary agent assignment.
func (h *Handler) handoffExternalExecutor(w http.ResponseWriter, r *http.Request, q *db.Queries, tx pgx.Tx, issue db.Issue, req leadershipRequest, requestID, agentID, previous, sessionID, previousSession pgtype.UUID, member db.Member) {
	ctx := r.Context()
	old, err := q.LockAgentForContract(ctx, db.LockAgentForContractParams{ID: previous, WorkspaceID: issue.WorkspaceID})
	if err != nil || old.ExecutionKind != "external" {
		writeError(w, 400, "direct executor handoff requires an external current executor")
		return
	}
	target, err := q.LockAgentForContract(ctx, db.LockAgentForContractParams{ID: agentID, WorkspaceID: issue.WorkspaceID})
	if err != nil || target.ArchivedAt.Valid || !h.memberCanWireAgent(ctx, member, target, uuidToString(issue.WorkspaceID)) {
		writeError(w, 403, "target agent is unavailable or not callable")
		return
	}
	if (target.ExecutionKind == "external") != sessionID.Valid {
		writeError(w, 400, "external targets require a session; managed targets do not accept one")
		return
	}
	issue, err = q.LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID})
	if err != nil {
		writeError(w, 409, "issue changed")
		return
	}
	raw, _ := json.Marshal(req)
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	if prior, err := q.GetActivity(ctx, requestID); err == nil {
		var data map[string]any
		_ = json.Unmarshal(prior.Details, &data)
		if prior.WorkspaceID != issue.WorkspaceID || prior.IssueID != issue.ID || prior.ActorID != member.UserID || data["request_digest"] != digest {
			writeError(w, 409, "request id reused with different content")
			return
		}
		writeJSON(w, 200, data)
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 500, "load handoff failed")
		return
	}
	binding, err := q.GetActiveAgentSessionBindingView(ctx, db.GetActiveAgentSessionBindingViewParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, AgentID: previous})
	if err != nil || binding.SessionID != previousSession {
		writeError(w, 409, "previous executor session changed")
		return
	}
	updated, err := q.HandoffIssueExecutor(ctx, db.HandoffIssueExecutorParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID, AgentID: agentID, PreviousAgentID: previous, ExpectedIssueRevision: req.ExpectedIssueRevision})
	if err != nil {
		writeError(w, 409, "executor or issue revision changed")
		return
	}
	if _, err = q.RevokeActiveAgentSessionBindings(ctx, db.RevokeActiveAgentSessionBindingsParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, AgentID: previous}); err != nil {
		writeError(w, 500, "revoke executor binding failed")
		return
	}
	if sessionID.Valid {
		if _, err = sessionroute.Bind(ctx, q, sessionroute.BindRequest{WorkspaceID: uuidToString(issue.WorkspaceID), IssueID: uuidToString(issue.ID), AgentID: req.AgentID, SessionID: req.SessionID, CreatedBy: requestUserID(r)}); err != nil {
			writeError(w, 409, err.Error())
			return
		}
	}
	if err = q.WithdrawLeadershipDeliveries(ctx, db.WithdrawLeadershipDeliveriesParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, AgentID: previous}); err != nil {
		writeError(w, 500, "withdraw executor dispatch failed")
		return
	}
	if err = q.CancelExecutorQueuedTasks(ctx, db.CancelExecutorQueuedTasksParams{IssueID: issue.ID, AgentID: previous}); err != nil {
		writeError(w, 500, "cancel queued executor dispatch failed")
		return
	}
	var queued db.AgentTaskQueue
	if updated.Status != "backlog" && !updated.TriageState.Valid {
		var recipient *string
		if sessionID.Valid {
			recipient = &req.SessionID
		}
		if err = service.ExternalAssignment(ctx, q, updated, service.AssignmentTarget{Agent: target}, recipient, true, "member", requestUserID(r), requestUserID(r)); err != nil {
			writeError(w, 409, err.Error())
			return
		}
		if target.ExecutionKind == "managed" {
			queued, _, err = h.TaskService.EnqueueAssignedTaskInTx(ctx, q, updated, nil, nil)
			if err != nil {
				writeError(w, 409, err.Error())
				return
			}
		}
	}
	response := map[string]any{"issue_id": uuidToString(issue.ID), "issue_revision": updated.Revision, "agent_id": req.AgentID, "session_id": req.SessionID, "previous_agent_id": req.PreviousAgentID, "previous_session_id": req.PreviousSessionID, "reason": req.Reason, "request_digest": digest, "historical_results": "explicit_result_takeover_required"}
	data, _ := json.Marshal(response)
	if _, err = q.CreateActivity(ctx, db.CreateActivityParams{ID: requestID, WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, ActorType: strToText("member"), ActorID: member.UserID, Action: "executor_handoff", Details: data}); err != nil {
		writeError(w, 500, "record executor handoff failed")
		return
	}
	if err = tx.Commit(ctx); err != nil {
		writeError(w, 409, "executor handoff conflicted")
		return
	}
	if queued.ID.Valid {
		h.TaskService.NotifyTaskEnqueued(ctx, queued)
	}
	writeJSON(w, 200, response)
}

package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/delegation"
	"github.com/multica-ai/multica/server/internal/issueleader"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/sessionroute"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
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
	SuppressRun           bool   `json:"suppress_run,omitempty"`
}

// projectTaskLeader fills the task-leader fields from the chain root. Subtasks
// carry only the leadership root pointer; the leader columns live on the root,
// so a response built from the subtask row alone would read "unavailable".
func (h *Handler) projectTaskLeader(ctx context.Context, issue db.Issue, resp *IssueResponse) {
	if resp == nil || resp.LeaderAgentID != "" || !issue.LeadershipRootIssueID.Valid {
		return
	}
	root, err := h.Queries.GetIssueLeadershipRoot(ctx, db.GetIssueLeadershipRootParams{
		ID:          issue.ID,
		WorkspaceID: issue.WorkspaceID,
	})
	if err != nil {
		return
	}
	resp.LeadershipRootIssueID = uuidToString(root.ID)
	resp.LeadershipSquadID = uuidToString(root.LeadershipSquadID)
	resp.LeaderAgentID = uuidToString(root.LeaderAgentID)
	resp.LeaderExternalSessionID = uuidToString(root.LeaderExternalSessionID)
	resp.LeadershipRevision = root.LeadershipRevision
	resp.LeadershipRootRevision = root.Revision
}

// issueResponseForEvent renders the issue embedded in an issue:updated event.
// Every issue:updated payload goes through it: a subtask row carries only the
// leadership root pointer, so without the projection the event would patch the
// detail cache with a row whose leader is empty and drop the chain's leader.
func (h *Handler) issueResponseForEvent(ctx context.Context, issue db.Issue) IssueResponse {
	resp := issueToResponse(issue, h.getIssuePrefix(ctx, issue.WorkspaceID))
	h.projectTaskLeader(ctx, issue, &resp)
	return resp
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
	if issue.LeadershipRootIssueID.Valid {
		writeError(w, 409, "task already has a leadership root; use explicit handoff")
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
	h.handoffIssue(w, r, false)
}

func (h *Handler) HandoffIssueExecutor(w http.ResponseWriter, r *http.Request) {
	h.handoffIssue(w, r, true)
}

// LegacyIssueHandoff preserves the old dispatch until all repository callers
// have migrated and one release cycle has passed.
func (h *Handler) LegacyIssueHandoff(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Deprecation", "true")
	if isMachineCredentialActor(r) {
		writeError(w, 403, "leadership handoff requires a human owner/admin")
		return
	}
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	h.handoffIssue(w, r, issue.AssigneeType.String == "agent")
}

func (h *Handler) handoffIssue(w http.ResponseWriter, r *http.Request, executor bool) {
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
	r = r.WithContext(withWakeupHandedTo(r.Context(), uuidToString(agentID)))
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
	if executor {
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
		if prior.Action != "leadership_handoff" || prior.WorkspaceID != issue.WorkspaceID || prior.IssueID != root.ID || prior.ActorID != member.UserID || data["request_digest"] != digest {
			writeError(w, 409, "request id reused with different content")
			return
		}
		data["suppress_run"] = req.SuppressRun
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
	intent, err := delegation.LoadAssignmentIntent(r.Context(), q, root.ID, delegation.Assignee{Type: root.AssigneeType.String, ID: uuidToString(root.AssigneeID)})
	if err != nil {
		writeError(w, 500, "load accepted assignment return failed")
		return
	}
	var returns []delegation.ReturnIntent
	if intent != nil {
		returns = intent.Returns
	}
	updated, err := q.HandoffIssueLeadership(r.Context(), db.HandoffIssueLeadershipParams{ID: root.ID, WorkspaceID: root.WorkspaceID, AgentID: agentID, SessionID: sessionID, ExpectedRevision: req.ExpectedRevision, ExpectedIssueRevision: req.ExpectedIssueRevision})
	if err != nil {
		writeError(w, 409, "leadership or issue revision changed")
		return
	}
	contracts := []string{}
	advancedReturns := []map[string]string{}
	for _, row := range issues {
		before, after, err := advanceHandoffReturn(r.Context(), q, row, previous, previousSession, agentID, sessionID)
		if err != nil {
			writeError(w, 409, "current return differs from the previous leader; explicitly resolve the return before handoff")
			return
		}
		if after.Valid {
			advancedReturns = append(advancedReturns, map[string]string{"issue_id": uuidToString(row.ID), "previous_return_id": uuidToString(before), "current_return_id": uuidToString(after)})
		}
		if err = issueleader.RetireExecution(r.Context(), q, row, previous); err != nil {
			status := 500
			if errors.Is(err, issueleader.ErrConflict) {
				status = 409
			}
			writeError(w, status, "task execution is changing; retry handoff")
			return
		}
		if has, err := q.IssueHasContract(r.Context(), db.IssueHasContractParams{IssueID: row.ID, WorkspaceID: row.WorkspaceID}); err != nil {
			writeError(w, 500, "load affected contracts failed")
			return
		} else if has {
			contracts = append(contracts, uuidToString(row.ID))
		}
	}
	var queued db.AgentTaskQueue
	if updated.Status != "backlog" && !updated.TriageState.Valid {
		target := service.AssignmentTarget{Agent: agent, Squad: &squad}
		var recipient *string
		if sessionID.Valid {
			recipient = &req.SessionID
		}
		if err = service.ExternalAssignment(r.Context(), q, updated, target, recipient, !req.SuppressRun, "member", requestUserID(r), requestUserID(r), h.cfg.AppURL); err != nil {
			writeError(w, 409, err.Error())
			return
		}
		if agent.ExecutionKind == "managed" && !req.SuppressRun {
			if queued, _, err = h.TaskService.EnqueueSquadLeaderTaskInTx(r.Context(), q, updated, agentID, squad.ID, pgtype.UUID{}, service.OriginDerived, returns, nil); err != nil {
				writeError(w, 409, err.Error())
				return
			}
		}
	}
	if intent != nil {
		if err = delegation.StoreAssignmentIntent(r.Context(), q, updated.WorkspaceID, updated.ID, delegation.Assignee{Type: updated.AssigneeType.String, ID: uuidToString(updated.AssigneeID)}, returns, queued.ID); err != nil {
			writeError(w, 500, "preserve accepted assignment return failed")
			return
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
	response := map[string]any{"root_issue_id": uuidToString(root.ID), "revision": updated.LeadershipRevision, "issue_revision": updated.Revision, "previous_agent_id": req.PreviousAgentID, "previous_session_id": req.PreviousSessionID, "agent_id": req.AgentID, "session_id": req.SessionID, "reason": req.Reason, "suppress_run": req.SuppressRun, "request_digest": digest, "stale_contract_ids": contracts, "advanced_returns": advancedReturns, "historical_results": "explicit_result_takeover_required"}
	details, _ := json.Marshal(response)
	if _, err = q.CreateActivity(r.Context(), db.CreateActivityParams{ID: requestID, WorkspaceID: root.WorkspaceID, IssueID: root.ID, ActorType: pgtype.Text{String: "member", Valid: true}, ActorID: member.UserID, Action: "leadership_handoff", Details: details}); err != nil {
		writeError(w, 500, "record handoff failed")
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		writeError(w, 409, "handoff conflicted")
		return
	}
	if queued.ID.Valid {
		h.TaskService.NotifyTaskEnqueued(r.Context(), queued)
	}
	h.publishIssueChange(r.Context(), updated, root.Status, "member", requestUserID(r))
	h.publishMailboxChanged(updated.WorkspaceID, uuidToString(updated.ID))
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
		if prior.Action != "executor_handoff" || prior.WorkspaceID != issue.WorkspaceID || prior.IssueID != issue.ID || prior.ActorID != member.UserID || data["request_digest"] != digest {
			writeError(w, 409, "request id reused with different content")
			return
		}
		data["suppress_run"] = req.SuppressRun
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
	intent, err := delegation.LoadAssignmentIntent(ctx, q, issue.ID, delegation.Assignee{Type: issue.AssigneeType.String, ID: uuidToString(issue.AssigneeID)})
	if err != nil {
		writeError(w, 500, "load accepted assignment return failed")
		return
	}
	var returns []delegation.ReturnIntent
	if intent != nil {
		returns = intent.Returns
	}
	previousReturn, currentReturn, err := advanceHandoffReturn(ctx, q, issue, previous, previousSession, agentID, sessionID)
	if err != nil {
		writeError(w, 409, "current return differs from the previous executor; explicitly resolve it before handoff")
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
		if err = service.ExternalAssignment(ctx, q, updated, service.AssignmentTarget{Agent: target}, recipient, !req.SuppressRun, "member", requestUserID(r), requestUserID(r), h.cfg.AppURL); err != nil {
			writeError(w, 409, err.Error())
			return
		}
		if target.ExecutionKind == "managed" && !req.SuppressRun {
			queued, _, err = h.TaskService.EnqueueAssignedTaskInTx(ctx, q, updated, returns, nil)
			if err != nil {
				writeError(w, 409, err.Error())
				return
			}
		}
	}
	if intent != nil {
		if err = delegation.StoreAssignmentIntent(ctx, q, updated.WorkspaceID, updated.ID, delegation.Assignee{Type: updated.AssigneeType.String, ID: uuidToString(updated.AssigneeID)}, returns, queued.ID); err != nil {
			writeError(w, 500, "preserve accepted assignment return failed")
			return
		}
	}
	response := map[string]any{"issue_id": uuidToString(issue.ID), "issue_revision": updated.Revision, "agent_id": req.AgentID, "session_id": req.SessionID, "previous_agent_id": req.PreviousAgentID, "previous_session_id": req.PreviousSessionID, "reason": req.Reason, "suppress_run": req.SuppressRun, "request_digest": digest, "historical_results": "explicit_result_takeover_required"}
	if currentReturn.Valid {
		response["previous_return_id"] = uuidToString(previousReturn)
		response["current_return_id"] = uuidToString(currentReturn)
	}
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
	h.publishExecutorHandoffChange(ctx, updated, issue, "member", requestUserID(r))
	h.publishMailboxChanged(updated.WorkspaceID, uuidToString(updated.ID))
	writeJSON(w, 200, response)
}

// Same-template handoff advances the current dispatcher address. Existing run
// obligations keep the old immutable return version until explicitly received.
func advanceHandoffReturn(ctx context.Context, q *db.Queries, issue db.Issue, previous, previousSession, next, nextSession pgtype.UUID) (pgtype.UUID, pgtype.UUID, error) {
	if previous != next || !nextSession.Valid {
		return pgtype.UUID{}, pgtype.UUID{}, nil
	}
	current, err := q.LockIssueDelegationReturnForDispatcher(ctx, db.LockIssueDelegationReturnForDispatcherParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, DispatcherAgentID: previous})
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, pgtype.UUID{}, nil
	}
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, err
	}
	if current.ReturnKind != string(delegation.KindExternalSession) || current.ExternalSessionID != previousSession {
		return pgtype.UUID{}, pgtype.UUID{}, issueleader.ErrConflict
	}
	if previousSession == nextSession {
		return current.ID, current.ID, nil
	}
	target, err := q.CreateDelegationReturnVersion(ctx, db.CreateDelegationReturnVersionParams{ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, DispatcherAgentID: next, ReturnKind: string(delegation.KindExternalSession), ExternalSessionID: nextSession})
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, err
	}
	if err = q.RetireCurrentDelegationReturn(ctx, db.RetireCurrentDelegationReturnParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, DispatcherAgentID: previous}); err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, err
	}
	if err = q.SetCurrentDelegationReturn(ctx, target.ID); err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, err
	}
	return current.ID, target.ID, nil
}

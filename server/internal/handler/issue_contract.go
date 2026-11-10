package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/issuecontract"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type ContractView struct {
	Contract               *issuecontract.State            `json:"contract"`
	Stale                  bool                            `json:"stale"`
	Accepted               []int                           `json:"accepted"`
	PendingActor           *issuecontract.Actor            `json:"pending_actor"`
	Executor               *issuecontract.Actor            `json:"executor"`
	ExecutorRepresentative *issuecontract.Actor            `json:"executor_representative"`
	Acceptor               *issuecontract.Actor            `json:"acceptor"`
	CanEdit                bool                            `json:"can_edit"`
	CanSubmit              bool                            `json:"can_submit"`
	CanReview              bool                            `json:"can_review"`
	MergeAuthorization     IssueMergeAuthorizationResponse `json:"merge_authorization"`
}

type contractRequest struct {
	Action            string   `json:"action"`
	ClientRequestID   string   `json:"client_request_id"`
	ExpectedSequence  int      `json:"expected_sequence"`
	Version           int      `json:"version"`
	Goal              string   `json:"goal,omitempty"`
	Scope             string   `json:"scope,omitempty"`
	AuthorizationNote string   `json:"authorization_note,omitempty"`
	Requirements      []string `json:"requirements,omitempty"`
	Criteria          []int    `json:"criteria,omitempty"`
	Evidence          string   `json:"evidence,omitempty"`
	DeliveryID        string   `json:"delivery_id,omitempty"`
	Reason            string   `json:"reason,omitempty"`
	ChildIDs          []string `json:"child_ids,omitempty"`
}

func readContract(ctx context.Context, q *db.Queries, issue db.Issue) (issuecontract.State, error) {
	raw, err := q.GetIssueContract(ctx, db.GetIssueContractParams{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return issuecontract.Empty(), nil
	}
	if err != nil {
		return issuecontract.State{}, err
	}
	var state issuecontract.State
	err = json.Unmarshal(raw, &state)
	return state, err
}

func contractBasis(ctx context.Context, q *db.Queries, issue db.Issue) (json.RawMessage, error) {
	raw, err := q.GetIssueContractBasis(ctx, db.GetIssueContractBasisParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID})
	return json.RawMessage(raw), err
}

func sameJSON(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

func contractRepresentative(basis json.RawMessage) *issuecontract.Actor {
	var fields struct {
		Representative *issuecontract.Actor `json:"executor_representative"`
	}
	_ = json.Unmarshal(basis, &fields)
	return fields.Representative
}

func representsContractExecutor(actor, executor issuecontract.Actor, representative *issuecontract.Actor) bool {
	return actor == executor || representative != nil && actor == *representative
}

func (h *Handler) contractView(ctx context.Context, r *http.Request, q *db.Queries, issue db.Issue, state issuecontract.State) (ContractView, error) {
	actorType, actorID := h.resolveActor(r, requestUserID(r), uuidToString(issue.WorkspaceID))
	actor := issuecontract.Actor{Type: actorType, ID: actorID}
	executor := issuecontract.Actor{Type: issue.AssigneeType.String, ID: uuidToString(issue.AssigneeID)}
	basis, err := contractBasis(ctx, q, issue)
	if err != nil {
		return ContractView{}, err
	}
	active := issuestatus.Effective(ctx, q, issue.WorkspaceID, issue.Status) != "cancelled" && !issue.TriageState.Valid
	view := ContractView{Accepted: []int{}, CanEdit: active && h.canMaintainContract(ctx, r, q, issue, state, basis), MergeAuthorization: mergeAuthorizationResponse(issue)}
	if issue.AssigneeID.Valid {
		view.Executor = &executor
	}
	if issue.CoordinatorID.Valid {
		view.Acceptor = &issuecontract.Actor{Type: issue.CoordinatorType.String, ID: uuidToString(issue.CoordinatorID)}
	}
	view.ExecutorRepresentative = contractRepresentative(basis)
	v := state.Current()
	if v == nil {
		return view, nil
	}
	view.Contract = &state
	view.Stale = !sameJSON(v.Basis, basis)
	if !view.Stale {
		view.Accepted = state.Accepted()
	}
	if !active || !view.Stale && len(view.Accepted) == len(v.Criteria) {
		return view, nil
	}
	if view.Stale {
		view.PendingActor = &v.Author
		return view, nil
	}
	if state.Pending() != nil {
		view.PendingActor = &v.Acceptor
		view.CanReview = actor == v.Acceptor && !representsContractExecutor(actor, v.Executor, view.ExecutorRepresentative) && actor != state.Pending().Author
	} else {
		view.PendingActor = &v.Executor
		view.CanSubmit = representsContractExecutor(actor, v.Executor, view.ExecutorRepresentative)
	}
	return view, nil
}

func (h *Handler) GetIssueContract(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	state, err := readContract(r.Context(), h.Queries, issue)
	if err != nil {
		writeError(w, 500, "load contract failed")
		return
	}
	view, err := h.contractView(r.Context(), r, h.Queries, issue, state)
	if err != nil {
		writeError(w, 500, "load contract basis failed")
		return
	}
	writeJSON(w, 200, view)
}

type contractProblem struct {
	status  int
	message string
}

type contractEffects struct {
	children  []db.Issue
	cancelled []db.AgentTaskQueue
}

func (e contractProblem) Error() string             { return e.message }
func contractFail(status int, message string) error { return contractProblem{status, message} }

func (h *Handler) UpdateIssueContract(w http.ResponseWriter, r *http.Request) {
	r = h.withWakeupActor(r)
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	var req contractRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&req) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeError(w, 400, "invalid contract request")
		return
	}
	requestID, ok := parseUUIDOrBadRequest(w, req.ClientRequestID, "client_request_id")
	if !ok {
		return
	}
	req.ClientRequestID = uuidToString(requestID)
	for i, child := range req.ChildIDs {
		id, ok := parseUUIDOrBadRequest(w, child, "child_ids")
		if !ok {
			return
		}
		req.ChildIDs[i] = uuidToString(id)
		if slices.Contains(req.ChildIDs[:i], req.ChildIDs[i]) {
			writeError(w, 400, "duplicate child id")
			return
		}
	}
	actorType, actorID := h.resolveActor(r, requestUserID(r), uuidToString(issue.WorkspaceID))
	actor := issuecontract.Actor{Type: actorType, ID: actorID}
	if isMachineCredentialActor(r) && actor.Type != "agent" {
		writeError(w, 403, "contract actions require a concrete agent identity")
		return
	}
	// A contract write is session-sensitive: a template credential must name
	// the session it acts for, so one session cannot revise or accept on
	// another's behalf.
	if _, ok := h.requireExternalActorSession(w, r); !ok {
		return
	}
	raw, _ := json.Marshal(struct {
		Request contractRequest
		Actor   issuecontract.Actor
	}{req, actor})
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	ctx := r.Context()
	tx, err := h.beginWakeupWrite(ctx)
	if err != nil {
		writeError(w, 500, "begin contract update failed")
		return
	}
	defer tx.Rollback(ctx)
	q := h.Queries.WithTx(tx)
	// Completion also locks a task before its issue; keep that order here.
	if req.Action == "revise" && r.Header.Get("X-Actor-Source") == "task_token" {
		taskID, err := util.ParseUUID(r.Header.Get("X-Task-ID"))
		if err != nil {
			writeError(w, 403, "a verified source run is required")
			return
		}
		if _, err := q.LockAgentTaskForDelegation(ctx, taskID); err != nil {
			writeError(w, 403, "a verified source run is required")
			return
		}
	}
	issue, err = q.LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID})
	if err != nil {
		writeError(w, 409, "issue changed; reload")
		return
	}
	if issue.AssigneeType.String == "squad" {
		// NOWAIT preserves archive/leader protection without waiting in the
		// opposite order from runtime teardown's Agent-to-task locks.
		squad, err := q.LockSquadForContract(ctx, db.LockSquadForContractParams{ID: issue.AssigneeID, WorkspaceID: issue.WorkspaceID})
		if err != nil {
			writeError(w, 409, "squad is changing; reload before retrying")
			return
		}
		if _, err := q.LockAgentForContract(ctx, db.LockAgentForContractParams{ID: squad.LeaderID, WorkspaceID: issue.WorkspaceID}); err != nil {
			writeError(w, 409, "squad leader is changing; reload before retrying")
			return
		}
	}
	state, err := readContract(ctx, q, issue)
	if err != nil {
		writeError(w, 500, "load contract failed")
		return
	}
	for _, prior := range state.Requests {
		if prior.ID != req.ClientRequestID {
			continue
		}
		if prior.Digest != digest {
			writeError(w, 409, "request id reused with different content or actor")
			return
		}
		view, err := h.contractView(ctx, r, q, issue, state)
		if err != nil {
			writeError(w, 500, "load contract failed")
			return
		}
		writeJSON(w, 200, view)
		return
	}
	if req.ExpectedSequence != state.Sequence {
		writeError(w, 409, "contract changed; reload before retrying")
		return
	}
	if issuestatus.Effective(ctx, q, issue.WorkspaceID, issue.Status) == "cancelled" || issue.TriageState.Valid {
		writeError(w, 409, "reopen or accept triage before changing the contract")
		return
	}
	effects := contractEffects{}
	status, err := h.applyContractRequest(ctx, r, q, issue, &state, req, actor, &effects)
	if err == nil {
		err = contractStatusAllowed(ctx, q, issue, status)
	}
	if err != nil {
		var problem contractProblem
		if errors.As(err, &problem) {
			writeError(w, problem.status, problem.message)
		} else {
			writeError(w, 500, "contract update failed")
		}
		return
	}
	state.Sequence++
	state.Requests = append(state.Requests, issuecontract.RequestRecord{ID: req.ClientRequestID, Digest: digest})
	data, err := json.Marshal(state)
	if err != nil || len(data) > 2*1024*1024 {
		writeError(w, 409, "contract history limit reached")
		return
	}
	if err = q.SaveIssueContract(ctx, db.SaveIssueContractParams{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, Data: data}); err != nil {
		writeError(w, 500, "save contract failed")
		return
	}
	criteria, _ := json.Marshal(state.Current().Criteria)
	updated, err := q.UpdateIssue(ctx, db.UpdateIssueParams{
		ID: issue.ID, Status: strToText(status), SourceTaskID: h.wakeupSourceTaskID(r),
		AssigneeType: issue.AssigneeType, AssigneeID: issue.AssigneeID,
		StartDate: issue.StartDate, DueDate: issue.DueDate, ParentIssueID: issue.ParentIssueID,
		ProjectID: issue.ProjectID, Stage: issue.Stage,
		AcceptanceCriteria: criteria, ContractChanged: true,
	})
	if err != nil {
		writeError(w, 409, "status change conflicts with the contract")
		return
	}
	// Native field changes already emit issue.updated. A note-only revision
	// needs the same actor-aware event even when no native field changed.
	if issue.Status == updated.Status && sameJSON(issue.AcceptanceCriteria, criteria) {
		if err := q.CaptureIssueContractWakeup(ctx, db.CaptureIssueContractWakeupParams{IssueID: issue.ID, EventKey: "contract:" + req.ClientRequestID, SourceTaskID: h.wakeupSourceTaskID(r)}); err != nil {
			writeError(w, 500, "capture contract update failed")
			return
		}
	}
	cancelled, err := service.StopClosedIssueWakeups(ctx, q, updated)
	if err != nil {
		writeError(w, 500, "stop closed issue wakeups failed")
		return
	}
	if err = tx.Commit(ctx); err != nil {
		writeError(w, 500, "commit contract failed")
		return
	}
	h.broadcastCancelledWakeups(ctx, issue.WorkspaceID, append(cancelled, effects.cancelled...))
	h.recordIssueActivity(ctx, updated, actor.Type, actor.ID, "issue_contract_"+req.Action, map[string]any{"sequence": state.Sequence, "version": state.Current().Number})
	h.publishContractIssue(ctx, updated, issue.Status, actor)
	for _, child := range effects.children {
		h.publishContractIssue(ctx, child, "in_review", actor)
	}
	view, err := h.contractView(ctx, r, h.Queries, updated, state)
	if err != nil {
		writeError(w, 500, "load saved contract failed")
		return
	}
	writeJSON(w, 200, view)
}

func (h *Handler) applyContractRequest(ctx context.Context, r *http.Request, q *db.Queries, issue db.Issue, state *issuecontract.State, req contractRequest, actor issuecontract.Actor, effects *contractEffects) (string, error) {
	now := time.Now().UTC()
	v := state.Current()
	basis, err := contractBasis(ctx, q, issue)
	if err != nil {
		return "", err
	}
	representative := contractRepresentative(basis)
	if req.Action == "revise" {
		if !h.canMaintainContract(ctx, r, q, issue, *state, basis) {
			return "", contractFail(403, "contract maintenance requires a verified original requester in scope; the executor cannot change its agreement")
		}
		executor := issuecontract.Actor{Type: issue.AssigneeType.String, ID: uuidToString(issue.AssigneeID)}
		if representsContractExecutor(actor, executor, representative) {
			return "", contractFail(403, "the executor cannot revise its own agreement")
		}
		acceptor := issuecontract.Actor{Type: issue.CoordinatorType.String, ID: uuidToString(issue.CoordinatorID)}
		if !issue.AssigneeID.Valid || !issue.CoordinatorID.Valid || (executor.Type != "member" && executor.Type != "agent" && executor.Type != "squad") || representsContractExecutor(acceptor, executor, representative) {
			return "", contractFail(400, "choose distinct executor and coordinator before creating a contract")
		}
		if executor.Type == "squad" {
			if representative == nil {
				return "", contractFail(400, "choose an active squad with a current leader member")
			}
		} else if _, _, msg := h.validateCoordinator(ctx, uuidToString(issue.WorkspaceID), IssueCoordinatorInput{Type: executor.Type, ID: executor.ID}); msg != "" {
			return "", contractFail(400, msg)
		}
		if _, _, msg := h.validateCoordinator(ctx, uuidToString(issue.WorkspaceID), IssueCoordinatorInput{Type: acceptor.Type, ID: acceptor.ID}); msg != "" {
			return "", contractFail(400, msg)
		}
		if strings.TrimSpace(req.Goal) == "" || strings.TrimSpace(req.AuthorizationNote) == "" || len(req.Requirements) == 0 || len(req.Requirements) > 50 {
			return "", contractFail(400, "goal, authorization note and 1–50 criteria are required")
		}
		for _, c := range req.Requirements {
			if strings.TrimSpace(c) == "" || len(c) > 2000 {
				return "", contractFail(400, "criteria must contain 1–2000 bytes")
			}
		}
		criteria, _ := json.Marshal(req.Requirements)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(basis, &fields); err != nil {
			return "", err
		}
		fields["criteria"] = criteria
		basis, err = json.Marshal(fields)
		if err != nil {
			return "", err
		}
		state.Versions = append(state.Versions, issuecontract.Version{Number: len(state.Versions) + 1, Goal: req.Goal, Scope: req.Scope, AuthorizationNote: req.AuthorizationNote, Criteria: req.Requirements, Executor: executor, Acceptor: acceptor, Basis: basis, Author: actor, CreatedAt: now})
		return "in_progress", nil
	}
	if v == nil || req.Version != v.Number {
		return "", contractFail(409, "delivery must reference the current contract version")
	}
	if !sameJSON(v.Basis, basis) {
		return "", contractFail(409, "responsibility, criteria or authorization changed; revise the contract")
	}
	pending := state.Pending()
	switch req.Action {
	case "submit":
		if !representsContractExecutor(actor, v.Executor, representative) {
			return "", contractFail(403, "only the executor may submit delivery")
		}
		if pending != nil {
			return "", contractFail(409, "a delivery is already awaiting review")
		}
		if strings.TrimSpace(req.Evidence) == "" {
			return "", contractFail(400, "delivery evidence is required")
		}
		if err := issuecontract.ValidateCriteria(req.Criteria, state.Remaining()); err != nil {
			return "", contractFail(400, err.Error())
		}
		state.Deliveries = append(state.Deliveries, issuecontract.Delivery{ID: req.ClientRequestID, Version: v.Number, Criteria: req.Criteria, Evidence: req.Evidence, ChildIDs: append([]string{}, req.ChildIDs...), Author: actor, CreatedAt: now})
		return "in_review", nil
	case "accept", "return":
		if actor != v.Acceptor || representsContractExecutor(actor, v.Executor, representative) || pending != nil && actor == pending.Author {
			return "", contractFail(403, "only the specified acceptor may decide delivery")
		}
		if pending == nil || pending.ID != req.DeliveryID {
			return "", contractFail(409, "delivery is no longer awaiting review")
		}
		if req.Action == "return" {
			if strings.TrimSpace(req.Reason) == "" || len(req.Criteria) > 0 || len(req.ChildIDs) > 0 {
				return "", contractFail(400, "return requires a reason and cannot accept criteria or children")
			}
		} else {
			if err := issuecontract.ValidateCriteria(req.Criteria, pending.Criteria); err != nil {
				return "", contractFail(400, err.Error())
			}
			if len(req.ChildIDs) > 0 && len(req.Criteria) != len(pending.Criteria) {
				return "", contractFail(400, "closing children requires accepting every submitted criterion")
			}
			for _, childID := range req.ChildIDs {
				if !slices.Contains(pending.ChildIDs, childID) {
					return "", contractFail(400, "child was not covered by this delivery")
				}
				child, err := q.LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{ID: parseUUID(childID), WorkspaceID: issue.WorkspaceID})
				if err != nil || child.ParentIssueID != issue.ID || child.Status != "in_review" || child.CoordinatorType != issue.CoordinatorType || child.CoordinatorID != issue.CoordinatorID {
					return "", contractFail(409, "child must remain a direct in_review delivery with the same acceptor")
				}
				childContract, err := readContract(ctx, q, child)
				if err != nil {
					return "", err
				}
				if childContract.Current() != nil {
					return "", contractFail(409, "accept the child's own contract separately")
				}
				if err := contractStatusAllowed(ctx, q, child, "done"); err != nil {
					return "", err
				}
				updated, err := q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: child.ID, WorkspaceID: issue.WorkspaceID, Status: "done", SourceTaskID: h.wakeupSourceTaskID(r)})
				if err != nil {
					return "", err
				}
				cancelled, err := service.StopClosedIssueWakeups(ctx, q, updated)
				if err != nil {
					return "", err
				}
				effects.children = append(effects.children, updated)
				effects.cancelled = append(effects.cancelled, cancelled...)
			}
		}
		state.Decisions = append(state.Decisions, issuecontract.Decision{DeliveryID: pending.ID, Version: v.Number, Outcome: req.Action, Criteria: append([]int{}, req.Criteria...), Reason: req.Reason, ChildIDs: append([]string{}, req.ChildIDs...), Author: actor, CreatedAt: now})
		if len(state.Remaining()) == 0 {
			return "done", nil
		}
		return "in_progress", nil
	default:
		return "", contractFail(400, fmt.Sprintf("unknown contract action %q", req.Action))
	}
}

func contractStatusAllowed(ctx context.Context, q *db.Queries, issue db.Issue, status string) error {
	def, _, err := issueworkflow.ForProject(ctx, q, issue.WorkspaceID, issue.ProjectID)
	if err != nil {
		return err
	}
	if err := issueworkflow.CheckStatus(def, status); err != nil {
		return contractFail(409, "the project workflow must include contract status "+status)
	}
	return nil
}

func (h *Handler) publishContractIssue(ctx context.Context, issue db.Issue, previous string, actor issuecontract.Actor) {
	resp := issueToResponse(issue, h.getIssuePrefix(ctx, issue.WorkspaceID))
	h.fillStatusCategory(ctx, issue.WorkspaceID, &resp)
	h.Bus.Publish(events.Event{Type: protocol.EventIssueUpdated, WorkspaceID: uuidToString(issue.WorkspaceID), ActorType: actor.Type, ActorID: actor.ID,
		Payload: map[string]any{"issue": resp, "status_changed": previous != issue.Status, "prev_status": previous}})
}

package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Issue records are an append-only ledger of decisions and experiment runs.
// See docs/design/issue-record.md. There is no update or delete endpoint; a
// correction is a new record whose supersedes_seq names the earlier one.
const (
	maxIssueRecords           = 5000
	maxIssueRecordSummary     = 500
	maxIssueRecordRationale   = 4000
	maxIssueRecordEvidence    = 20
	maxIssueRecordEvidenceLen = 2000
	maxIssueRecordFieldsBytes = 4096
	maxIssueRecordBodyBytes   = 64 << 10
)

var issueRecordVerdicts = map[string]map[string]bool{
	"decision": {"pending": true, "keep": true, "revert": true},
	"run":      {"pending": true, "pass": true, "fail": true, "inconclusive": true},
}

type CreateIssueRecordRequest struct {
	ClientRequestID string          `json:"client_request_id"`
	Kind            string          `json:"kind"`
	Summary         string          `json:"summary"`
	Rationale       string          `json:"rationale"`
	Evidence        []string        `json:"evidence"`
	Verdict         string          `json:"verdict"`
	Fields          json.RawMessage `json:"fields"`
	SupersedesSeq   *int32          `json:"supersedes_seq"`
}

type IssueRecordResponse struct {
	ID              string         `json:"id"`
	IssueID         string         `json:"issue_id"`
	Seq             int32          `json:"seq"`
	ClientRequestID string         `json:"client_request_id"`
	Kind            string         `json:"kind"`
	Summary         string         `json:"summary"`
	Rationale       string         `json:"rationale"`
	Evidence        []string       `json:"evidence"`
	Verdict         string         `json:"verdict"`
	Fields          map[string]any `json:"fields"`
	SupersedesSeq   *int32         `json:"supersedes_seq"`
	AuthorType      string         `json:"author_type"`
	AuthorID        string         `json:"author_id"`
	ActorSessionID  *string        `json:"actor_session_id"`
	CreatedAt       string         `json:"created_at"`
}

func issueRecordToResponse(rec db.IssueRecord) IssueRecordResponse {
	evidence := []string{}
	_ = json.Unmarshal(rec.Evidence, &evidence)
	var supersedes *int32
	if rec.SupersedesSeq.Valid {
		v := rec.SupersedesSeq.Int32
		supersedes = &v
	}
	return IssueRecordResponse{
		ID:              uuidToString(rec.ID),
		IssueID:         uuidToString(rec.IssueID),
		Seq:             rec.Seq,
		ClientRequestID: uuidToString(rec.ClientRequestID),
		Kind:            rec.Kind,
		Summary:         rec.Summary,
		Rationale:       rec.Rationale,
		Evidence:        evidence,
		Verdict:         rec.Verdict,
		Fields:          parseIssueMetadata(rec.Fields),
		SupersedesSeq:   supersedes,
		AuthorType:      rec.AuthorType,
		AuthorID:        uuidToString(rec.AuthorID),
		ActorSessionID:  uuidToPtr(rec.ActorSessionID),
		CreatedAt:       timestampToString(rec.CreatedAt),
	}
}

// validateIssueRecordRequest normalizes the request in place and returns the
// canonical evidence and fields JSON to store.
func validateIssueRecordRequest(req *CreateIssueRecordRequest) (evidence, fields []byte, err error) {
	verdicts, ok := issueRecordVerdicts[req.Kind]
	if !ok {
		return nil, nil, errors.New("kind must be decision or run")
	}
	if !verdicts[req.Verdict] {
		return nil, nil, fmt.Errorf("verdict %q is not valid for kind %s", req.Verdict, req.Kind)
	}
	if req.Summary == "" {
		return nil, nil, errors.New("summary is required")
	}
	if utf8.RuneCountInString(req.Summary) > maxIssueRecordSummary {
		return nil, nil, fmt.Errorf("summary exceeds %d characters", maxIssueRecordSummary)
	}
	if utf8.RuneCountInString(req.Rationale) > maxIssueRecordRationale {
		return nil, nil, fmt.Errorf("rationale exceeds %d characters", maxIssueRecordRationale)
	}
	if len(req.Evidence) > maxIssueRecordEvidence {
		return nil, nil, fmt.Errorf("evidence cannot exceed %d entries", maxIssueRecordEvidence)
	}
	if req.Evidence == nil {
		req.Evidence = []string{}
	}
	for _, e := range req.Evidence {
		if e == "" || utf8.RuneCountInString(e) > maxIssueRecordEvidenceLen {
			return nil, nil, fmt.Errorf("each evidence entry must be 1-%d characters", maxIssueRecordEvidenceLen)
		}
	}
	if req.SupersedesSeq != nil && *req.SupersedesSeq <= 0 {
		return nil, nil, errors.New("supersedes_seq must be positive")
	}
	fields = []byte("{}")
	if len(req.Fields) > 0 && string(req.Fields) != "null" {
		var parsed map[string]json.RawMessage
		if err := json.Unmarshal(req.Fields, &parsed); err != nil {
			return nil, nil, errors.New("fields must be a JSON object")
		}
		for k, v := range parsed {
			if err := validateIssueMetadataKey(k); err != nil {
				return nil, nil, errors.New("fields " + err.Error())
			}
			if err := validateIssueMetadataValue(v); err != nil {
				return nil, nil, fmt.Errorf("fields.%s: %w", k, err)
			}
		}
		if fields, err = json.Marshal(parsed); err != nil {
			return nil, nil, errors.New("fields are invalid")
		}
		if len(fields) > maxIssueRecordFieldsBytes {
			return nil, nil, fmt.Errorf("fields exceed %d bytes", maxIssueRecordFieldsBytes)
		}
	}
	evidence, err = json.Marshal(req.Evidence)
	return evidence, fields, err
}

// sameIssueRecordRequest decides whether a replayed request UUID describes the
// record already stored under it.
func sameIssueRecordRequest(rec db.IssueRecord, req CreateIssueRecordRequest) bool {
	if rec.Kind != req.Kind || rec.Summary != req.Summary || rec.Verdict != req.Verdict {
		return false
	}
	if req.SupersedesSeq == nil {
		return !rec.SupersedesSeq.Valid
	}
	return rec.SupersedesSeq.Valid && rec.SupersedesSeq.Int32 == *req.SupersedesSeq
}

func (h *Handler) ListIssueRecords(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	params := db.ListIssueRecordsParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID}
	if kind := r.URL.Query().Get("kind"); kind != "" {
		if _, ok := issueRecordVerdicts[kind]; !ok {
			writeError(w, http.StatusBadRequest, "kind must be decision or run")
			return
		}
		params.Kind = pgtype.Text{String: kind, Valid: true}
	}
	rows, err := h.Queries.ListIssueRecords(r.Context(), params)
	if err != nil {
		slog.Warn("ListIssueRecords failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to list records")
		return
	}
	records := make([]IssueRecordResponse, 0, len(rows))
	for _, row := range rows {
		records = append(records, issueRecordToResponse(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"records": records})
}

func (h *Handler) CreateIssueRecord(w http.ResponseWriter, r *http.Request) {
	r = h.withWakeupActor(r)
	var req CreateIssueRecordRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxIssueRecordBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	requestID, ok := parseUUIDOrBadRequest(w, req.ClientRequestID, "client_request_id")
	if !ok {
		return
	}
	evidence, fields, err := validateIssueRecordRequest(&req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := uuidToString(issue.WorkspaceID)
	actorType, actorID := h.resolveActor(r, userID, workspaceID)
	authorID, ok := parseUUIDOrBadRequest(w, actorID, "actor")
	if !ok {
		return
	}
	session, _ := externalActorSessionID(r)

	ctx := r.Context()
	tx, err := h.beginWakeupWrite(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to begin record write")
		return
	}
	defer tx.Rollback(ctx)
	q := h.Queries.WithTx(tx)
	if err := q.LockIssueRecordLedger(ctx, issue.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to lock record ledger")
		return
	}

	existing, err := q.GetIssueRecordByRequest(ctx, db.GetIssueRecordByRequestParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, ClientRequestID: requestID,
	})
	if err == nil {
		if !sameIssueRecordRequest(existing, req) {
			writeError(w, http.StatusConflict, "client_request_id was already used for a different record")
			return
		}
		writeJSON(w, http.StatusOK, issueRecordToResponse(existing))
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "failed to load record")
		return
	}

	next, err := q.NextIssueRecordSeq(ctx, db.NextIssueRecordSeqParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to allocate record sequence")
		return
	}
	if next > maxIssueRecords {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("an issue holds at most %d records", maxIssueRecords))
		return
	}
	var supersedes pgtype.Int4
	if req.SupersedesSeq != nil {
		target, err := q.GetIssueRecordBySeq(ctx, db.GetIssueRecordBySeqParams{
			WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, Seq: *req.SupersedesSeq,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusBadRequest, "supersedes_seq does not name a record on this issue")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to load superseded record")
			return
		}
		if target.Kind != req.Kind {
			writeError(w, http.StatusBadRequest, "a record can only supersede a record of the same kind")
			return
		}
		supersedes = pgtype.Int4{Int32: *req.SupersedesSeq, Valid: true}
	}

	rec, err := q.CreateIssueRecord(ctx, db.CreateIssueRecordParams{
		WorkspaceID:     issue.WorkspaceID,
		IssueID:         issue.ID,
		Seq:             next,
		ClientRequestID: requestID,
		Kind:            req.Kind,
		Summary:         req.Summary,
		Rationale:       req.Rationale,
		Evidence:        evidence,
		Verdict:         req.Verdict,
		Fields:          fields,
		SupersedesSeq:   supersedes,
		AuthorType:      actorType,
		AuthorID:        authorID,
		ActorSessionID:  session,
	})
	if err != nil {
		slog.Warn("CreateIssueRecord failed", append(logger.RequestAttrs(r), "error", err, "issue_id", uuidToString(issue.ID))...)
		writeError(w, http.StatusInternalServerError, "failed to create record")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to commit record")
		return
	}

	resp := issueRecordToResponse(rec)
	h.publish(protocol.EventIssueRecordCreated, workspaceID, actorType, actorID, map[string]any{
		"issue_id": resp.IssueID,
		"seq":      resp.Seq,
	})
	writeJSON(w, http.StatusCreated, resp)
}

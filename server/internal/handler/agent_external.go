package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// ExternalSessionResponse identifies a registered host session. id names the
// Multica session record used for issue bindings. host_id
// is the Paseo server/connection id (never a hostname) and session_id is the
// Paseo agent id (never the underlying Codex thread id). daemon_id names the
// authorized Multica daemon that delivers mailbox work locally.
type ExternalSessionResponse struct {
	ID          string  `json:"id,omitempty"`
	Provider    string  `json:"provider"`
	HostID      string  `json:"host_id"`
	SessionID   string  `json:"session_id"`
	DisplayName string  `json:"display_name"`
	URL         *string `json:"url"`
	DaemonID    string  `json:"daemon_id"`
}

func externalSessionToResponse(s db.ExternalAgentSession) *ExternalSessionResponse {
	return &ExternalSessionResponse{
		ID:          uuidToString(s.ID),
		Provider:    s.Provider,
		HostID:      s.HostID,
		SessionID:   s.SessionID,
		DisplayName: s.DisplayName,
		URL:         textToPtr(s.Url),
		DaemonID:    s.DaemonID,
	}
}

// externalSessionsByAgent loads the session projection for a page of agents in
// one query. A read failure degrades to no session detail rather than failing
// the whole agent list; the identity still resolves as external through
// execution_kind.
func (h *Handler) externalSessionsByAgent(ctx context.Context, agentIDs []pgtype.UUID) (map[string]*ExternalSessionResponse, map[string]int) {
	out := map[string]*ExternalSessionResponse{}
	counts := map[string]int{}
	if len(agentIDs) == 0 {
		return out, counts
	}
	rows, err := h.Queries.ListExternalAgentSessionsByAgentIDs(ctx, agentIDs)
	if err != nil {
		return out, counts
	}
	for _, row := range rows {
		agentID := uuidToString(row.AgentID)
		out[agentID] = externalSessionToResponse(row)
		counts[agentID]++
	}
	return out, counts
}

// attachExternalSession enriches one response. Used by detail/create/update
// paths where a single extra read is cheaper than a batch.
func (h *Handler) attachExternalSession(ctx context.Context, resp *AgentResponse) {
	if resp == nil || resp.ExecutionKind != "external" {
		return
	}
	if session, err := h.Queries.GetExternalAgentSessionByAgentID(ctx, parseUUID(resp.ID)); err == nil {
		resp.ExternalSession = externalSessionToResponse(session)
	}
	if rows, err := h.Queries.ListExternalAgentSessionsByAgentIDs(ctx, []pgtype.UUID{parseUUID(resp.ID)}); err == nil {
		resp.ExternalSessionCount = len(rows)
	}
}

// externalSessionInput is the request half of the frozen external_session DTO.
// It deliberately does not accept an agent or runtime id: the session tuple is
// the identity and the server mints everything else.
type externalSessionInput struct {
	Provider    string  `json:"provider"`
	HostID      string  `json:"host_id"`
	SessionID   string  `json:"session_id"`
	DisplayName string  `json:"display_name"`
	URL         *string `json:"url"`
	DaemonID    string  `json:"daemon_id"`
}

type createExternalAgentRequest struct {
	// AgentID attaches another host session to an existing external agent.
	// Omitted, registration mints a new agent identity, as before.
	AgentID string `json:"agent_id,omitempty"`
	// Name is the session/agent display name. Legacy registration uses it as
	// the agent name; template registration uses TemplateName for the host
	// identity and keeps this as the session's own title.
	Name string `json:"name"`
	// TemplateName is the host template's display name. It never includes the
	// session title, so legacy servers (which ignore this field) keep minting
	// one distinctly named agent per session.
	TemplateName    string               `json:"template_name,omitempty"`
	ExternalSession externalSessionInput `json:"external_session"`
	PermissionMode  *string              `json:"permission_mode"`
	// Template attaches a NEW host session to the host template keyed by
	// (workspace, owner, provider, host instance) instead of minting one agent
	// per session. Existing sessions are immutable: a repeat connect returns
	// its original agent even when it is a legacy per-session agent.
	Template bool `json:"template,omitempty"`
}

var externalSessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,199}$`)

const (
	maxExternalAgentNameLength  = 120
	maxExternalDisplayNameRunes = 200
)

// validatePaseoSessionURL accepts exactly
// paseo://h/<serverId>/agent/<paseoAgentId> and nothing else. Credentials,
// query strings, fragments, extra or empty path segments are rejected so a
// stored link can never be a disguised arbitrary URL.
func validatePaseoSessionURL(raw string) (*string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, "external_session.url is not a valid URL"
	}
	if u.Scheme != "paseo" {
		return nil, "external_session.url must use the paseo scheme"
	}
	if u.Opaque != "" || u.User != nil || u.Host != "h" || u.RawQuery != "" || u.Fragment != "" {
		return nil, "external_session.url must be paseo://h/<serverId>/agent/<agentId>"
	}
	segments := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(segments) != 3 || segments[1] != "agent" || segments[0] == "" || segments[2] == "" {
		return nil, "external_session.url must be paseo://h/<serverId>/agent/<agentId>"
	}
	for _, segment := range segments {
		if segment == "." || segment == ".." || strings.ContainsAny(segment, " \t\r\n\\") {
			return nil, "external_session.url must be paseo://h/<serverId>/agent/<agentId>"
		}
	}
	return &raw, ""
}

func (h *Handler) GetOwnPaseoSession(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	workspaceUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}
	ownerID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireWorkspaceRole(w, r, workspaceID, "workspace not found", "owner", "admin", "member"); !ok {
		return
	}
	hostID, sessionID := r.URL.Query().Get("host_id"), r.URL.Query().Get("session_id")
	if !externalSessionIDPattern.MatchString(hostID) || !externalSessionIDPattern.MatchString(sessionID) {
		writeError(w, http.StatusBadRequest, "host_id and session_id must be Paseo locators")
		return
	}
	session, err := h.Queries.GetExternalAgentSessionByIdentity(r.Context(), db.GetExternalAgentSessionByIdentityParams{
		WorkspaceID: workspaceUUID, OwnerUserID: parseUUID(ownerID), Provider: "paseo", HostID: hostID, SessionID: sessionID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, nil)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to look up external session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"agent_id": uuidToString(session.AgentID), "session_id": uuidToString(session.ID), "daemon_id": session.DaemonID,
	})
}

// sanitizeExternalDisplayName strips control characters and bounds the rune
// count. The value is presentation only; identity never depends on it.
func sanitizeExternalDisplayName(raw, fallback string) string {
	name := strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, raw))
	if name == "" {
		name = fallback
	}
	if utf8.RuneCountInString(name) > maxExternalDisplayNameRunes {
		name = string([]rune(name)[:maxExternalDisplayNameRunes])
	}
	return name
}

// CreateExternalAgent registers one host session and mints the ordinary Agent
// identity that authors its comments. Registration does not start any worker
// and binds no runtime: the host owns execution.
func (h *Handler) CreateExternalAgent(w http.ResponseWriter, r *http.Request) {
	if isMachineCredentialActor(r) {
		writeError(w, http.StatusForbidden, "this endpoint is only available to human actors")
		return
	}
	workspaceID := h.resolveWorkspaceID(r)
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}
	ownerID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	// Registering one's own host session is ordinary member work, like
	// creating a native agent. Management and token issuance keep their
	// existing owner/admin gate (canManageAgent); this endpoint grants no
	// authority over another member's agents.
	if _, ok := h.requireWorkspaceRole(w, r, workspaceID, "workspace not found", "owner", "admin", "member"); !ok {
		return
	}

	var req createExternalAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if req.AgentID == "" && name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Template && req.AgentID != "" {
		writeError(w, http.StatusBadRequest, "agent_id cannot be combined with template registration")
		return
	}
	if req.Template && !h.cfg.ExternalAgentTemplatesEnabled {
		// Template writes are a rollout switch, not a silent capability. A
		// template-capable client must know it is talking to a server that
		// has not opened them yet, so the write is refused before anything
		// else is validated or created.
		writeErrorCode(w, http.StatusBadRequest, "external_agent_templates_disabled", "external agent templates are not enabled on this server")
		return
	}
	if utf8.RuneCountInString(name) > maxExternalAgentNameLength {
		writeError(w, http.StatusBadRequest, "name is too long")
		return
	}
	templateName := strings.TrimSpace(req.TemplateName)
	if req.Template {
		if templateName == "" {
			templateName = name
		}
		if utf8.RuneCountInString(templateName) > maxExternalAgentNameLength {
			writeError(w, http.StatusBadRequest, "template_name is too long")
			return
		}
	}

	session := req.ExternalSession
	session.Provider = strings.TrimSpace(session.Provider)
	if session.Provider == "" {
		session.Provider = "paseo"
	}
	if session.Provider != "paseo" {
		writeError(w, http.StatusBadRequest, "external_session.provider must be 'paseo'")
		return
	}
	session.HostID = strings.TrimSpace(session.HostID)
	session.SessionID = strings.TrimSpace(session.SessionID)
	session.DaemonID = strings.TrimSpace(session.DaemonID)
	if !externalSessionIDPattern.MatchString(session.HostID) {
		writeError(w, http.StatusBadRequest, "external_session.host_id is invalid")
		return
	}
	if !externalSessionIDPattern.MatchString(session.SessionID) {
		writeError(w, http.StatusBadRequest, "external_session.session_id is invalid")
		return
	}
	if !externalSessionIDPattern.MatchString(session.DaemonID) {
		writeError(w, http.StatusBadRequest, "external_session.daemon_id is invalid")
		return
	}
	link, msg := validatePaseoSessionURL(valueOrEmpty(session.URL))
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if link != nil {
		parsed, _ := url.Parse(*link)
		if parsed.Path != "/"+session.HostID+"/agent/"+session.SessionID {
			writeError(w, http.StatusBadRequest, "external_session.url does not match its host and session")
			return
		}
	}
	session.URL = link
	displayName := sanitizeExternalDisplayName(session.DisplayName, name)

	daemonOK, err := h.Queries.WorkspaceDaemonOwnedByUser(r.Context(), db.WorkspaceDaemonOwnedByUserParams{
		WorkspaceID: wsUUID,
		DaemonID:    pgtype.Text{String: session.DaemonID, Valid: true},
		OwnerUserID: parseUUID(ownerID),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to validate daemon_id")
		return
	}
	if !daemonOK {
		writeError(w, http.StatusBadRequest, "external_session.daemon_id must be owned by the registering user in this workspace")
		return
	}

	permissionMode := "private"
	visibility := "private"
	if req.PermissionMode != nil {
		switch *req.PermissionMode {
		case "private":
		case "public_to":
			permissionMode = "public_to"
			visibility = "workspace"
		default:
			writeError(w, http.StatusBadRequest, "permission_mode must be 'private' or 'public_to'")
			return
		}
	}

	// Adding a session to an existing external agent is the only way one agent
	// identity owns several physical sessions. The agent stays the same
	// identity; only its set of addresses grows.
	var targetAgent db.Agent
	if req.AgentID != "" {
		existingAgentID, parseErr := util.ParseUUID(strings.TrimSpace(req.AgentID))
		if parseErr != nil {
			writeError(w, http.StatusBadRequest, "agent_id is not a valid uuid")
			return
		}
		targetAgent, err = h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{ID: existingAgentID, WorkspaceID: wsUUID})
		if err != nil {
			writeError(w, http.StatusNotFound, "agent not found")
			return
		}
		if targetAgent.ExecutionKind != "external" {
			writeError(w, http.StatusBadRequest, "only external agents can own host sessions")
			return
		}
		if targetAgent.ArchivedAt.Valid {
			writeError(w, http.StatusConflict, "archived agents cannot register a session")
			return
		}
		// A template is the identity of ONE host instance. Attaching a session
		// that reports another host would silently merge two hosts under one
		// template, which the key deliberately keeps apart.
		if targetAgent.ExternalProvider.Valid && (targetAgent.ExternalProvider.String != session.Provider || targetAgent.ExternalHostID.String != session.HostID) {
			writeError(w, http.StatusBadRequest, "external session belongs to a different host template")
			return
		}
		if !h.canManageAgent(w, r, targetAgent) {
			return
		}
		if name == "" {
			name = targetAgent.Name
		}
	}

	// Repeat registration of the same session returns the same identity, never
	// a second agent. Only presentation fields may move.
	if existing, err := h.Queries.GetExternalAgentSessionByIdentity(r.Context(), db.GetExternalAgentSessionByIdentityParams{
		WorkspaceID: wsUUID,
		OwnerUserID: parseUUID(ownerID),
		Provider:    session.Provider,
		HostID:      session.HostID,
		SessionID:   session.SessionID,
	}); err == nil {
		h.writeExistingExternalAgent(w, r, existing, targetAgent.ID, displayName, link, session.DaemonID)
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "failed to resolve external session")
		return
	}

	// The host template is resolved BEFORE the session transaction. A unique
	// violation inside a transaction aborts it, and the losing side of a
	// concurrent first connect must be able to re-read the winner's row in the
	// same call; an auto-commit insert leaves the connection usable. The
	// template row is an idempotent host identity, so it can safely outlive a
	// session write that is later refused.
	var resolvedTemplate db.Agent
	templateCreated := false
	if req.Template {
		resolvedTemplate, templateCreated, err = h.resolveExternalTemplate(r.Context(), h.Queries, wsUUID, parseUUID(ownerID), session.Provider, session.HostID, templateName, permissionMode, visibility)
		if err != nil {
			h.writeExternalTemplateError(w, err)
			return
		}
		// The template row is created before the session transaction, so its
		// invocation targets must be written in the same window: a later
		// refused session write would otherwise leave a public_to template
		// without targets, and a retry no longer counts as "created".
		if templateCreated && permissionMode == "public_to" {
			if err := replaceInvocationTargetsWithQueries(r.Context(), h.Queries, resolvedTemplate.ID, parseUUID(ownerID), []targetSpec{{
				targetType: invocationTargetWorkspace,
				targetID:   wsUUID,
			}}); err != nil {
				writeError(w, http.StatusInternalServerError, "failed to save agent access")
				return
			}
		}
	}

	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to start external agent create")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)

	if _, err := qtx.LockWorkspaceForChatSessionCreate(r.Context(), wsUUID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to lock workspace")
		return
	}
	// Re-check under the workspace lock: a concurrent register of the same
	// session may have committed between the read above and now.
	if existing, err := qtx.GetExternalAgentSessionByIdentity(r.Context(), db.GetExternalAgentSessionByIdentityParams{
		WorkspaceID: wsUUID,
		OwnerUserID: parseUUID(ownerID),
		Provider:    session.Provider,
		HostID:      session.HostID,
		SessionID:   session.SessionID,
	}); err == nil {
		if err := tx.Commit(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to resolve external session")
			return
		}
		h.writeExistingExternalAgent(w, r, existing, targetAgent.ID, displayName, link, session.DaemonID)
		return
	}

	created := targetAgent
	if req.Template {
		if !created.ID.Valid {
			created = resolvedTemplate
		}
	} else if !created.ID.Valid {
		created, err = qtx.CreateExternalAgent(r.Context(), db.CreateExternalAgentParams{
			WorkspaceID:    wsUUID,
			Name:           name,
			Visibility:     visibility,
			PermissionMode: permissionMode,
			OwnerID:        parseUUID(ownerID),
		})
		if err != nil {
			if isUniqueViolation(err) {
				writeError(w, http.StatusConflict, "an agent with this name already exists in this workspace")
				return
			}
			writeError(w, http.StatusInternalServerError, "failed to create external agent")
			return
		}
	}
	createdSession, err := qtx.CreateExternalAgentSession(r.Context(), db.CreateExternalAgentSessionParams{
		ID:          dbid.NewV7(),
		WorkspaceID: wsUUID,
		OwnerUserID: parseUUID(ownerID),
		AgentID:     created.ID,
		Provider:    session.Provider,
		HostID:      session.HostID,
		SessionID:   session.SessionID,
		DisplayName: displayName,
		Url:         ptrToText(link),
		DaemonID:    session.DaemonID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// A concurrent registration won the unique identity index. Its agent
		// is the identity; discard ours.
		_ = tx.Rollback(r.Context())
		existing, getErr := h.Queries.GetExternalAgentSessionByIdentity(r.Context(), db.GetExternalAgentSessionByIdentityParams{
			WorkspaceID: wsUUID,
			OwnerUserID: parseUUID(ownerID),
			Provider:    session.Provider,
			HostID:      session.HostID,
			SessionID:   session.SessionID,
		})
		if getErr != nil {
			writeError(w, http.StatusConflict, "external session was registered concurrently; retry")
			return
		}
		h.writeExistingExternalAgent(w, r, existing, targetAgent.ID, displayName, link, session.DaemonID)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create external session")
		return
	}
	if permissionMode == "public_to" && !targetAgent.ID.Valid && !req.Template {
		if err := replaceInvocationTargetsWithQueries(r.Context(), qtx, created.ID, parseUUID(ownerID), []targetSpec{{
			targetType: invocationTargetWorkspace,
			targetID:   wsUUID,
		}}); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to save agent access")
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to commit external agent create")
		return
	}

	resp := h.agentToResponse(created)
	resp.ExternalSession = externalSessionToResponse(createdSession)
	enrichInvocationTargets := func(resp *AgentResponse) {
		if err := h.enrichAgentResponseWithTargets(r.Context(), resp, created.ID); err != nil {
			resp.InvocationTargets = []AgentInvocationTargetDTO{}
		}
	}
	enrichInvocationTargets(&resp)
	redactAgentResponseForActor(&resp, "member")
	writeJSON(w, http.StatusCreated, resp)
}

// writeExistingExternalAgent answers a repeat registration of one physical
// host session. requestedAgentID is the agent the caller explicitly asked to
// extend; a session identity is immutable, so naming a different agent is a
// conflict rather than a silent move of the session to that agent.
func (h *Handler) writeExistingExternalAgent(w http.ResponseWriter, r *http.Request, session db.ExternalAgentSession, requestedAgentID pgtype.UUID, displayName string, link *string, daemonID string) {
	if session.RevokedAt.Valid {
		writeError(w, http.StatusConflict, "external session is revoked; register a new host session")
		return
	}
	if requestedAgentID.Valid && requestedAgentID != session.AgentID {
		writeError(w, http.StatusConflict, "external session is already registered to another agent")
		return
	}
	agent, err := h.Queries.GetAgent(r.Context(), session.AgentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load external agent")
		return
	}
	// The session id scopes the write. An agent may own several host sessions,
	// and re-registering one must leave every sibling's profile and daemon as
	// they were.
	updatedSession, err := h.Queries.UpdateExternalAgentSessionProfile(r.Context(), db.UpdateExternalAgentSessionProfileParams{
		ID:          session.ID,
		WorkspaceID: session.WorkspaceID,
		DisplayName: displayName,
		Url:         ptrToText(link),
		DaemonID:    daemonID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update external session profile")
		return
	}
	resp := h.agentToResponse(agent)
	resp.ExternalSession = externalSessionToResponse(updatedSession)
	if err := h.enrichAgentResponseWithTargets(r.Context(), &resp, agent.ID); err != nil {
		resp.InvocationTargets = []AgentInvocationTargetDTO{}
	}
	writeJSON(w, http.StatusOK, resp)
}

type createExternalAgentTokenRequest struct {
	ExpiresAt string `json:"expires_at"`
	// ExternalSessionID names the exact registered session this credential
	// acts for. It is required when the target agent has more than one live
	// session; the single-session case may omit it because the server can prove
	// the identity it names.
	ExternalSessionID string `json:"external_session_id,omitempty"`
}

type createExternalAgentTokenResponse struct {
	ID                string `json:"id"`
	Token             string `json:"token"`
	ExpiresAt         string `json:"expires_at"`
	ExternalSessionID string `json:"external_session_id,omitempty"`
}

const maxExternalAgentTokenLifetime = 366 * 24 * time.Hour

// CreateExternalAgentToken mints an independently revocable machine credential
// for one external agent. The raw token is returned exactly once; only its
// hash is persisted.
func (h *Handler) CreateExternalAgentToken(w http.ResponseWriter, r *http.Request) {
	agent, ok := h.loadExternalAgentForHumanAdmin(w, r)
	if !ok {
		return
	}
	if agent.ArchivedAt.Valid {
		writeError(w, http.StatusConflict, "archived agents cannot receive credentials")
		return
	}
	var req createExternalAgentTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	expiresAt, err := time.Parse(time.RFC3339, strings.TrimSpace(req.ExpiresAt))
	if err != nil {
		writeError(w, http.StatusBadRequest, "expires_at must be an RFC3339 timestamp")
		return
	}
	now := time.Now()
	if !expiresAt.After(now.Add(time.Minute)) {
		writeError(w, http.StatusBadRequest, "expires_at must be in the future")
		return
	}
	if expiresAt.After(now.Add(maxExternalAgentTokenLifetime)) {
		writeError(w, http.StatusBadRequest, "expires_at is too far in the future")
		return
	}
	sessionID, ok := h.resolveTokenSession(w, r, agent, req.ExternalSessionID)
	if !ok {
		return
	}
	raw, err := auth.GenerateExternalAgentToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}
	// The credential acts with the AGENT OWNER's authority, never the minting
	// admin's. An owner/admin may rotate another member's credential, but that
	// must not lend the admin's identity or invocation rights to the external
	// session; the auth path re-checks the owner's membership every request.
	created, err := h.Queries.CreateExternalAgentToken(r.Context(), db.CreateExternalAgentTokenParams{
		ID:                dbid.NewV7(),
		AgentID:           agent.ID,
		WorkspaceID:       agent.WorkspaceID,
		OwnerUserID:       agent.OwnerID,
		TokenHash:         auth.HashToken(raw),
		ExpiresAt:         pgtype.Timestamptz{Time: expiresAt.UTC(), Valid: true},
		ExternalSessionID: sessionID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create token")
		return
	}
	writeJSON(w, http.StatusCreated, createExternalAgentTokenResponse{
		ID:                uuidToString(created.ID),
		Token:             raw,
		ExpiresAt:         timestampToString(created.ExpiresAt),
		ExternalSessionID: uuidToString(created.ExternalSessionID),
	})
}

// RevokeExternalAgentToken revokes one token without touching the agent's other
// credentials. 204 is returned for an already-revoked token so a retry is
// idempotent.
func (h *Handler) RevokeExternalAgentToken(w http.ResponseWriter, r *http.Request) {
	agent, ok := h.loadExternalAgentForHumanAdmin(w, r)
	if !ok {
		return
	}
	tokenID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "tokenId"), "token id")
	if !ok {
		return
	}
	_, err := h.Queries.RevokeExternalAgentToken(r.Context(), db.RevokeExternalAgentTokenParams{
		ID:          tokenID,
		AgentID:     agent.ID,
		WorkspaceID: agent.WorkspaceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Either the token never existed or it was already revoked. Both
			// leave the caller with a dead credential.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to revoke token")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RevokeExternalAgentSession revokes one registered session without touching
// its siblings, the agent, or its history. 204 is returned for an
// already-revoked session so a retry is idempotent.
func (h *Handler) RevokeExternalAgentSession(w http.ResponseWriter, r *http.Request) {
	agent, ok := h.loadExternalAgentForHumanAdmin(w, r)
	if !ok {
		return
	}
	sessionID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "sessionId"), "session id")
	if !ok {
		return
	}
	_, err := h.Queries.RevokeExternalAgentSession(r.Context(), db.RevokeExternalAgentSessionParams{
		ID:          sessionID,
		AgentID:     agent.ID,
		WorkspaceID: agent.WorkspaceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "external session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to revoke session")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// loadExternalAgentForHumanAdmin resolves {id} to a live external agent and
// enforces the human owner/admin rule. Machine credentials never reach here:
// the route is wrapped in RequireHumanActor and this backstop repeats it.
func (h *Handler) loadExternalAgentForHumanAdmin(w http.ResponseWriter, r *http.Request) (db.Agent, bool) {
	if isMachineCredentialActor(r) {
		writeError(w, http.StatusForbidden, "this endpoint is only available to human actors")
		return db.Agent{}, false
	}
	workspaceID := h.resolveWorkspaceID(r)
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return db.Agent{}, false
	}
	agentID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "agent id")
	if !ok {
		return db.Agent{}, false
	}
	agent, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{
		ID:          agentID,
		WorkspaceID: wsUUID,
	})
	if err != nil || agent.ExecutionKind != "external" {
		writeError(w, http.StatusNotFound, "external agent not found")
		return db.Agent{}, false
	}
	if !h.canManageAgent(w, r, agent) {
		return db.Agent{}, false
	}
	return agent, true
}

func valueOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

import type { Label } from "./label";
import type { IssuePropertyValues } from "./property";
import type { Agent, ExternalSession } from "./agent";
import type { ContractView } from "../api/issue-contract-schema";

/** Four lifecycle classifications. User-facing columns group by status key. */
export type IssueStatusCategory =
  | "unstarted"
  | "started"
  | "done"
  | "closed";

/** The seven built-in status keys kept for issue/API compatibility. */
export type BuiltInIssueStatus =
  | "backlog"
  | "todo"
  | "in_progress"
  | "in_review"
  | "done"
  | "blocked"
  | "cancelled";

/**
 * A status KEY as stored on the issue: one of the 7 built-ins, or a custom key
 * an admin defined for this workspace.
 *
 * OPEN by design. `(string & {})` keeps editor autocomplete for the 7 built-ins
 * while accepting any catalog key, which is what the server has always been
 * able to send. Anything that needs presentation (label, colour, board column)
 * must resolve the key to its CATEGORY first — `useIssueStatuses(wsId)` in a
 * component, `statusCategoryOfKey` in a pure path. (MUL-6243)
 */
export type IssueStatus = BuiltInIssueStatus | (string & {});

export type IssuePriority = "urgent" | "high" | "medium" | "low" | "none";

export type IssueAssigneeType = "member" | "agent" | "squad";

export interface IssueReaction {
  id: string;
  issue_id: string;
  actor_type: string;
  actor_id: string;
  emoji: string;
  created_at: string;
  issue_revision?: number;
}

/**
 * Per-issue metadata is a flat KV map agents use to record pipeline state
 * (PR number, pipeline_status, waiting_on, ...). Values are primitives only —
 * string / number / bool — enforced by both the API and the DB. Always
 * present in responses (empty object when unset) so reads don't need a
 * nil guard on the parent field.
 */
export type IssueMetadataValue = string | number | boolean;
export type IssueMetadata = Record<string, IssueMetadataValue>;

export interface SourceContextAttachment {
  id: string;
  source_attachment_id?: string;
  owner_type: "issue" | "comment" | (string & {});
  owner_id: string;
  filename: string;
  content_type: string;
  size_bytes: number;
  created_at: string;
}

export interface SourceContextAuthor {
  type: "member" | "agent" | (string & {});
  id: string;
  name: string;
}

export interface SourceContextIssueSnapshot {
  id: string;
  identifier: string;
  number: number;
  title: string;
  description: string | null;
  created_at: string;
  updated_at: string;
  revision: number;
  attachments: SourceContextAttachment[];
}

export interface SourceContextCommentSnapshot {
  id: string;
  parent_id: string | null;
  type: string;
  content: string;
  author: SourceContextAuthor;
  created_at: string;
  updated_at: string;
  revision: number;
  attachments: SourceContextAttachment[];
  /** A comment deleted while it had replies: kept, empty, so they keep their parent. */
  deleted?: boolean;
}

export interface SourceContextSnapshot {
  /** Capture metadata is present on a persisted detail snapshot and omitted
   * from the pre-submit preview payload. */
  version?: number;
  captured_by_user_id?: string;
  captured_at?: string;
  source_issue: SourceContextIssueSnapshot;
  comment_thread: SourceContextCommentSnapshot[];
  anchor_comment_id: string;
}

export interface SourceContextLimitUsage {
  comment_count: number;
  text_bytes: number;
  attachment_count: number;
  attachment_bytes: number;
}

export interface SourceContextPreview extends SourceContextSnapshot {
  capture_token: string;
  limits: SourceContextLimitUsage;
}

export interface SourceContextAuthorState {
  type: string;
  id: string;
  captured_name: string;
  current_name?: string;
  state: string;
}

export interface SourceContextDescriptionAttachmentChange {
  kind: "added" | "removed" | "replaced" | (string & {});
  attachment_id: string;
  filename: string;
  previous_filename?: string;
}

export interface SourceContextChangeDetails {
  changed_comment_ids: string[];
  added_comments?: SourceContextCommentSnapshot[];
  removed_comment_ids?: string[];
  description_attachment_changes: SourceContextDescriptionAttachmentChange[];
}

export interface IssueSourceContext {
  id: string;
  version: number;
  usage: "read_only_historical_background" | (string & {});
  captured_at: string;
  display_state: "unchanged" | "changed" | "deleted" | "unavailable" | (string & {});
  source_issue_state: "unchanged" | "changed" | "deleted" | "unavailable" | (string & {});
  comment_thread_state: "unchanged" | "changed" | "unavailable" | (string & {});
  anchor_comment_state: "available" | "deleted" | "unavailable" | (string & {});
  can_open_current_source: boolean;
  change_reasons?: string[];
  change_details?: SourceContextChangeDetails;
  current_source?: { issue_id: string; identifier: string; anchor_comment_id: string };
  source_author_state?: SourceContextAuthorState[];
  snapshot: SourceContextSnapshot;
}

/** The original a duplicate points at: enough to link it and show its status. */
export interface IssueDuplicateOf {
  id: string;
  identifier: string;
  title: string;
  status: IssueStatus;
}

/** The fixed upper-level coordinator recorded at dispatch time (WORK-9). */
export interface IssueCoordinator {
  type: "member" | "agent" | (string & {});
  id: string;
}

/**
 * Merge policy projection. It describes what the issue authorized; it is not
 * proof that a merge happened, and absence means disallowed.
 */
export interface IssueMergeAuthorization {
  allowed: boolean;
  repository_url: string | null;
  target_branch: string | null;
  granted_by: string | null;
  granted_at: string | null;
}

/** Delivery is about the host mailbox, not about execution progress. */
export type MailboxDeliveryStatus =
  | "pending"
  | "leased"
  | "delivered"
  | "failed"
  | "unknown";

/** Read/handled are acknowledgements by the recipient, not run status. */
export type MailboxProcessingStatus = "unread" | "read" | "handled" | "unknown";

/** One mailbox delivery of a native comment to an external agent session. */
export interface MailboxMessage {
  payload?: Record<string, unknown>;
  handled_by_task_id?: string;
  id: string;
  issue_id: string;
  /**
   * Empty for a generic delivery. A generic notification has no comment and
   * must never be shown as if it carried one.
   */
  comment_id: string;
  recipient_agent_id: string;
  /** The session that sent the notification, when the producer had one. */
  source_session: ExternalSession | null;
  /**
   * The session frozen at enqueue time. It never follows a later rebind, so an
   * old notification keeps the address it was actually sent to.
   */
  target_session?: ExternalSession | null;
  /** Producer family, for example `comment` or `issue_wakeup`. */
  source_type?: string;
  /** Notification kind, for example `comment_mention` or `issue_wakeup`. */
  kind?: string;
  delivery_status: MailboxDeliveryStatus;
  processing_status: MailboxProcessingStatus;
  created_at: string;
  delivered_at: string | null;
  last_error: string | null;
}

/**
 * The active (agent, issue) target used for FUTURE notifications. A saved
 * binding never moves an existing delivery; each message keeps its frozen
 * target.
 */
export interface IssueSessionBinding {
  id: string;
  agent_id: string;
  issue_id: string;
  session: ExternalSession | null;
  /** False when the agent is archived or the session owner left the workspace. */
  deliverable: boolean;
  /** `agent_archived` or `owner_not_member` when not deliverable; "" otherwise. */
  unavailable_reason?: string;
  created_at: string;
}

export interface Issue {
  contract?: ContractView;
  /**
   * Task-level leadership root (WORK-269). Only squad task chains carry one;
   * absent on older servers and on ordinary issues, so readers must treat it
   * as "no task leader" rather than rebuild a global default.
   */
  leadership_root_issue_id?: string;
  /** The squad whose roster owns the task chain, when it has a root. */
  leadership_squad_id?: string;
  /** The agent currently responsible for the task chain, when it has a root. */
  leader_agent_id?: string;
  /** Set only when the current task leader is an external host session. */
  leader_external_session_id?: string;
  /** Monotonic leadership revision; a handoff increments it. */
  leadership_revision?: number;
  /** The root issue's own revision, the CAS value a handoff must present. */
  leadership_root_revision?: number;
  id: string;
  workspace_id: string;
  number: number;
  identifier: string;
  title: string;
  description: string | null;
  status: IssueStatus;
  /**
   * The category `status` belongs to, when the endpoint resolved it. Optional
   * because older backends did not emit it. Built-in keys map to a category
   * locally; use `issueStatusCategory(issue)` rather than reading this directly.
   */
  status_category?: IssueStatusCategory;
  /**
   * A CUSTOM status's display name, carried beside the key. Empty for the 7
   * built-ins, which are localized from the key — prefer `useStatusLabel`,
   * which handles both and stays correct when an admin renames a status.
   *
   * Optional only for compatibility with a server that predates it; a current
   * server always sends the field. (MUL-6749)
   */
  status_name?: string;
  priority: IssuePriority;
  assignee_type: IssueAssigneeType | null;
  assignee_id: string | null;
  creator_type: IssueAssigneeType;
  creator_id: string;
  parent_issue_id: string | null;
  /**
   * The original this issue duplicates (MUL-7349): present only while the
   * issue is cancelled and the original still exists, resolved by the server.
   * Absent when connected to an older backend.
   */
  duplicate_of?: IssueDuplicateOf | null;
  project_id: string | null;
  position: number;
  // Ordered barrier group among sibling sub-issues (null = unstaged). The
  // parent assignee is woken when a stage closes while a later one waits;
  // see server/internal/service/issue_wakeup_system.go.
  stage: number | null;
  // Calendar days as date-only "YYYY-MM-DD" (no time, no timezone). Use the
  // helpers in @multica/core/issues/date to format/compare — never `new Date()`
  // + local formatting, which shifts the day by the viewer's offset.
  start_date: string | null;
  due_date: string | null;
  metadata: IssueMetadata;
  // Custom property values keyed by property definition id. Always present
  // in responses (empty object when unset), mirroring `metadata`.
  properties: IssuePropertyValues;
  reactions?: IssueReaction[];
  labels?: Label[];
  created_at: string;
  updated_at: string;
  /** Monotonic server revision; absent when connected to an older backend. */
  revision?: number;
  /**
   * Null until the server's historical activity backfill reaches this row.
   * This RFC3339 timestamp may include sub-second precision while legacy
   * created_at/updated_at values are second-precision; parse before comparing.
   */
  last_activity_at?: string | null;
  /** Present only on issue detail responses for issues created from a comment. */
  source_context?: IssueSourceContext;
  /**
   * Fixed coordinator selected at dispatch. Additive: absent on older servers
   * and on issues created before the coordinator existed.
   */
  coordinator?: IssueCoordinator | null;
  /** Projected merge policy; absent means no authorization is recorded. */
  merge_authorization?: IssueMergeAuthorization;
  /**
   * External agents that participated in this issue, from the server's
   * projection rather than inferred from one comment page.
   */
  external_participants?: Agent[];
  /** Bounded recent deliveries (server caps this; mailbox API paginates). */
  mailbox_deliveries?: MailboxMessage[];
  /** Active targets for future notifications, one per external agent. */
  external_session_bindings?: IssueSessionBinding[];
}

/** Parameters for an explicit task-leader handoff. */
export interface IssueLeadershipHandoffRequest {
  client_request_id: string;
  /** The root's leadership revision the caller saw. */
  expected_revision: number;
  /** The root issue's own revision the caller saw. */
  expected_issue_revision: number;
  previous_agent_id: string;
  previous_session_id?: string;
  agent_id: string;
  /** Required for an external target, omitted for a managed one. */
  session_id?: string;
  reason: string;
}

export interface IssueLeadershipAdvancedReturn {
  issue_id: string;
  previous_return_id?: string;
  current_return_id?: string;
}

/**
 * Result of one handoff. Contracts listed here are now stale (the acceptor
 * side must revise them); earlier results stay with the previous leader and
 * need an explicit result takeover.
 */
export interface IssueLeadershipHandoffResponse {
  root_issue_id: string;
  revision: number;
  issue_revision: number;
  stale_contract_ids: string[];
  advanced_returns: IssueLeadershipAdvancedReturn[];
  historical_results: string;
}

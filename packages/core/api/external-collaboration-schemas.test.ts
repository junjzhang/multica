import { parseWithFallback } from "./schema";
import { QueryClient } from "@tanstack/react-query";
import { onIssueUpdated } from "../issues/ws-updaters";
import { issueKeys } from "../issues/queries";
import type { Issue } from "../types";
import { describe, expect, it } from "vitest";
import {
  AgentListSchema,
  AgentSchema,
  CommentSchema,
  CommentTriggerPreviewSchema,
  IssueSchema,
  ListIssuesResponseSchema,
  MailboxMessageListSchema,
  AgentSessionListResponseSchema,
  IssueSessionBindingListSchema,
  AgentSessionBindingResponseSchema,
  TimelineEntriesSchema,
} from "./schemas";
import { agentExecutionKind, mentionDispatchKind } from "../agents/execution-kind";

const baseIssue = {
  id: "11111111-1111-1111-1111-111111111111",
  workspace_id: "ws-1",
  number: 1,
  identifier: "MUL-1",
  title: "Test",
  description: null,
  status: "todo",
  priority: "medium",
  assignee_type: null,
  assignee_id: null,
  creator_type: "member",
  creator_id: "user-1",
  parent_issue_id: null,
  project_id: null,
  position: 0,
  stage: null,
  start_date: null,
  due_date: null,
  metadata: {},
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

const externalSession = {
  provider: "paseo",
  host_id: "server-1",
  session_id: "agent-1",
  display_name: "Reviewer session",
  url: "paseo://h/server-1/agent/agent-1",
  daemon_id: "daemon-1",
};

const externalAgent = {
  id: "agent-1",
  workspace_id: "ws-1",
  name: "Reviewer",
  runtime_id: "",
  runtime_bound: false,
  execution_kind: "external",
  external_session: externalSession,
};

describe("AgentSchema WORK-9 fields", () => {
  it("omitted execution_kind keeps legacy managed behavior", () => {
    const parsed = AgentSchema.parse({ id: "agent-1", workspace_id: "ws-1", name: "Walt" });
    expect(agentExecutionKind(parsed)).toBe("managed");
    expect(parsed.external_session).toBeUndefined();
  });

  it("keeps an unknown execution_kind non-executable", () => {
    const parsed = AgentSchema.parse({ id: "agent-1", execution_kind: "future_kind" });
    expect(agentExecutionKind(parsed)).toBe("unknown");
  });

  it.each([
    ["null", null],
    ["empty", ""],
    ["a number", 42],
    ["an object", { kind: "managed" }],
  ])("never degrades a present %s execution_kind to managed", (_label, value) => {
    const parsed = AgentSchema.parse({ id: "agent-1", execution_kind: value });
    expect(parsed.execution_kind).toBe("unknown");
    expect(agentExecutionKind(parsed)).toBe("unknown");
  });

  it("carries external session identity and defaults missing session fields", () => {
    const parsed = AgentSchema.parse(externalAgent);
    expect(parsed.external_session?.host_id).toBe("server-1");
    expect(parsed.external_session?.session_id).toBe("agent-1");
    expect(parsed.external_session?.url).toBe("paseo://h/server-1/agent/agent-1");

    const partial = AgentSchema.parse({
      id: "agent-2",
      execution_kind: "external",
      external_session: { provider: "paseo", host_id: "server-2" },
    });
    expect(partial.external_session?.session_id).toBe("");
    expect(partial.external_session?.url).toBeNull();
  });

  it("drops one malformed agent without hiding the rest", () => {
    const parsed = AgentListSchema.parse([externalAgent, { name: "no id" }, { id: "agent-3" }]);
    expect(parsed.map((agent) => agent.id)).toEqual(["agent-1", "agent-3"]);
  });

  it("keeps the registered session id distinct from the host id and tolerates older responses", () => {
    const registered = AgentSchema.parse({
      ...externalAgent,
      external_session: { ...externalSession, id: "registered-session-1" },
    });
    expect(registered.external_session?.id).toBe("registered-session-1");
    expect(registered.external_session?.session_id).toBe("agent-1");
    expect(AgentSchema.parse(externalAgent).external_session?.id).toBeUndefined();

    const malformed = AgentSchema.parse({
      ...externalAgent,
      external_session: { ...externalSession, id: 42 },
    });
    expect(malformed.external_session?.id).toBeUndefined();
    expect(malformed.external_session?.session_id).toBe("agent-1");
  });

  it("carries the host template while an older response without it keeps legacy rendering", () => {
    const templated = AgentSchema.parse({
      ...externalAgent,
      external_template: {
        id: "agent-1",
        provider: "paseo",
        host_id: "server-1",
        name: "Paseo · VM-29-118",
      },
      external_session_count: 3,
    });
    expect(templated.external_template?.name).toBe("Paseo · VM-29-118");
    expect(templated.external_session_count).toBe(3);

    // The template switch off / an older backend: absent fields stay absent
    // instead of synthesizing a template or a zero count.
    const legacy = AgentSchema.parse(externalAgent);
    expect(legacy.external_template).toBeUndefined();
    expect(legacy.external_session_count).toBeUndefined();

    // A malformed additive value must not hide the agent.
    const malformed = AgentSchema.parse({
      ...externalAgent,
      external_template: "paseo",
      external_session_count: -1,
    });
    expect(malformed.id).toBe("agent-1");
    expect(malformed.external_template).toBeUndefined();
    expect(malformed.external_session_count).toBeUndefined();
  });

  it("keeps the server session total and tolerates older pages without one", () => {
    const page = AgentSessionListResponseSchema.parse({
      sessions: [{ id: "session-1", ...externalSession }],
      total: 7,
    });
    expect(page.total).toBe(7);

    const legacy = AgentSessionListResponseSchema.parse({
      sessions: [{ id: "session-1", ...externalSession }],
    });
    expect(legacy.total).toBeUndefined();
  });
});

describe("IssueSchema external collaboration projection", () => {
  it("preserves omitted collaboration fields for partial snapshots and older servers", () => {
    const parsed = IssueSchema.parse(baseIssue);
    expect(parsed.coordinator).toBeUndefined();
    expect(parsed.merge_authorization).toBeUndefined();
    expect(parsed).not.toHaveProperty("external_participants");
    expect(parsed).not.toHaveProperty("mailbox_deliveries");
  });

  it("parses coordinator, external participants and delivery/processing independently", () => {
    const parsed = ListIssuesResponseSchema.parse({
      issues: [
        {
          ...baseIssue,
          coordinator: { type: "agent", id: "agent-1" },
          merge_authorization: {
            allowed: true,
            repository_url: "https://github.com/junjzhang/multica",
            target_branch: "main",
            granted_by: "user-1",
            granted_at: "2026-01-02T00:00:00Z",
          },
          external_participants: [externalAgent],
          mailbox_deliveries: [
            {
              id: "msg-1",
              issue_id: baseIssue.id,
              comment_id: "comment-1",
              recipient_agent_id: "agent-1",
              source_session: externalSession,
              delivery_status: "delivered",
              processing_status: "unread",
              created_at: "2026-01-02T00:00:00Z",
              delivered_at: "2026-01-02T00:00:01Z",
              last_error: null,
            },
          ],
        },
      ],
      total: 1,
    });

    const issue = parsed.issues[0]!;
    expect(issue.coordinator).toEqual({ type: "agent", id: "agent-1" });
    expect(issue.merge_authorization?.allowed).toBe(true);
    expect(issue.external_participants?.[0]?.name).toBe("Reviewer");
    expect(issue.mailbox_deliveries?.[0]?.delivery_status).toBe("delivered");
    expect(issue.mailbox_deliveries?.[0]?.processing_status).toBe("unread");
  });

  it("keeps a delivered-but-unread message distinct, including unknown states", () => {
    const parsed = MailboxMessageListSchema.parse([
      { id: "msg-1", delivery_status: "delivered", processing_status: "unread", payload: { result_task_id: "result-1" }, handled_by_task_id: "consumer-1" },
      { id: "msg-2", delivery_status: "future_delivery", processing_status: "future_processing", payload: 42, handled_by_task_id: [] },
      { id: "msg-3", delivery_status: "failed", processing_status: "handled", last_error: "host offline" },
      { delivery_status: "delivered" },
    ]);
    expect(parsed[0]?.payload).toEqual({ result_task_id: "result-1" });
    expect(parsed[0]?.handled_by_task_id).toBe("consumer-1");
    expect(parsed[1]?.payload).toBeUndefined();
    expect(parsed[1]?.handled_by_task_id).toBeUndefined();
    expect(parsed.map((message) => [message.id, message.delivery_status, message.processing_status])).toEqual([
      ["msg-1", "delivered", "unread"],
      ["msg-2", "unknown", "unknown"],
      ["msg-3", "failed", "handled"],
    ]);
  });

  it("drops only a malformed additive field, keeping the issue", () => {
    const parsed = IssueSchema.parse({
      ...baseIssue,
      coordinator: { type: 42 },
      external_participants: "not an array",
      mailbox_deliveries: [{ id: 99 }],
    });
    expect(parsed.id).toBe(baseIssue.id);
    expect(parsed.coordinator).toBeUndefined();
    expect(parsed.external_participants).toBeUndefined();
    expect(parsed.mailbox_deliveries).toEqual([]);
  });
});

describe("dispatch_kind parsing", () => {
  it("defaults an omitted trigger preview dispatch to a native run", () => {
    const parsed = CommentTriggerPreviewSchema.parse({
      agents: [{ id: "agent-1", name: "Walt" }],
    });
    expect(parsed.agents[0]?.dispatch_kind).toBeUndefined();
    expect(mentionDispatchKind(parsed.agents[0]?.dispatch_kind)).toBe("run");
  });

  it("carries mailbox dispatch and treats unknown values as unknown", () => {
    const parsed = CommentTriggerPreviewSchema.parse({
      agents: [
        { id: "agent-1", name: "Reviewer", dispatch_kind: "mailbox" },
        { id: "agent-2", name: "Future", dispatch_kind: "queue" },
      ],
      blocked: [{ target_type: "agent", target_id: "agent-3", reason_code: "x", dispatch_kind: "mailbox" }],
    });
    expect(mentionDispatchKind(parsed.agents[0]?.dispatch_kind)).toBe("mailbox");
    expect(mentionDispatchKind(parsed.agents[1]?.dispatch_kind)).toBe("unknown");
    expect(parsed.blocked[0]?.dispatch_kind).toBe("mailbox");
  });

  it("never turns a present malformed dispatch_kind into a native run", () => {
    const parsed = CommentTriggerPreviewSchema.parse({
      agents: [
        { id: "agent-1", name: "Null", dispatch_kind: null },
        { id: "agent-2", name: "Empty", dispatch_kind: "" },
        { id: "agent-3", name: "Number", dispatch_kind: 42 },
        { id: "agent-4", name: "Omitted" },
      ],
    });
    expect(parsed.agents.map((agent) => mentionDispatchKind(agent.dispatch_kind))).toEqual([
      "unknown",
      "unknown",
      "unknown",
      "run",
    ]);
  });

  it("keeps dispatch_kind additive on comments and timeline entries", () => {
    const comment = CommentSchema.parse({
      id: "c-1", issue_id: "i-1", author_type: "agent", author_id: "a-1",
      content: "hi", type: "comment", parent_id: null, created_at: "x", updated_at: "x",
      dispatch_kind: "mailbox",
    });
    expect(comment.dispatch_kind).toBe("mailbox");
    const timeline = TimelineEntriesSchema.parse([
      { type: "comment", id: "c-1", actor_type: "agent", actor_id: "a-1", created_at: "x", dispatch_kind: "mailbox" },
    ]);
    expect(timeline[0]?.dispatch_kind).toBe("mailbox");
  });

  it("keeps malformed comment and timeline dispatches non-executable", () => {
    const comment = CommentSchema.parse({
      id: "c-1", issue_id: "i-1", author_type: "agent", author_id: "a-1",
      content: "hi", type: "comment", parent_id: null, created_at: "x", updated_at: "x",
      dispatch_kind: null,
    });
    expect(mentionDispatchKind(comment.dispatch_kind)).toBe("unknown");
    const timeline = TimelineEntriesSchema.parse([
      { type: "comment", id: "c-1", actor_type: "agent", actor_id: "a-1", created_at: "x", dispatch_kind: 42 },
      { type: "comment", id: "c-2", actor_type: "agent", actor_id: "a-1", created_at: "x" },
    ]);
    expect(timeline.map((entry) => mentionDispatchKind(entry.dispatch_kind))).toEqual([
      "unknown",
      "run",
    ]);
  });
});


it("preserves collaboration detail across partial events and accepts explicit clears", () => {
  const parseIssue = (data: unknown) => {
    const issue = parseWithFallback<Issue | undefined>(data, IssueSchema, undefined, {
      endpoint: "issue:updated",
    });
    if (!issue) throw new Error("Invalid issue fixture");
    return issue;
  };
  const qc = new QueryClient();
  const key = issueKeys.detail(baseIssue.workspace_id, baseIssue.id);
  const detail = parseIssue({
    ...baseIssue,
    coordinator: { type: "agent", id: externalAgent.id },
    merge_authorization: { allowed: true },
    external_participants: [externalAgent],
    mailbox_deliveries: [],
    external_session_bindings: [
      {
        id: "binding-1",
        agent_id: externalAgent.id,
        issue_id: baseIssue.id,
        session: { id: "session-1", ...externalSession },
        deliverable: true,
        unavailable_reason: "",
        created_at: "2026-01-02T00:00:00Z",
      },
    ],
  });
  qc.setQueryData(key, detail);
  onIssueUpdated(qc, baseIssue.workspace_id, parseIssue({ ...baseIssue, title: "Renamed" }));
  const after = qc.getQueryData<Issue>(key);
  expect(after?.title).toBe("Renamed");
  expect(after?.coordinator).toEqual(detail.coordinator);
  expect(after?.merge_authorization).toEqual(detail.merge_authorization);
  expect(after?.external_participants).toEqual(detail.external_participants);
  expect(after?.external_session_bindings).toEqual(detail.external_session_bindings);
  onIssueUpdated(qc, baseIssue.workspace_id, parseIssue({
    ...baseIssue, coordinator: null, merge_authorization: { allowed: false },
    external_participants: [], mailbox_deliveries: [], external_session_bindings: [],
  }));
  const cleared = qc.getQueryData<Issue>(key);
  expect(cleared?.coordinator).toBeNull();
  expect(cleared?.merge_authorization?.allowed).toBe(false);
  expect(cleared?.external_participants).toEqual([]);
  expect(cleared?.mailbox_deliveries).toEqual([]);
  expect(cleared?.external_session_bindings).toEqual([]);
});

describe("task session read projections", () => {
  it("keeps a generic delivery real: no fabricated comment, frozen target kept", () => {
    const [generic] = MailboxMessageListSchema.parse([
      {
        id: "msg-generic",
        issue_id: "issue-1",
        comment_id: null,
        recipient_agent_id: "agent-1",
        source_session: null,
        target_session: { id: "session-1", ...externalSession },
        source_type: "issue_wakeup",
        kind: "issue_wakeup",
        delivery_status: "unknown",
        processing_status: "unread",
        created_at: "2026-01-02T00:00:00Z",
      },
    ]);
    expect(generic?.comment_id).toBe("");
    expect(generic?.source_session).toBeNull();
    expect(generic?.target_session?.id).toBe("session-1");
    expect(generic?.source_type).toBe("issue_wakeup");
    expect(generic?.kind).toBe("issue_wakeup");
    expect(generic?.delivery_status).toBe("unknown");
  });

  it("defaults absent generic fields and degrades an unknown enum without dropping the row", () => {
    const [row] = MailboxMessageListSchema.parse([
      {
        id: "msg-future",
        comment_id: "",
        delivery_status: "future_status",
        processing_status: "future_processing",
        target_session: { id: 42, provider: "paseo" },
      },
    ]);
    expect(row?.kind).toBe("");
    expect(row?.source_type).toBe("");
    expect(row?.target_session).toBeNull();
    expect(row?.delivery_status).toBe("unknown");
    expect(row?.processing_status).toBe("unknown");
  });

  it("lists every registered session and drops only the malformed one", () => {
    const parsed = AgentSessionListResponseSchema.parse({
      sessions: [
        {
          id: "session-1",
          provider: "paseo",
          host_id: "host-1",
          session_id: "host-session-1",
          display_name: "Review",
          url: null,
          daemon_id: "daemon-1",
          deliverable: false,
          unavailable_reason: "agent_archived",
        },
        { id: 7 },
      ],
    });
    expect(parsed.sessions).toHaveLength(1);
    expect(parsed.sessions[0]?.id).toBe("session-1");
    expect(parsed.sessions[0]?.deliverable).toBe(false);
    expect(parsed.sessions[0]?.unavailable_reason).toBe("agent_archived");

    const defaults = AgentSessionListResponseSchema.parse({ sessions: [{ id: "session-2" }] });
    expect(defaults.sessions[0]?.deliverable).toBe(false);
    expect(defaults.sessions[0]?.unavailable_reason).toBe("");
    expect(defaults.sessions[0]?.provider).toBe("");
  });

  it("requires a real issue session binding but degrades malformed ones", () => {
    const parsed = IssueSessionBindingListSchema.parse([
      {
        id: "binding-1",
        agent_id: "agent-1",
        issue_id: "issue-1",
        session: { id: "session-1", ...externalSession },
        deliverable: true,
        unavailable_reason: "",
        created_at: "2026-01-02T00:00:00Z",
      },
      {
        id: "binding-2",
        agent_id: "agent-2",
        issue_id: "issue-1",
        session: { id: 42, provider: "paseo" },
        deliverable: true,
      },
      { agent_id: "missing-id" },
    ]);
    expect(parsed).toHaveLength(2);
    expect(parsed[1]?.id).toBe("binding-2");
    expect(parsed[1]?.session).toBeNull();
    expect(parsed[0]?.session?.id).toBe("session-1");
    expect(parsed[0]?.deliverable).toBe(true);

    expect(AgentSessionBindingResponseSchema.parse({ binding: parsed[0] })?.binding?.agent_id).toBe("agent-1");
    expect(AgentSessionBindingResponseSchema.parse({ binding: null }).binding).toBeNull();
    expect(AgentSessionBindingResponseSchema.safeParse("not an object").success).toBe(false);
  });

  it("carries bindings on the issue and treats a malformed field as absent", () => {
    const issue = IssueSchema.parse({
      ...baseIssue,
      external_session_bindings: [
        {
          id: "binding-1",
          agent_id: "agent-1",
          issue_id: baseIssue.id,
          session: { id: "session-1", ...externalSession },
          deliverable: true,
          unavailable_reason: "",
          created_at: "2026-01-02T00:00:00Z",
        },
      ],
    });
    expect(issue.external_session_bindings?.[0]?.session?.session_id).toBe("agent-1");

    const malformed = IssueSchema.parse({
      ...baseIssue,
      external_session_bindings: [{ id: 3 }],
    });
    expect(malformed.external_session_bindings).toBeUndefined();
    expect(IssueSessionBindingListSchema.safeParse([{ id: 3 }]).success).toBe(false);

    const empty = IssueSchema.parse({ ...baseIssue, external_session_bindings: [] });
    expect(empty.external_session_bindings).toEqual([]);
  });
});

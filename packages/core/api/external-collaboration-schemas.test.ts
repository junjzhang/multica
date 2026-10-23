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
      { id: "msg-1", delivery_status: "delivered", processing_status: "unread" },
      { id: "msg-2", delivery_status: "future_delivery", processing_status: "future_processing" },
      { id: "msg-3", delivery_status: "failed", processing_status: "handled", last_error: "host offline" },
      { delivery_status: "delivered" },
    ]);
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
    expect(parsed.external_participants).toEqual([]);
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
  });
  qc.setQueryData(key, detail);
  onIssueUpdated(qc, baseIssue.workspace_id, parseIssue({ ...baseIssue, title: "Renamed" }));
  const after = qc.getQueryData<Issue>(key);
  expect(after?.title).toBe("Renamed");
  expect(after?.coordinator).toEqual(detail.coordinator);
  expect(after?.merge_authorization).toEqual(detail.merge_authorization);
  expect(after?.external_participants).toEqual(detail.external_participants);
  onIssueUpdated(qc, baseIssue.workspace_id, parseIssue({
    ...baseIssue, coordinator: null, merge_authorization: { allowed: false },
    external_participants: [], mailbox_deliveries: [],
  }));
  const cleared = qc.getQueryData<Issue>(key);
  expect(cleared?.coordinator).toBeNull();
  expect(cleared?.merge_authorization?.allowed).toBe(false);
  expect(cleared?.external_participants).toEqual([]);
  expect(cleared?.mailbox_deliveries).toEqual([]);
});

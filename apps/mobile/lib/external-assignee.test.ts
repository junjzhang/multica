import { describe, expect, it } from "vitest";
import type { Agent, Issue, IssueSessionBinding } from "@multica/core/types";
import {
  boundExternalSession,
  externalAssigneeAgent,
  externalAssigneeChipLabel,
  isExternalExecutionAgent,
} from "./external-assignee";

const boundSession = {
  id: "session-bound",
  provider: "paseo",
  host_id: "host-1",
  session_id: "paseo-bound",
  display_name: "Issue route",
  url: null,
  daemon_id: "daemon-1",
};

const agent: Agent = {
  id: "agent-1",
  workspace_id: "ws-1",
  runtime_id: "",
  runtime_bound: false,
  name: "Reviewer",
  description: "",
  instructions: "",
  avatar_url: null,
  runtime_mode: "local",
  runtime_config: {},
  custom_args: [],
  visibility: "workspace",
  permission_mode: "public_to",
  invocation_targets: [],
  status: "idle",
  max_concurrent_tasks: 1,
  model: "",
  owner_id: "user-1",
  skills: [],
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
  archived_at: null,
  archived_by: null,
  execution_kind: "external",
  external_session: { ...boundSession, display_name: "Oldest agent route" },
  external_template: {
    id: "agent-1",
    provider: "paseo",
    host_id: "host-1",
    name: "Paseo · host-1",
  },
};

const managedAgent: Agent = {
  ...agent,
  id: "agent-managed",
  execution_kind: "managed",
  external_template: null,
  external_session: null,
};

const binding = (session: typeof boundSession): IssueSessionBinding => ({
  id: "binding-1",
  agent_id: agent.id,
  issue_id: "issue-1",
  session,
  deliverable: true,
  unavailable_reason: "",
  created_at: "2026-01-02T00:00:00Z",
});

function issue(overrides: Partial<Issue> = {}): Issue {
  return {
    id: "issue-1",
    workspace_id: "ws-1",
    number: 1,
    identifier: "WORK-1",
    title: "External assignment",
    description: null,
    status: "in_progress",
    priority: "medium",
    assignee_type: "agent",
    assignee_id: agent.id,
    creator_type: "member",
    creator_id: "user-1",
    parent_issue_id: null,
    project_id: null,
    position: 0,
    stage: null,
    start_date: null,
    due_date: null,
    metadata: {},
    properties: {},
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...overrides,
  };
}

describe("mobile external assignee", () => {
  it("treats only external execution as read-only", () => {
    expect(isExternalExecutionAgent(agent)).toBe(true);
    expect(isExternalExecutionAgent(managedAgent)).toBe(false);
    expect(externalAssigneeAgent(issue(), [agent])?.id).toBe(agent.id);
    expect(externalAssigneeAgent(issue(), [managedAgent])).toBeNull();
    expect(
      externalAssigneeAgent(issue({ assignee_type: "member" }), [agent]),
    ).toBeNull();
  });

  it("reads the session from the issue binding, not the agent projection", () => {
    const issueBinding = binding({
      ...boundSession,
      id: "session-issue",
      display_name: "Issue route",
    });
    expect(
      boundExternalSession(
        issue({ external_session_bindings: [issueBinding] }),
        agent.id,
      )?.display_name,
    ).toBe("Issue route");
  });

  it("reports an unknown session instead of guessing one", () => {
    expect(boundExternalSession(issue({ external_session_bindings: [] }), agent.id)).toBeNull();
    expect(boundExternalSession(issue(), agent.id)).toBeUndefined();
  });

  it("labels the assigned template with the bound session or unknown", () => {
    const session = boundExternalSession(
      issue({ external_session_bindings: [binding(boundSession)] }),
      agent.id,
    );
    expect(externalAssigneeChipLabel(agent, session, "Session unknown")).toBe(
      "Paseo · host-1 › Issue route",
    );
    expect(externalAssigneeChipLabel(agent, null, "Session unknown")).toBe(
      "Paseo · host-1 › Session unknown",
    );
  });
});

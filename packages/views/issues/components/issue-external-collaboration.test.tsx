// @vitest-environment jsdom

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, screen } from "@testing-library/react";
import type { Agent, Issue } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";

vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({
    getActorName: (type: string, id: string) => (type === "member" && id === "user-1" ? "Ada" : id),
    getMemberName: (id: string) => (id === "user-1" ? "Ada" : id),
  }),
}));

vi.mock("../../common/actor-avatar", () => ({
  ActorAvatar: () => <span data-testid="avatar" />,
}));

vi.mock("../../platform", () => ({
  isDesktopShell: () => false,
  openExternal: vi.fn(),
}));

import { IssueExternalCollaboration } from "./issue-external-collaboration";

const externalAgent: Agent = {
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
  external_session: {
    provider: "paseo",
    host_id: "server-1",
    session_id: "paseo-agent-1",
    display_name: "Review session",
    url: "paseo://h/server-1/agent/paseo-agent-1",
    daemon_id: "daemon-1",
  },
};

function issue(overrides: Partial<Issue> = {}): Issue {
  return {
    id: "issue-1",
    workspace_id: "ws-1",
    number: 14,
    identifier: "WORK-14",
    title: "External collaboration",
    description: null,
    status: "in_progress",
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
    properties: {},
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...overrides,
  };
}

afterEach(cleanup);

describe("IssueExternalCollaboration", () => {
  it("renders nothing without coordinator, participants or deliveries", () => {
    const { container } = renderWithI18n(
      <IssueExternalCollaboration issue={issue()} agents={[]} />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it("shows the fixed coordinator, external participants and their sessions", () => {
    renderWithI18n(
      <IssueExternalCollaboration
        issue={issue({
          coordinator: { type: "member", id: "user-1" },
          external_participants: [externalAgent],
        })}
        agents={[externalAgent]}
      />,
    );

    expect(screen.getByText("External collaboration")).toBeInTheDocument();
    expect(screen.getByText("Coordinator")).toBeInTheDocument();
    expect(screen.getByText("Ada")).toBeInTheDocument();
    expect(screen.getByText("External participants")).toBeInTheDocument();
    expect(screen.getByText("Reviewer")).toBeInTheDocument();
    expect(screen.getByText("Review session")).toBeInTheDocument();
    // An external participant never renders the managed missing-runtime error.
    expect(screen.queryByText("Needs a runtime")).not.toBeInTheDocument();
  });

  it("renders delivery and processing as separate states, never as completion", () => {
    renderWithI18n(
      <IssueExternalCollaboration
        issue={issue({
          external_participants: [externalAgent],
          mailbox_deliveries: [
            {
              id: "msg-1",
              issue_id: "issue-1",
              comment_id: "comment-1",
              recipient_agent_id: "agent-1",
              source_session: externalAgent.external_session ?? null,
              delivery_status: "delivered",
              processing_status: "unread",
              created_at: "2026-01-02T00:00:00Z",
              delivered_at: "2026-01-02T00:00:01Z",
              last_error: null,
            },
            {
              id: "msg-2",
              issue_id: "issue-1",
              comment_id: "comment-2",
              recipient_agent_id: "agent-1",
              source_session: null,
              delivery_status: "failed",
              processing_status: "handled",
              created_at: "2026-01-02T00:00:00Z",
              delivered_at: null,
              last_error: "host offline",
            },
          ],
        })}
        agents={[externalAgent]}
      />,
    );

    expect(screen.getByText("Mailbox deliveries")).toBeInTheDocument();
    expect(screen.getByText("Delivered to mailbox")).toBeInTheDocument();
    expect(screen.getByText("Unread")).toBeInTheDocument();
    expect(screen.getByText("Delivery failed")).toBeInTheDocument();
    expect(screen.getByText("Handled")).toBeInTheDocument();
    expect(screen.getByText("host offline")).toBeInTheDocument();
    // Delivered is not completion: it must not imply the recipient handled it.
    expect(screen.getAllByText("Handled")).toHaveLength(1);
  });

  it("keeps a managed coordinator's missing-runtime error", () => {
    const managed = {
      ...externalAgent,
      id: "agent-2",
      name: "Planner",
      execution_kind: undefined,
      external_session: undefined,
      runtime_id: "",
      runtime_bound: false,
    } satisfies Agent;
    renderWithI18n(
      <IssueExternalCollaboration
        issue={issue({ coordinator: { type: "agent", id: "agent-2" } })}
        agents={[managed]}
      />,
    );
    expect(screen.getByText("Planner")).toBeInTheDocument();
    expect(screen.getByText("Needs a runtime")).toBeInTheDocument();
    expect(screen.queryByText("External session")).not.toBeInTheDocument();
  });

  it("identifies an external agent coordinator by its session", () => {
    renderWithI18n(
      <IssueExternalCollaboration
        issue={issue({ coordinator: { type: "agent", id: "agent-1" } })}
        agents={[externalAgent]}
      />,
    );
    expect(screen.getByText("External session")).toBeInTheDocument();
    expect(screen.getByText("Review session")).toBeInTheDocument();
  });
});

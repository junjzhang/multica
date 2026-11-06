// @vitest-environment jsdom

import type { ReactElement } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { Agent, ExternalSession, Issue } from "@multica/core/types";
import { issueKeys } from "@multica/core/issues/queries";
import { renderWithI18n } from "../../test/i18n";

const permissions = vi.hoisted(() => ({ canManage: true }));
const apiMock = vi.hoisted(() => ({
  listAgentSessions: vi.fn(),
  bindAgentSession: vi.fn(),
}));

vi.mock("@multica/core/permissions", () => ({
  useAgentPermissions: () => ({
    canEdit: {
      allowed: permissions.canManage,
      reason: permissions.canManage ? "allowed" : "not_resource_owner",
      message: "",
    },
    canAssign: { allowed: true, reason: "allowed", message: "" },
    isLoading: false,
  }),
}));

vi.mock("@multica/core/hooks", () => ({
  useWorkspaceId: () => "ws-1",
}));

vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({
    getActorName: (type: string, id: string) => (type === "member" && id === "user-1" ? "Ada" : id),
    getMemberName: (id: string) => (id === "user-1" ? "Ada" : id),
  }),
}));

vi.mock("@multica/core/api", () => ({ api: apiMock }));

vi.mock("../../common/actor-avatar", () => ({
  ActorAvatar: () => <span data-testid="avatar" />,
}));

vi.mock("../../platform", () => ({
  isDesktopShell: () => false,
  openExternal: vi.fn(),
}));

import { IssueExternalCollaboration } from "./issue-external-collaboration";

const boundSession: ExternalSession = {
  id: "session-bound",
  provider: "paseo",
  host_id: "server-1",
  session_id: "paseo-bound",
  display_name: "Issue route",
  url: "paseo://h/server-1/agent/paseo-bound",
  daemon_id: "daemon-1",
};

const secondSession: ExternalSession = {
  id: "session-second",
  provider: "paseo",
  host_id: "server-2",
  session_id: "paseo-second",
  display_name: "Second route",
  url: "paseo://h/server-2/agent/paseo-second",
  daemon_id: "daemon-2",
};

const sourceSession: ExternalSession = {
  id: "session-source",
  provider: "paseo",
  host_id: "server-3",
  session_id: "paseo-source",
  display_name: "Sender route",
  url: "paseo://h/server-3/agent/paseo-source",
  daemon_id: "daemon-3",
};

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
    display_name: "Agent detail route",
    url: "paseo://h/server-1/agent/paseo-agent-1",
    daemon_id: "daemon-1",
  },
};

const secondAgent: Agent = {
  ...externalAgent,
  id: "agent-2",
  name: "Planner",
  external_session: { ...externalAgent.external_session!, display_name: "Planner detail route" },
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

function binding(agentId: string, session: ExternalSession) {
  return {
    id: `binding-${agentId}`,
    agent_id: agentId,
    issue_id: "issue-1",
    session,
    deliverable: true,
    unavailable_reason: "",
    created_at: "2026-01-02T00:00:00Z",
  };
}

function renderCollaboration(ui: ReactElement, client?: QueryClient) {
  const queryClient =
    client ??
    new QueryClient({
      defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
    });
  return renderWithI18n(
    <QueryClientProvider client={queryClient}>{ui}</QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  permissions.canManage = true;
  apiMock.listAgentSessions.mockReset();
  apiMock.bindAgentSession.mockReset();
});

beforeEach(() => {
  apiMock.listAgentSessions.mockResolvedValue([
    {
      ...boundSession,
      deliverable: true,
      unavailable_reason: "",
    },
    {
      ...secondSession,
      deliverable: true,
      unavailable_reason: "",
    },
  ]);
});

describe("IssueExternalCollaboration", () => {
  it("renders nothing without coordinator, participants, bindings or deliveries", () => {
    const { container } = renderCollaboration(
      <IssueExternalCollaboration issue={issue()} agents={[]} />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it("shows the issue binding instead of the agent's single detail session", () => {
    renderCollaboration(
      <IssueExternalCollaboration
        issue={issue({
          coordinator: { type: "member", id: "user-1" },
          external_participants: [externalAgent],
          external_session_bindings: [binding("agent-1", boundSession)],
        })}
        agents={[externalAgent]}
      />,
    );

    expect(screen.getByText("External collaboration")).toBeInTheDocument();
    expect(screen.getByText("Coordinator")).toBeInTheDocument();
    expect(screen.getByText("Ada")).toBeInTheDocument();
    expect(screen.getByText("External participants")).toBeInTheDocument();
    expect(screen.getByText("Reviewer")).toBeInTheDocument();
    expect(screen.getByText("Issue session")).toBeInTheDocument();
    expect(screen.getByText("Issue route")).toBeInTheDocument();
    // The agent-detail lookup picks one arbitrary session; it must not stand
    // in for the real issue binding.
    expect(screen.queryByText("Agent detail route")).not.toBeInTheDocument();
  });

  it("renders the correct address for two agents on the same issue", () => {
    renderCollaboration(
      <IssueExternalCollaboration
        issue={issue({
          external_participants: [externalAgent, secondAgent],
          external_session_bindings: [
            binding("agent-1", boundSession),
            binding("agent-2", secondSession),
          ],
        })}
        agents={[externalAgent, secondAgent]}
      />,
    );

    expect(screen.getByText("Issue route")).toBeInTheDocument();
    expect(screen.getByText("Second route")).toBeInTheDocument();
  });

  it("is read-only without agent management permission", () => {
    permissions.canManage = false;
    renderCollaboration(
      <IssueExternalCollaboration
        issue={issue({
          external_participants: [externalAgent],
          external_session_bindings: [binding("agent-1", boundSession)],
        })}
        agents={[externalAgent]}
      />,
    );

    expect(screen.getByText("Issue route")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /change session/i })).not.toBeInTheDocument();
  });

  it("keeps the displayed binding when the save is rejected", async () => {
    const user = userEvent.setup();
    apiMock.bindAgentSession.mockRejectedValue(new Error("forbidden"));
    renderCollaboration(
      <IssueExternalCollaboration
        issue={issue({
          external_participants: [externalAgent],
          external_session_bindings: [binding("agent-1", boundSession)],
        })}
        agents={[externalAgent]}
      />,
    );

    await user.click(screen.getByRole("button", { name: /change session/i }));
    expect(await screen.findByText("Second route")).toBeInTheDocument();
    await user.click(screen.getByRole("radio", { name: /second route/i }));
    await user.click(screen.getByRole("button", { name: /save session/i }));

    await waitFor(() => {
      expect(apiMock.bindAgentSession).toHaveBeenCalledWith("agent-1", {
        issue_id: "issue-1",
        session_id: "session-second",
      });
    });
    expect(
      await screen.findByText("Could not confirm the session save. The displayed binding has not changed."),
    ).toBeInTheDocument();
    // The dialog stays open and the old binding is still the displayed target.
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(screen.getAllByText("Issue route").length).toBeGreaterThan(0);
  });

  it("explains a generic unknown delivery without inventing a comment or a resend", () => {
    permissions.canManage = false;
    renderCollaboration(
      <IssueExternalCollaboration
        issue={issue({
          external_participants: [externalAgent],
          mailbox_deliveries: [
            {
              id: "msg-future",
              issue_id: "issue-1",
              comment_id: "",
              recipient_agent_id: "agent-1",
              source_session: sourceSession,
              target_session: boundSession,
              source_type: "issue_wakeup",
              kind: "future_result",
              delivery_status: "unknown",
              processing_status: "unread",
              created_at: "2026-01-02T00:00:00Z",
              delivered_at: null,
              last_error: "host outcome not confirmed",
            },
          ],
        })}
        agents={[externalAgent]}
      />,
    );

    expect(screen.getByText("Other notification")).toBeInTheDocument();
    expect(screen.getByText("Unknown")).toBeInTheDocument();
    expect(
      screen.getByText("Outcome unconfirmed. The host may have accepted it; this view does not resend it."),
    ).toBeInTheDocument();
    expect(screen.getByText("Sent to")).toBeInTheDocument();
    expect(screen.getByText("Sent from")).toBeInTheDocument();
    expect(screen.getByText("Issue route")).toBeInTheDocument();
    expect(screen.getByText("Sender route")).toBeInTheDocument();
    expect(screen.getByText("host outcome not confirmed")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /resend|retry/i })).not.toBeInTheDocument();
  });

  it("shows a legacy delivery with no frozen target instead of guessing one", () => {
    permissions.canManage = false;
    renderCollaboration(
      <IssueExternalCollaboration
        issue={issue({
          external_participants: [externalAgent],
          mailbox_deliveries: [
            {
              id: "msg-legacy",
              issue_id: "issue-1",
              comment_id: "",
              recipient_agent_id: "agent-1",
              source_session: null,
              target_session: null,
              source_type: "issue_wakeup",
              kind: "issue_wakeup",
              delivery_status: "failed",
              processing_status: "unread",
              created_at: "2026-01-02T00:00:00Z",
              delivered_at: null,
              last_error: "legacy delivery has no registered external session",
            },
          ],
        })}
        agents={[externalAgent]}
      />,
    );

    expect(screen.getByText("No frozen target (legacy)")).toBeInTheDocument();
    expect(screen.getByText("legacy delivery has no registered external session")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /resend|retry/i })).not.toBeInTheDocument();
  });

  it("renders delivery and processing as separate states, never as completion", () => {
    permissions.canManage = false;
    renderCollaboration(
      <IssueExternalCollaboration
        issue={issue({
          external_participants: [externalAgent],
          mailbox_deliveries: [
            {
              id: "msg-1",
              issue_id: "issue-1",
              comment_id: "comment-1",
              recipient_agent_id: "agent-1",
              source_session: sourceSession,
              target_session: boundSession,
              source_type: "comment",
              kind: "comment_mention",
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
              target_session: boundSession,
              source_type: "comment",
              kind: "comment_mention",
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
    expect(screen.getAllByText("Comment mention")).toHaveLength(2);
    expect(screen.getAllByText("Delivered to mailbox")).toHaveLength(1);
    expect(screen.getByText("Unread")).toBeInTheDocument();
    expect(screen.getByText("Delivery failed")).toBeInTheDocument();
    expect(screen.getByText("Handled")).toBeInTheDocument();
    expect(screen.getByText("host offline")).toBeInTheDocument();
    // Delivered is not completion: it must not imply the recipient handled it.
    expect(screen.getAllByText("Handled")).toHaveLength(1);
    expect(
      screen.getByText("Delivery means the host accepted the notification, not that the task finished or passed acceptance."),
    ).toBeInTheDocument();
  });

  it("keeps a managed coordinator's missing-runtime error", () => {
    const managed = {
      ...externalAgent,
      id: "agent-3",
      name: "Planner",
      execution_kind: undefined,
      external_session: undefined,
      runtime_id: "",
      runtime_bound: false,
    } satisfies Agent;
    renderCollaboration(
      <IssueExternalCollaboration
        issue={issue({ coordinator: { type: "agent", id: "agent-3" } })}
        agents={[managed]}
      />,
    );
    expect(screen.getByText("Planner")).toBeInTheDocument();
    expect(screen.getByText("Needs a runtime")).toBeInTheDocument();
    expect(screen.queryByText("External session")).not.toBeInTheDocument();
  });

  it("shows an external coordinator's issue binding, not its detail session", () => {
    renderCollaboration(
      <IssueExternalCollaboration
        issue={issue({
          coordinator: { type: "agent", id: "agent-1" },
          external_session_bindings: [binding("agent-1", boundSession)],
        })}
        agents={[externalAgent]}
      />,
    );
    expect(screen.getByText("Issue session")).toBeInTheDocument();
    expect(screen.getByText("Issue route")).toBeInTheDocument();
    expect(screen.queryByText("Agent detail route")).not.toBeInTheDocument();
  });

  it("does not present a failed binding projection as no binding", () => {
    renderCollaboration(
      <IssueExternalCollaboration
        issue={issue({ external_participants: [externalAgent] })}
        agents={[externalAgent]}
      />,
    );
    expect(
      screen.getByText("Could not load the current issue binding. Refresh the issue to try again."),
    ).toBeInTheDocument();
    expect(
      screen.queryByText("No session bound. New notifications for this agent on this issue are blocked until an owner or admin binds one."),
    ).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /change session/i })).not.toBeInTheDocument();
  });

  it("closes after a confirmed save and invalidates the issue projection", async () => {
    const user = userEvent.setup();
    apiMock.bindAgentSession.mockResolvedValue({
      id: "binding-new",
      agent_id: "agent-1",
      issue_id: "issue-1",
      session_id: "session-second",
      created_at: "2026-01-02T00:00:00Z",
    });
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
    });
    client.setQueryData(
      issueKeys.detail("ws-1", "issue-1"),
      issue({
        external_participants: [externalAgent],
        external_session_bindings: [binding("agent-1", boundSession)],
      }),
    );
    renderCollaboration(
      <IssueExternalCollaboration
        issue={issue({
          external_participants: [externalAgent],
          external_session_bindings: [binding("agent-1", boundSession)],
        })}
        agents={[externalAgent]}
      />,
      client,
    );

    await user.click(screen.getByRole("button", { name: /change session/i }));
    await user.click(await screen.findByRole("radio", { name: /second route/i }));
    await user.click(screen.getByRole("button", { name: /save session/i }));

    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await waitFor(() =>
      expect(client.getQueryState(issueKeys.detail("ws-1", "issue-1"))?.isInvalidated).toBe(true),
    );
  });
});

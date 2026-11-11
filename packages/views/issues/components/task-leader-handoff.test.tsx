// @vitest-environment jsdom

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "@multica/core/api";
import { issueKeys } from "@multica/core/issues/queries";
import type { Agent, Issue } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";

const state = vi.hoisted(() => ({ role: "owner" as string | null }));
const calls = vi.hoisted(() => ({
  handoffIssueLeadership: vi.fn(),
  listSquadMembers: vi.fn(),
  listAgents: vi.fn(),
  listAgentSessions: vi.fn(),
}));

vi.mock("@multica/core/api", async (original) => ({
  ...(await original<typeof import("@multica/core/api")>()),
  api: calls,
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/permissions", () => ({
  useCurrentMember: () => ({
    userId: "user-1",
    role: state.role,
    member: null,
    isLoading: false,
  }),
}));

import { TaskLeaderHandoff } from "./task-leader-handoff";

const leaderSession = {
  id: "session-current",
  provider: "paseo",
  host_id: "host-1",
  session_id: "paseo-current",
  display_name: "Current route",
  url: null,
  daemon_id: "daemon-1",
};

const leader: Agent = {
  id: "agent-external",
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
  external_session: leaderSession,
  external_template: { id: "agent-external", provider: "paseo", host_id: "host-1", name: "Paseo · host-1" },
};

const nextAgent: Agent = {
  ...leader,
  id: "agent-next",
  name: "Second Reviewer",
  external_session: { ...leaderSession, id: "session-next" },
};

const managedAgent: Agent = {
  ...leader,
  id: "agent-managed",
  name: "Builder",
  execution_kind: "managed",
  external_session: null,
  external_template: null,
};

function issue(overrides: Partial<Issue> = {}): Issue {
  return {
    id: "issue-1",
    workspace_id: "ws-1",
    number: 14,
    identifier: "WORK-14",
    title: "Handoff target",
    description: null,
    status: "in_progress",
    priority: "medium",
    assignee_type: "squad",
    assignee_id: "squad-1",
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
    revision: 9,
    leadership_root_issue_id: "issue-1",
    leadership_squad_id: "squad-1",
    leader_agent_id: "agent-external",
    leader_external_session_id: "session-current",
    leadership_revision: 3,
    leadership_root_revision: 7,
    ...overrides,
  };
}

function handoffQueryClient() {
  return new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
}

function renderHandoff(value: Issue = issue(), queryClient: QueryClient = handoffQueryClient()) {
  const view = renderWithI18n(
    <QueryClientProvider client={queryClient}>
      <TaskLeaderHandoff issue={value} />
    </QueryClientProvider>,
  );
  return { ...view, queryClient };
}

beforeEach(() => {
  state.role = "owner";
  calls.handoffIssueLeadership.mockReset();
  calls.listSquadMembers.mockReset();
  calls.listAgents.mockReset();
  calls.listAgentSessions.mockReset();
  calls.listAgents.mockResolvedValue([leader, nextAgent, managedAgent]);
  calls.listSquadMembers.mockResolvedValue([
    { id: "m1", squad_id: "squad-1", member_type: "agent", member_id: "agent-external", role: "leader", created_at: "now" },
    { id: "m2", squad_id: "squad-1", member_type: "agent", member_id: "agent-next", role: "member", created_at: "now" },
    { id: "m3", squad_id: "squad-1", member_type: "agent", member_id: "agent-managed", role: "member", created_at: "now" },
  ]);
  calls.listAgentSessions.mockResolvedValue([
    { ...leaderSession, id: "session-next", session_id: "paseo-next", display_name: "Next route", deliverable: true, unavailable_reason: "" },
  ]);
});

afterEach(cleanup);

describe("TaskLeaderHandoff", () => {
  it("is hidden from plain members", () => {
    state.role = "member";
    renderHandoff();
    expect(screen.queryByRole("button", { name: "Hand off" })).not.toBeInTheDocument();
  });

  it("hands the chain to another registered session with the current revisions", async () => {
    const user = userEvent.setup();
    calls.handoffIssueLeadership.mockResolvedValue({
      root_issue_id: "issue-1",
      revision: 4,
      issue_revision: 8,
      stale_contract_ids: ["contract-1"],
      advanced_returns: [{ issue_id: "issue-1", current_return_id: "return-2" }],
      historical_results: "explicit_result_takeover_required",
    });
    renderHandoff();
    await user.click(screen.getByRole("button", { name: "Hand off" }));
    const dialog = await screen.findByRole("dialog");
    await user.click(await within(dialog).findByRole("radio", { name: /Second Reviewer/ }));
    await user.click(await within(dialog).findByRole("radio", { name: /Next route/ }));
    await user.type(within(dialog).getByLabelText("Reason"), "Primary session is parked");
    await user.click(within(dialog).getByRole("button", { name: "Hand off" }));

    await waitFor(() => {
      expect(calls.handoffIssueLeadership).toHaveBeenCalledWith("issue-1", expect.objectContaining({
        expected_revision: 3,
        expected_issue_revision: 7,
        previous_agent_id: "agent-external",
        previous_session_id: "session-current",
        agent_id: "agent-next",
        session_id: "session-next",
        reason: "Primary session is parked",
      }));
    });
    expect(await within(dialog).findByText(/Contracts now stale: 1/)).toBeInTheDocument();
    expect(within(dialog).getByText(/explicit result takeover/)).toBeInTheDocument();
  });

  it("hands a subtask over through its leadership root", async () => {
    const user = userEvent.setup();
    calls.handoffIssueLeadership.mockResolvedValue({
      root_issue_id: "issue-1",
      revision: 4,
      issue_revision: 8,
      stale_contract_ids: [],
      advanced_returns: [],
      historical_results: "explicit_result_takeover_required",
    });
    const queryClient = handoffQueryClient();
    queryClient.setQueryData(issueKeys.detail("ws-1", "issue-1"), issue());
    const subtask = issue({
      id: "sub-1",
      number: 15,
      identifier: "WORK-15",
      parent_issue_id: "issue-1",
      revision: 5,
      leadership_root_issue_id: "issue-1",
    });
    renderHandoff(subtask, queryClient);

    await user.click(screen.getByRole("button", { name: "Hand off" }));
    const dialog = await screen.findByRole("dialog");
    await user.click(await within(dialog).findByRole("radio", { name: /Second Reviewer/ }));
    await user.click(await within(dialog).findByRole("radio", { name: /Next route/ }));
    await user.type(within(dialog).getByLabelText("Reason"), "Subtask owner is parked");
    await user.click(within(dialog).getByRole("button", { name: "Hand off" }));

    await waitFor(() => {
      expect(calls.handoffIssueLeadership).toHaveBeenCalledWith(
        "issue-1",
        expect.objectContaining({
          expected_issue_revision: 7,
          previous_agent_id: "agent-external",
          previous_session_id: "session-current",
          agent_id: "agent-next",
          session_id: "session-next",
        }),
      );
    });
    await waitFor(() => {
      expect(queryClient.getQueryState(issueKeys.detail("ws-1", "issue-1"))?.isInvalidated).toBe(true);
    });
  });

  it("reports a revision or leader conflict without claiming success", async () => {
    const user = userEvent.setup();
    calls.handoffIssueLeadership.mockRejectedValue(new ApiError("conflict", 409, "Conflict"));
    renderHandoff();
    await user.click(screen.getByRole("button", { name: "Hand off" }));
    const dialog = await screen.findByRole("dialog");
    await user.click(await within(dialog).findByRole("radio", { name: /Second Reviewer/ }));
    await user.click(await within(dialog).findByRole("radio", { name: /Next route/ }));
    await user.type(within(dialog).getByLabelText("Reason"), "Move the task");
    await user.click(within(dialog).getByRole("button", { name: "Hand off" }));

    expect(await within(dialog).findByRole("alert")).toHaveTextContent(/changed while the dialog was open/);
    expect(within(dialog).queryByText("Handoff recorded")).not.toBeInTheDocument();
  });
});

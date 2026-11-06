import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import type { Agent } from "@multica/core/types";
import enIssues from "../../../locales/en/issues.json";
import { AssigneePicker } from "./assignee-picker";

const MEMBERS = [{ user_id: "user-1", name: "Ada Lovelace", role: "member" }];

function agent(overrides: Partial<Agent>): Agent {
  return {
    id: "agent-base",
    workspace_id: "workspace-1",
    runtime_id: "runtime-1",
    name: "Agent",
    description: "",
    instructions: "",
    avatar_url: null,
    runtime_mode: "local",
    runtime_config: {},
    custom_args: [],
    visibility: "workspace",
    permission_mode: "public_to",
    invocation_targets: [{ target_type: "workspace", target_id: null }],
    status: "idle",
    max_concurrent_tasks: 1,
    model: "",
    owner_id: "user-1",
    skills: [],
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    archived_at: null,
    archived_by: null,
    ...overrides,
  };
}

const MANAGED_BOUND = agent({ id: "agent-bound", name: "Bound Agent" });
const MANAGED_UNBOUND = agent({
  id: "agent-unbound",
  name: "Unbound Agent",
  runtime_id: "",
  runtime_bound: false,
});
const EXTERNAL = agent({
  id: "agent-external",
  name: "External Reviewer",
  runtime_id: "",
  runtime_bound: false,
  execution_kind: "external",
  external_session: {
    provider: "paseo",
    host_id: "server-1",
    session_id: "paseo-agent-1",
    display_name: "Review session",
    url: "paseo://h/server-1/agent/paseo-agent-1",
    daemon_id: "daemon-1",
  },
});
const UNKNOWN = agent({
  id: "agent-unknown",
  name: "Future Agent",
  execution_kind: "future_kind",
});

vi.mock("@tanstack/react-query", () => ({
  useQuery: ({ queryKey }: { queryKey: string[] }) => {
    if (queryKey[0] === "members") return { data: MEMBERS };
    if (queryKey[0] === "agents")
      return { data: [MANAGED_BOUND, MANAGED_UNBOUND, EXTERNAL, UNKNOWN] };
    if (queryKey.includes("sessions")) return { data: [
      { id: "session-a", display_name: "Session A", session_id: "paseo-a", host_id: "host-a", provider: "paseo", deliverable: true },
      { id: "session-b", display_name: "Session B", session_id: "paseo-b", host_id: "host-b", provider: "paseo", deliverable: true },
    ] };
    return { data: [] };
  },
}));

vi.mock("@multica/core/agents/session-bindings", () => ({ agentSessionsOptions: (_ws: string, agentId: string) => ({ queryKey: ["sessions", agentId] }) }));

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "workspace-1" }));
vi.mock("@multica/core/auth", () => ({
  useAuthStore: (selector?: (state: { user: { id: string } }) => unknown) => {
    const state = { user: { id: "user-1" } };
    return selector ? selector(state) : state;
  },
}));
vi.mock("@multica/core/permissions", () => ({
  canAssignAgentToIssue: () => ({ allowed: true }),
}));
vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getActorName: () => "Ada Lovelace" }),
}));
vi.mock("@multica/core/workspace/queries", () => ({
  memberListOptions: () => ({ queryKey: ["members"] }),
  agentListOptions: () => ({ queryKey: ["agents"] }),
  squadListOptions: () => ({ queryKey: ["squads"] }),
  assigneeFrequencyOptions: () => ({ queryKey: ["frequency"] }),
}));
vi.mock("../../../common/actor-avatar", () => ({
  ActorAvatar: () => <span data-testid="actor-avatar" />,
}));

function renderPicker(onUpdate: (updates: unknown) => void) {
  return render(
    <I18nProvider locale="en" resources={{ en: { issues: enIssues } }}>
      <AssigneePicker
        assigneeType={null}
        assigneeId={null}
        onUpdate={onUpdate}
        open
        onOpenChange={() => {}}
      />
    </I18nProvider>,
  );
}

describe("AssigneePicker execution identity", () => {
  it("keeps external agents selectable and shows their session identity", async () => {
    const user = userEvent.setup();
    const onUpdate = vi.fn();
    renderPicker(onUpdate);

    expect(await screen.findByText("Review session")).toBeInTheDocument();
    const external = screen.getByRole("button", { name: /External Reviewer/ });
    expect(external).not.toBeDisabled();

    await user.click(external);
    expect(onUpdate).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Save session" })).toBeDisabled();
    expect(screen.getByRole("radio", { name: /Session A/ })).not.toBeChecked();
    expect(screen.getByRole("radio", { name: /Session B/ })).not.toBeChecked();
    await user.click(screen.getByRole("radio", { name: /Session B/ }));
    await user.click(screen.getByRole("button", { name: "Save session" }));
    expect(onUpdate).toHaveBeenCalledWith({
      assignee_type: "agent",
      assignee_id: "agent-external",
      recipient_session_id: "session-b",
    });
  });

  it("keeps managed missing-runtime and unknown execution rows disabled", async () => {
    const user = userEvent.setup();
    const onUpdate = vi.fn();
    renderPicker(onUpdate);

    expect(screen.getByRole("button", { name: /Bound Agent/ })).not.toBeDisabled();
    expect(screen.getByRole("button", { name: /Unbound Agent/ })).toBeDisabled();
    const unknown = screen.getByRole("button", { name: /Future Agent/ });
    expect(unknown).toBeDisabled();

    await user.click(unknown);
    expect(onUpdate).not.toHaveBeenCalled();
  });
});

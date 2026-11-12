// @vitest-environment jsdom

import type { ReactElement } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { Agent } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";

const mocks = vi.hoisted(() => ({
  total: 2,
  sessions: [] as Array<Record<string, unknown>>,
  limits: [] as number[],
}));

vi.mock("@tanstack/react-query", () => ({
  useQuery: (options: { queryKey?: readonly unknown[] }) => {
    const limit = Number(options.queryKey?.[options.queryKey.length - 1] ?? 0);
    mocks.limits.push(limit);
    return {
      data: { sessions: mocks.sessions.slice(0, limit), total: mocks.total },
      isPending: false,
      isError: false,
    };
  },
}));

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));

vi.mock("@multica/core/agents/session-bindings", () => ({
  agentSessionsPageOptions: (wsId: string, agentId: string, limit: number) => ({
    queryKey: ["sessions", wsId, agentId, limit],
  }),
}));

vi.mock("../../common/actor-avatar", () => ({
  ActorAvatar: () => <span data-testid="avatar" />,
}));

import { AgentSessionsPopover } from "./agent-sessions-popover";

function agent(overrides: Partial<Agent> = {}): Agent {
  return {
    id: "agent-1",
    workspace_id: "ws-1",
    runtime_id: "",
    runtime_bound: false,
    name: "Paseo · VM-29-118",
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
    external_template: { id: "agent-1", provider: "paseo", host_id: "server-1", name: "Paseo · VM-29-118" },
    external_session_count: 2,
    ...overrides,
  };
}

function session(overrides: Record<string, unknown> = {}) {
  return {
    id: "session-1",
    provider: "paseo",
    host_id: "server-1",
    session_id: "paseo-1",
    display_name: "Issue route",
    url: null,
    daemon_id: "daemon-1",
    deliverable: true,
    unavailable_reason: "",
    ...overrides,
  };
}

function render(ui: ReactElement) {
  return renderWithI18n(ui);
}

afterEach(() => {
  cleanup();
  mocks.sessions = [];
  mocks.total = 0;
  mocks.limits = [];
});

describe("AgentSessionsPopover", () => {
  it("lists each registered session with its availability", async () => {
    mocks.sessions = [session(), session({ id: "session-2", display_name: "Second route", deliverable: false, unavailable_reason: "session_revoked" })];
    mocks.total = 2;
    render(<AgentSessionsPopover agent={agent()} />);

    await userEvent.click(screen.getByRole("button", { name: "Sessions" }));
    expect(await screen.findByText("Issue route")).toBeInTheDocument();
    expect(screen.getByText("Second route")).toBeInTheDocument();
    expect(screen.getByText(/Deliverable/)).toBeInTheDocument();
    expect(screen.getByText(/Session revoked/)).toBeInTheDocument();
    expect(screen.getByText("Showing 2 of 2")).toBeInTheDocument();
  });

  it("pages with the server total instead of loading every session", async () => {
    mocks.sessions = Array.from({ length: 6 }, (_, index) =>
      session({ id: `session-${index}`, display_name: `Route ${index}` }),
    );
    mocks.total = 6;
    render(<AgentSessionsPopover agent={agent({ external_session_count: 6 })} />);

    await userEvent.click(screen.getByRole("button", { name: "Sessions" }));
    expect(await screen.findByText("Route 0")).toBeInTheDocument();
    expect(screen.queryByText("Route 5")).not.toBeInTheDocument();
    expect(screen.getByText("Showing 5 of 6")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Show more" }));
    expect(await screen.findByText("Route 5")).toBeInTheDocument();
    expect(mocks.limits).toContain(10);
  });
});

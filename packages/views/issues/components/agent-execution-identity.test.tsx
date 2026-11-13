// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, screen } from "@testing-library/react";
import type { Agent } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";

const openExternal = vi.hoisted(() => vi.fn());
const desktop = vi.hoisted(() => ({ value: true }));

vi.mock("../../platform", () => ({
  isDesktopShell: () => desktop.value,
  openExternal,
}));

vi.mock("../../common/actor-avatar", () => ({
  ActorAvatar: () => <span data-testid="avatar" />,
}));

import { AgentExecutionStatus, ExternalAgentCard } from "./agent-execution-identity";
import { ExternalAuthorSession } from "./external-author-session";

function agent(overrides: Partial<Agent>): Agent {
  return {
    id: "agent-1",
    workspace_id: "ws-1",
    runtime_id: "runtime-1",
    runtime_bound: true,
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
    ...overrides,
  };
}

const external = agent({
  execution_kind: "external",
  runtime_id: "",
  runtime_bound: false,
  external_session: {
    provider: "paseo",
    host_id: "server-1",
    session_id: "paseo-agent-1",
    display_name: "Review session",
    url: "paseo://h/server-1/agent/paseo-agent-1",
    daemon_id: "daemon-1",
  },
});

beforeEach(() => {
  desktop.value = true;
  openExternal.mockClear();
  Object.defineProperty(navigator, "clipboard", {
    configurable: true,
    value: { writeText: vi.fn().mockResolvedValue(undefined) },
  });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("AgentExecutionStatus", () => {
  it("keeps the managed missing-runtime error on managed agents", () => {
    renderWithI18n(
      <AgentExecutionStatus agent={agent({ runtime_id: "", runtime_bound: false })} />,
    );
    expect(screen.getByText("Needs a runtime")).toBeInTheDocument();
    expect(screen.queryByText("External session")).not.toBeInTheDocument();
  });

  it("shows an external agent's concrete session instead of a runtime error", () => {
    renderWithI18n(<AgentExecutionStatus agent={external} />);
    expect(screen.getByText("Review session")).toBeInTheDocument();
    expect(screen.getByText(/paseo · host server-1/)).toBeInTheDocument();
    expect(screen.getByText("session paseo-agent-1")).toBeInTheDocument();
    expect(screen.queryByText("Needs a runtime")).not.toBeInTheDocument();
  });

  it("renders an unknown execution kind as unsupported, not external", () => {
    renderWithI18n(<AgentExecutionStatus agent={agent({ execution_kind: "future_kind" })} />);
    expect(screen.getByText("Unsupported execution type")).toBeInTheDocument();
    expect(screen.queryByText("External session")).not.toBeInTheDocument();
    expect(screen.queryByText("Needs a runtime")).not.toBeInTheDocument();
  });

  it("opens a strictly valid session URL and copies the session id", () => {
    renderWithI18n(<AgentExecutionStatus agent={external} />);
    fireEvent.click(screen.getByRole("button", { name: "Open session" }));
    expect(openExternal).toHaveBeenCalledWith("paseo://h/server-1/agent/paseo-agent-1");

    fireEvent.click(screen.getByRole("button", { name: "Copy session ID" }));
    expect(navigator.clipboard.writeText).toHaveBeenCalledWith("paseo-agent-1");
  });

  it("does not offer opening for a malicious or malformed session URL", () => {
    for (const url of [
      "paseo://evil/server-1/agent/paseo-agent-1",
      "paseo://h/server-1/agent/paseo-agent-1?x=1",
      "javascript:alert(1)",
    ]) {
      const { unmount } = renderWithI18n(
        <AgentExecutionStatus
          agent={{
            ...external,
            external_session: { ...external.external_session!, url },
          }}
        />,
      );
      expect(screen.queryByRole("button", { name: "Open session" })).not.toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Copy session ID" })).toBeInTheDocument();
      unmount();
    }
  });

  it("does not offer opening when the surface has no desktop gate", () => {
    desktop.value = false;
    renderWithI18n(<AgentExecutionStatus agent={external} />);
    expect(screen.queryByRole("button", { name: "Open session" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Copy session ID" })).toBeInTheDocument();
  });
});

describe("ExternalAgentCard author resolution", () => {
  it("shows the real external agent name, session and owner", () => {
    renderWithI18n(<ExternalAgentCard agent={external} ownerName="Ada" />);
    expect(screen.getByText("Reviewer")).toBeInTheDocument();
    expect(screen.getByText("External session")).toBeInTheDocument();
    expect(screen.getByText("on behalf of Ada")).toBeInTheDocument();
    expect(screen.queryByText("Unknown Agent")).not.toBeInTheDocument();
  });
});

describe("ExternalAuthorSession author resolution", () => {
  it("resolves an external agent author to its session", () => {
    renderWithI18n(<ExternalAuthorSession agentId="agent-1" agents={[external]} />);
    expect(screen.getByText("Review session")).toBeInTheDocument();
  });

  it("renders nothing for a managed author or an unresolved id", () => {
    const managed = agent({ id: "agent-2", name: "Walt" });
    const { container } = renderWithI18n(
      <ExternalAuthorSession agentId="agent-2" agents={[managed]} />,
    );
    expect(container).toBeEmptyDOMElement();

    const { container: missing } = renderWithI18n(
      <ExternalAuthorSession agentId="unknown" agents={[external]} />,
    );
    expect(missing).toBeEmptyDOMElement();
  });
});

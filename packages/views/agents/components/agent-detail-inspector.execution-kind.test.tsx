import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, screen } from "@testing-library/react";
import type { Agent } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import { AgentDetailInspector } from "./agent-detail-inspector";

vi.mock("@tanstack/react-query", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@tanstack/react-query")>()),
  useQuery: () => ({ data: undefined, isSuccess: false }),
}));

vi.mock("../../common/avatar-upload-control", () => ({
  AvatarUploadControl: () => <div data-testid="avatar-upload" />,
}));
vi.mock("./inspector/model-picker", () => ({
  ModelPicker: () => <div data-testid="model-picker" />,
}));
vi.mock("./inspector/runtime-picker", () => ({
  RuntimePicker: () => <div data-testid="runtime-picker" />,
}));
vi.mock("./inspector/thinking-prop-row", () => ({
  ThinkingSettingField: () => <div data-testid="thinking-field" />,
}));
vi.mock("./inspector/service-tier-setting-field", () => ({
  ServiceTierSettingField: () => <div data-testid="service-tier-field" />,
}));

function agent(overrides: Partial<Agent>): Agent {
  return {
    id: "agent-1",
    workspace_id: "workspace-1",
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
    ...overrides,
  };
}

function renderInspector(value: Agent) {
  renderWithI18n(
    <AgentDetailInspector
      agent={value}
      runtime={null}
      runtimes={[]}
      members={[]}
      currentUserId="user-1"
      canEdit
      onUpdate={vi.fn(async () => {})}
    />,
  );
}

afterEach(cleanup);

describe("AgentDetailInspector execution identity", () => {
  it("shows the external session instead of managed runtime controls", () => {
    renderInspector(
      agent({
        execution_kind: "external",
        external_session: {
          provider: "paseo",
          host_id: "server-1",
          session_id: "paseo-agent-1",
          display_name: "Review session",
          url: "paseo://h/server-1/agent/paseo-agent-1",
          daemon_id: "daemon-1",
        },
      }),
    );

    expect(screen.getByText("Review session")).toBeInTheDocument();
    expect(screen.queryByTestId("runtime-picker")).toBeNull();
    expect(screen.queryByTestId("model-picker")).toBeNull();
    expect(screen.queryByTestId("thinking-field")).toBeNull();
    expect(screen.queryByTestId("service-tier-field")).toBeNull();
  });

  it("shows an unsupported execution boundary for unknown kinds", () => {
    renderInspector(agent({ execution_kind: "future_kind" }));

    expect(screen.getByText("Unsupported execution type")).toBeInTheDocument();
    expect(screen.queryByTestId("runtime-picker")).toBeNull();
  });
});

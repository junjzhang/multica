import { describe, expect, it } from "vitest";
import type { Agent } from "../types";
import {
  agentExecutionKind,
  agentNeedsRuntime,
  isExternalAgent,
  isMailboxDispatch,
  mentionDispatchKind,
} from "./execution-kind";

function agent(overrides: Partial<Agent>): Agent {
  return {
    id: "agent-1",
    workspace_id: "ws-1",
    runtime_id: "runtime-1",
    runtime_bound: true,
    name: "Walt",
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

describe("agentExecutionKind", () => {
  it("treats an omitted kind as the existing managed behavior", () => {
    expect(agentExecutionKind(agent({}))).toBe("managed");
    expect(agentExecutionKind(agent({ execution_kind: "managed" }))).toBe("managed");
  });

  it("recognizes external agents", () => {
    const external = agent({ execution_kind: "external", runtime_id: "" });
    expect(agentExecutionKind(external)).toBe("external");
    expect(isExternalAgent(external)).toBe(true);
  });

  it("keeps unknown kinds non-executable instead of guessing external", () => {
    expect(agentExecutionKind(agent({ execution_kind: "remote" }))).toBe("unknown");
    expect(isExternalAgent(agent({ execution_kind: "remote" }))).toBe(false);
  });

  it("does not treat a present null or empty kind as the legacy managed default", () => {
    expect(agentExecutionKind(agent({ execution_kind: null as unknown as string }))).toBe("unknown");
    expect(agentExecutionKind(agent({ execution_kind: "" }))).toBe("unknown");
  });
});

describe("agentNeedsRuntime", () => {
  it("keeps the real missing-runtime signal for managed agents", () => {
    expect(agentNeedsRuntime(agent({ runtime_id: "", runtime_bound: false }))).toBe(true);
    expect(agentNeedsRuntime(agent({ runtime_id: "runtime-1" }))).toBe(false);
  });

  it("never labels an external agent as needing a runtime", () => {
    expect(
      agentNeedsRuntime(agent({ execution_kind: "external", runtime_id: "", runtime_bound: false })),
    ).toBe(false);
  });
});

describe("mentionDispatchKind", () => {
  it("defaults only an omitted dispatch to a native run", () => {
    expect(mentionDispatchKind(undefined)).toBe("run");
    expect(mentionDispatchKind("run")).toBe("run");
  });

  it("does not treat a present null or empty dispatch as a native run", () => {
    expect(mentionDispatchKind(null)).toBe("unknown");
    expect(mentionDispatchKind("")).toBe("unknown");
  });

  it("recognizes mailbox delivery and rejects unknown values as neither", () => {
    expect(mentionDispatchKind("mailbox")).toBe("mailbox");
    expect(isMailboxDispatch("mailbox")).toBe(true);
    expect(mentionDispatchKind("external")).toBe("unknown");
    expect(isMailboxDispatch("external")).toBe(false);
  });
});

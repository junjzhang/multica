import { describe, expect, it } from "vitest";
import { sessionOpenEnv } from "../server/session-open";
import { contextPath, writeSessionContext } from "../server/store";
import { SESSION_A, makeContext, makeHome } from "./fixtures";

function request(overrides: Partial<Parameters<typeof sessionOpenEnv>[1]> = {}) {
  return {
    agentId: SESSION_A,
    workspaceId: "workspace",
    provider: "codex",
    cwd: "/tmp/project",
    reason: "resume" as const,
    purpose: "interactive" as const,
    title: null,
    labels: {},
    tools: [],
    mcpServers: {},
    env: { EXISTING: "1" },
    ...overrides,
  };
}

describe("session_open env", () => {
  it("injects only the context path for a connected interactive session", async () => {
    const home = await makeHome();
    await writeSessionContext(home, makeContext());
    const env = await sessionOpenEnv(home, request());
    expect(env).toEqual({
      EXISTING: "1",
      MULTICA_EXTERNAL_SESSION_FILE: contextPath(home, SESSION_A),
    });
    expect(JSON.stringify(env)).not.toContain(makeContext().token);
  });

  it("skips history replay, an explicit override, and an unconnected session", async () => {
    const home = await makeHome();
    await writeSessionContext(home, makeContext());
    expect(await sessionOpenEnv(home, request({ purpose: "history" }))).toBeUndefined();
    expect(
      await sessionOpenEnv(
        home,
        request({ env: { MULTICA_EXTERNAL_SESSION_FILE: "/explicit/context.json" } }),
      ),
    ).toBeUndefined();

    const emptyHome = await makeHome();
    expect(await sessionOpenEnv(emptyHome, request())).toBeUndefined();
  });
});

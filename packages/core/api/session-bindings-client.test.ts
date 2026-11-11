// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient } from "./client";

afterEach(() => vi.unstubAllGlobals());

function clientReturning(body: unknown): ApiClient {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify(body), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  })));
  return new ApiClient("https://api.example.test");
}

describe("session route reads", () => {
  it("distinguishes an explicitly absent binding from a failed read", async () => {
    const client = clientReturning({ binding: null });
    await expect(client.getAgentIssueSessionBinding("agent-1", "issue-1")).resolves.toBeNull();
  });

  it.each([null, {}, { binding: "invalid" }, { binding: { agent_id: "missing-id" } }])(
    "rejects malformed binding responses instead of reporting an unbound issue: %j",
    async (body) => {
      const client = clientReturning(body);
      await expect(client.getAgentIssueSessionBinding("agent-1", "issue-1")).rejects.toThrow(/binding/i);
    },
  );

  it("returns an existing binding even when its session is unavailable", async () => {
    const client = clientReturning({ binding: {
      id: "binding-1", agent_id: "agent-1", issue_id: "issue-1", session: null,
      deliverable: false, unavailable_reason: "agent_archived",
    } });
    await expect(client.getAgentIssueSessionBinding("agent-1", "issue-1")).resolves.toMatchObject({
      id: "binding-1", session: null, deliverable: false,
    });
  });

  it("accepts a genuinely empty session registry", async () => {
    await expect(clientReturning({ sessions: [] }).listAgentSessions("agent-1")).resolves.toEqual([]);
  });

  it.each([null, {}, { sessions: "invalid" }, { sessions: [{ id: 7 }] }])(
    "rejects unreadable registries instead of reporting no registered sessions: %j",
    async (body) => {
      await expect(clientReturning(body).listAgentSessions("agent-1")).rejects.toThrow(/session/i);
    },
  );

  it("keeps valid sessions when another registry entry is malformed", async () => {
    const client = clientReturning({ sessions: [{ id: "session-1" }, { id: 7 }] });
    await expect(client.listAgentSessions("agent-1")).resolves.toMatchObject([
      { id: "session-1", deliverable: false },
    ]);
  });
});

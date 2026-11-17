const tasks = (agentId: string) => ({
  state: "ready", agentId: "shared-external-agent", sessionId: agentId, workspaceId: "same-workspace",
  owned: ["session-a", "session-b"].includes(agentId) ? [{ issueId: agentId, identifier: agentId === "session-a" ? "WORK-1" : "WORK-2", title: `Only ${agentId}`, role: "executor", status: "in_progress", statusCategory: "started", url: `${location.origin}/acme/issues/${agentId === "session-a" ? "WORK-1" : "WORK-2"}` }] : [],
  accepting: [], returning: [], delegated: [], requests: [], truncated: false, checkedAt: "fixture",
});
const handlers: Record<string, (input: any) => Promise<any>> = {
  "session.tasks": async ({ agentId }) => agentId === "disconnected" ? { state: "disconnected", agentId, reason: "This Paseo session has no Multica binding.", checkedAt: "fixture" } : tasks(agentId),
  "session.task-expansion": async () => [],
  "session.status": async () => ({ state: "connected", checkedAt: "fixture" }),
  "session.pull-requests": async () => ({ pull_requests: [] }),
  "session.task-detail": async ({ section }) => ({ ok: true, data: ({ contract: { contract: null, accepted: [] }, runs: [], comments: [], prs: { pull_requests: [] }, children: { issues: [] } } as Record<string, unknown>)[section] }),
  "web.open": async ({ path }) => ({ url: location.origin + path, externalUrl: location.origin + path }),
};
export function useRpc(contract: { name: string }) { return handlers[contract.name]; }
export async function openExternalUrl() {}
export async function copyText() {}

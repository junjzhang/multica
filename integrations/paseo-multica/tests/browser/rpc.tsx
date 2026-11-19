import React from "react";
const board = (agentId: string) => ({
  state: "ready", scope: "session", viewer: { agentId: "shared-external-agent", sessionId: agentId, forgeUsernames: [] },
  issues: ["session-a", "session-b"].includes(agentId) ? [{ id: agentId, identifier: agentId === "session-a" ? "WORK-1" : "WORK-2", title: `Only ${agentId}`, status: "in_progress", statusCategory: "started", lastActivityAt: new Date().toISOString(), activeRun: true, lastRunFailed: false, pullRequests: [], url: `${location.origin}/acme/issues/${agentId === "session-a" ? "WORK-1" : "WORK-2"}` }] : [],
  rootIds: ["session-a", "session-b"].includes(agentId) ? [agentId] : [], projects: [], truncated: false, checkedAt: new Date().toISOString(),
  archivedDeliveries: agentId === "session-a" ? [{ id: "failed-delivery", issueIdentifier: "WORK-3", issueTitle: "Delivery to a closed conversation", sessionUrl: "paseo://h/same-host/agent/archived-session" }] : [],
});
const handlers: Record<string, (input: any) => Promise<any>> = {
  "session.board": async ({ agentId }) => agentId === "disconnected" ? { state: "disconnected", reason: "This Paseo session has no Multica binding.", checkedAt: "fixture" } : board(agentId),
  "session.task-expansion": async () => [],
  "session.status": async () => ({ state: "connected", checkedAt: "fixture" }),
  "web.open": async ({ path }) => ({ url: location.origin + path, externalUrl: location.origin + path }),
};
export function useRpc(contract: { name: string }) { return handlers[contract.name]; }
export async function openExternalUrl() {}
export async function copyText() {}
export function Icon({ name }: { name?: string }) { return <span aria-hidden="true" data-icon={name} />; }
export function Loading() { return <span>Loading...</span>; }

import React from "react";
const board = (agentId: string) => ({
  state: "ready", scope: "session", viewer: { agentId: "shared-external-agent", sessionId: agentId, forgeUsernames: [] },
  issues: ["session-a", "session-b"].includes(agentId) ? [{ id: agentId, identifier: agentId === "session-a" ? "WORK-1" : "WORK-2", title: `Only ${agentId}`, status: "in_progress", statusCategory: "started", lastActivityAt: new Date().toISOString(), activeRun: true, lastRunFailed: false, pullRequests: [], url: `${location.origin}/acme/issues/${agentId === "session-a" ? "WORK-1" : "WORK-2"}` }] : [],
  rootIds: ["session-a", "session-b"].includes(agentId) ? [agentId] : [], projects: [], truncated: false, checkedAt: new Date().toISOString(),
  archivedDeliveries: agentId === "session-a" ? [{ id: "failed-delivery", issueIdentifier: "WORK-3", issueTitle: "Delivery to a closed conversation", sessionUrl: "paseo://h/same-host/agent/archived-session" }] : [],
});
const recent = new Date(Date.now() - 2 * 60 * 60 * 1000).toISOString();
const issue = (identifier: string, title: string, extra: object) => ({ id: identifier, identifier, title, status: "in_progress", statusCategory: "started", lastActivityAt: recent, activeRun: false, lastRunFailed: false, pullRequests: [], url: `${location.origin}/acme/issues/${identifier}`, ...extra });
const mr = (repo: string, number: number, extra: object) => ({ id: `${repo}-${number}`, number, repo, title: `MR ${number}`, state: "open", url: `https://git.woa.com/o/${repo}/merge_requests/${number}`, ...extra });
const closed = { status: "done", statusCategory: "completed" };
const mrBoard = {
  state: "ready", scope: "session", viewer: { agentId: "shared-external-agent", sessionId: "mr", userId: "me", forgeUsernames: ["me"] },
  issues: [
    issue("WORK-830", "feat(serving): 统一推理服务的 MR 入口", { activeRun: true }),
    issue("WORK-831", "接入 CI 结果", { ...closed, parentId: "WORK-830", stage: 1, pullRequests: [mr("sglang", 833, { ci: "failed" })] }),
    issue("WORK-832", "迁移调度配置", { ...closed, parentId: "WORK-830", stage: 1, pullRequests: [mr("slime", 16, { ci: "passed", mergeable: "CONFLICTING" })] }),
    issue("WORK-834", "补齐压测脚本", { parentId: "WORK-830", stage: 2, activeRun: true, pullRequests: [mr("mimikyu", 20, { ci: "pending" })] }),
    issue("WORK-835", "灰度开关", { status: "in_review", parentId: "WORK-830", stage: 2, contract: { accepted: true }, pullRequests: [mr("gw", 41, { ci: "passed", myReview: "approving" })] }),
    issue("WORK-840", "fix(sched): 修复调度器抢占", { pullRequests: [mr("sglang", 901, { ci: "failed" })] }),
  ],
  rootIds: ["WORK-830", "WORK-840"], projects: [], truncated: false, archivedDeliveries: [], checkedAt: new Date().toISOString(),
};
const handlers: Record<string, (input: any) => Promise<any>> = {
  "session.board": async ({ agentId }) => agentId === "disconnected" ? { state: "disconnected", reason: "This Paseo session has no Multica binding.", checkedAt: "fixture" } : agentId === "mr" ? mrBoard : board(agentId),
  "session.task-expansion": async () => [],
  "session.status": async () => ({ state: "connected", checkedAt: "fixture" }),
  "web.open": async ({ path }) => ({ url: location.origin + path, externalUrl: location.origin + path }),
};
export function useRpc(contract: { name: string }) { return handlers[contract.name]; }
export async function openExternalUrl() {}
export async function copyText() {}
// Outline paths of the Lucide icons the board draws, enough for evidence screenshots.
const LUCIDE: Record<string, string> = {
  ChevronRight: '<path d="m9 18 6-6-6-6"/>',
  ArrowUpRight: '<path d="M7 7h10v10"/><path d="M7 17 17 7"/>',
  GitMerge: '<circle cx="18" cy="18" r="3"/><circle cx="6" cy="6" r="3"/><path d="M6 21V9a9 9 0 0 0 9 9"/>',
  GitPullRequest: '<circle cx="18" cy="18" r="3"/><circle cx="6" cy="6" r="3"/><path d="M13 6h3a2 2 0 0 1 2 2v7"/><path d="M6 9v12"/>',
  GitPullRequestDraft: '<circle cx="18" cy="18" r="3"/><circle cx="6" cy="6" r="3"/><path d="M18 6V5"/><path d="M18 11v-1"/><path d="M6 9v12"/>',
  RefreshCw: '<path d="M3 12a9 9 0 0 1 15-6.7L21 8"/><path d="M21 3v5h-5"/><path d="M21 12a9 9 0 0 1-15 6.7L3 16"/><path d="M8 16H3v5"/>',
};
export function Icon({ name, icon, size = 14, color }: { name?: string; icon?: { svg: string }; size?: number; color?: string }) {
  const svg = icon?.svg ?? (name && LUCIDE[name] ? `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">${LUCIDE[name]}</svg>` : "");
  return <span aria-hidden="true" data-icon={name} style={{ display: "inline-flex", width: size, height: size, flexShrink: 0, color }} dangerouslySetInnerHTML={{ __html: svg.replace("<svg ", `<svg width="${size}" height="${size}" `) }} />;
}
export function Loading() { return <span>Loading...</span>; }

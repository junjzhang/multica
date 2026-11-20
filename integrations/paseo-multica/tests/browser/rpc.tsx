import React from "react";
const board = (agentId: string) => ({
  state: "ready", scope: "session", viewer: { agentId: "shared-external-agent", sessionId: agentId, forgeUsernames: [] },
  issues: ["session-a", "session-b"].includes(agentId) ? [{ id: agentId, identifier: agentId === "session-a" ? "WORK-1" : "WORK-2", title: `Only ${agentId}`, status: "in_progress", statusCategory: "started", lastActivityAt: new Date().toISOString(), activeRun: true, lastRunFailed: false, pullRequests: [], url: `${location.origin}/acme/issues/${agentId === "session-a" ? "WORK-1" : "WORK-2"}` }] : [],
  rootIds: ["session-a", "session-b"].includes(agentId) ? [agentId] : [], projects: [], truncated: false, checkedAt: new Date().toISOString(),
  archivedDeliveries: agentId === "session-a" ? [{ id: "failed-delivery", issueIdentifier: "WORK-3", issueTitle: "Delivery to a closed conversation", session: { serverId: "same-host", agentId: "archived-session" } }] : [],
});
const recent = new Date(Date.now() - 2 * 60 * 60 * 1000).toISOString();
const issue = (identifier: string, title: string, extra: object) => ({ id: identifier, identifier, title, status: "in_progress", statusCategory: "started", lastActivityAt: recent, activeRun: false, lastRunFailed: false, pullRequests: [], url: `${location.origin}/acme/issues/${identifier}`, ...extra });
const mr = (repo: string, number: number, extra: object) => ({ id: `${repo}-${number}`, number, repo, title: `MR ${number}`, state: "open", url: `https://git.example.com/o/${repo}/merge_requests/${number}`, ...extra });
const closed = { status: "done", statusCategory: "completed" };
const mrBoard = {
  state: "ready", scope: "session", viewer: { agentId: "shared-external-agent", sessionId: "mr", userId: "me", forgeUsernames: ["me"] },
  issues: [
    issue("WORK-830", "feat(serving): 统一推理服务的 MR 入口", { activeRun: true, executionSession: { serverId: "same-host", agentId: "worker-session" } }),
    issue("WORK-831", "接入 CI 结果", { ...closed, parentId: "WORK-830", stage: 1, pullRequests: [mr("sglang", 833, { ci: "failed" })] }),
    issue("WORK-832", "迁移调度配置", { ...closed, parentId: "WORK-830", stage: 1, pullRequests: [mr("slime", 16, { ci: "passed", mergeable: "CONFLICTING" })] }),
    issue("WORK-834", "补齐压测脚本", { parentId: "WORK-830", stage: 2, activeRun: true, pullRequests: [mr("mimikyu", 20, { ci: "pending" })] }),
    issue("WORK-835", "灰度开关", { status: "in_review", parentId: "WORK-830", stage: 2, contract: { accepted: true }, pullRequests: [mr("gw", 41, { ci: "passed", myReview: "approving" })] }),
    issue("WORK-840", "fix(sched): 修复调度器抢占", { pullRequests: [mr("sglang", 901, { ci: "failed" })], executionSession: { serverId: "same-host", agentId: "worker-session" } }),
    issue("WORK-841", "复核调度器补丁", { status: "in_review", contract: { pendingActor: { type: "agent", id: "shared-external-agent", session_id: "mr" }, accepted: false }, executionSession: { serverId: "same-host", agentId: "mr" } }),
  ],
  rootIds: ["WORK-830", "WORK-840", "WORK-841"], projects: [], truncated: false, archivedDeliveries: [], checkedAt: new Date().toISOString(),
};
const daysAgo = (days: number) => new Date(Date.now() - days * 86400000).toISOString();
const actionsBoard = {
  state: "ready", scope: "session", viewer: { agentId: "shared-external-agent", sessionId: "actions", userId: "me", forgeUsernames: [] },
  issues: [
    issue("WORK-860", "feat(train): 长序列训练", { status: "in_progress" }),
    issue("WORK-861", "跑 32k 长序列基线", {
      parentId: "WORK-860", stage: 1, lastRunFailed: true, lastActivityAt: daysAgo(10),
      lastFailure: { summary: "CUDA out of memory on rank 3 (tried to allocate 2.00 GiB)", at: daysAgo(10), taskId: "0199a0b1-8610-7861-8861-000000000001" },
      parent: { identifier: "WORK-860", title: "feat(train): 长序列训练", url: `${location.origin}/acme/issues/WORK-860` },
      contract: { pendingActor: { type: "agent", id: "worker" }, pendingName: "开发 · Opus", accepted: false },
      pullRequests: [mr("mimikyu", 52, { state: "merged" })],
    }),
    issue("WORK-862", "等数据集导出", { parentId: "WORK-860", stage: 1, status: "blocked", blockedSince: daysAgo(4), parent: { identifier: "WORK-860", title: "feat(train): 长序列训练", url: `${location.origin}/acme/issues/WORK-860` } }),
  ],
  rootIds: ["WORK-860"], projects: [], truncated: false, archivedDeliveries: [], checkedAt: new Date().toISOString(),
};
/** The browser run settles a held action itself, so the in-progress state can be captured. */
const held: { settle?: (result: unknown) => void } = {};
(window as any).settleAction = (result: unknown) => held.settle?.(result);
const handlers: Record<string, (input: any) => Promise<any>> = {
  "session.task-action": async ({ action }) => {
    document.body.dataset.lastAction = JSON.stringify(action);
    if (action.kind === "rerun") return new Promise((resolve) => { held.settle = resolve; });
    if (action.kind === "status" && action.status === "in_progress") return { ok: false, status: 409, reason: "accept the current contract before completing this issue" };
    return { ok: true };
  },
  "session.board": async ({ agentId }) => agentId === "disconnected" ? { state: "disconnected", reason: "This Paseo session has no Multica binding.", checkedAt: "fixture" } : agentId === "mr" ? mrBoard : agentId === "actions" ? actionsBoard : board(agentId),
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
  MessageSquare: '<path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/>',
  RefreshCw: '<path d="M3 12a9 9 0 0 1 15-6.7L21 8"/><path d="M21 3v5h-5"/><path d="M21 12a9 9 0 0 1-15 6.7L3 16"/><path d="M8 16H3v5"/>',
};
export function Icon({ name, icon, size = 14, color }: { name?: string; icon?: { svg: string }; size?: number; color?: string }) {
  const svg = icon?.svg ?? (name && LUCIDE[name] ? `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">${LUCIDE[name]}</svg>` : "");
  return <span aria-hidden="true" data-icon={name} style={{ display: "inline-flex", width: size, height: size, flexShrink: 0, color }} dangerouslySetInnerHTML={{ __html: svg.replace("<svg ", `<svg width="${size}" height="${size}" `) }} />;
}
export function Loading() { return <span>Loading...</span>; }

import React from "react";
import { act, create, type ReactTestRenderer } from "react-test-renderer";
import { expect, it, vi } from "vitest";
import type { PluginAgentPanelProps } from "@getpaseo/plugin/client";
import type { BoardIssue, ReadyBoardData } from "../shared/board";

const { openURL } = vi.hoisted(() => ({ openURL: vi.fn() }));
vi.mock("react-native", () => ({ View: "View", Text: "Text", Pressable: "Pressable", ActivityIndicator: "ActivityIndicator", Linking: { openURL } }));
vi.mock("@getpaseo/plugin/client", () => ({ useRpc: (contract: { name: string }) => (contract.name === "session.board" ? getBoard : setExpansion) }));
vi.mock("@getpaseo/plugin/client/ui", () => ({ Icon: "Icon", Loading: "Loading" }));
const setExpansion = vi.fn(async () => [] as string[]);
const recent = new Date(Date.now() - 60 * 60 * 1000).toISOString();
const base = { statusCategory: "started", lastActivityAt: recent, activeRun: false, lastRunFailed: false, pullRequests: [] };
const session = { type: "agent", id: "leader", session_id: "s1" };
const issues: BoardIssue[] = [
  { ...base, id: "root", identifier: "WORK-494", title: "feat(ui): 统一 Leader 视图", status: "backlog", projectId: "p1", url: "https://app.example/acme/issues/WORK-494" },
  { ...base, id: "delivered", identifier: "WORK-496", title: "删除会话 MR", status: "in_review", parentId: "root", stage: 1, url: "https://app.example/acme/issues/WORK-496", contract: { pendingActor: session, acceptor: session, accepted: false } },
  { ...base, id: "approval", identifier: "WORK-470", title: "slime 修复", status: "in_review", projectId: "p1", contract: { accepted: true }, pullRequests: [{ id: "pr", number: 813, repo: "slime", title: "fix", state: "open", url: "https://git.woa.com/o/slime/merge_requests/813", ci: "passed", review: "approving", myReview: "approving", mergeable: "mergeable" }] },
  { ...base, id: "old", identifier: "WORK-100", title: "旧任务", status: "backlog" },
  { ...base, id: "mr", identifier: "WORK-497", title: "接入 MR 行", status: "in_progress", projectId: "p1", activeRun: true, pullRequests: [{ id: "pr2", number: 818, repo: "mimikyu", title: "refactor(lint): 清理分支", state: "open", url: "https://git.woa.com/o/mimikyu/merge_requests/818", ci: "pending" }, { id: "pr3", number: 700, repo: "mimikyu", title: "旧", state: "merged", url: "https://git.woa.com/o/mimikyu/merge_requests/700" }] },
];
const ready = (scope: "session" | "all"): ReadyBoardData => ({ state: "ready", scope, viewer: { agentId: "leader", sessionId: "s1", forgeUsernames: ["me"] }, issues, rootIds: ["root", "approval", "old", "mr"], projects: [{ id: "p1", name: "Paseo" }], truncated: false, checkedAt: "2026-11-17T07:08:00Z" });
const getBoard = vi.fn(async ({ scope }: { scope: "session" | "all" }) => ready(scope));
import { BoardSection, displayTitle, prSignal, todoPhrase, treePhrase } from "../client/board-panel";
import { buildBoard } from "../shared/board";

type Rendered = ReturnType<ReactTestRenderer["toJSON"]>;
const leaves = (node: Rendered | string): string => typeof node === "string" ? node : !node ? "" : Array.isArray(node) ? node.map(leaves).join("\n") : (node.children ?? []).map(leaves).join(node.type === "Text" ? "" : "\n");
const text = (renderer: ReactTestRenderer) => leaves(renderer.toJSON());
type Instance = ReactTestRenderer["root"];
const label = (node: Instance): string => node.children.map((child) => (typeof child === "string" ? child : label(child))).join("");
const press = async (renderer: ReactTestRenderer, name: string) => {
  const target = renderer.root.findAll((node) => (node.type as unknown) === "Pressable" && (node.props.accessibilityLabel === name || label(node).includes(name)))[0]!;
  await act(async () => { target.props.onPress(); });
};

it("lists what needs you once, then the trees, on single lines", async () => {
  const openTask = vi.fn();
  const colors = Object.fromEntries(["surface0", "border", "foreground", "foregroundMuted", "accent", "statusDanger", "statusWarning"].map((key) => [key, key]));
  let renderer: ReactTestRenderer;
  await act(async () => { renderer = create(<BoardSection agentId="agent" theme={{ colors } as PluginAgentPanelProps["theme"]} openTask={openTask} />); });
  const first = text(renderer!);
  expect(getBoard).toHaveBeenCalledWith({ agentId: "agent", scope: "session", refresh: false });
  expect(first.indexOf("需要你")).toBeLessThan(first.indexOf("进行中"));
  expect(first).toContain("slime 修复\n!813 待你审批");
  expect(first).toContain("删除会话 MR\n待验收");
  expect(first.split("slime 修复").length).toBe(2);
  expect(first).toContain("统一 Leader 视图\n0/1");
  expect(first).not.toContain("feat(ui)");
  expect(first).not.toContain("WORK-");
  expect(first).toContain("搁置\n1");
  expect(first).not.toContain("旧任务");
  const approval = renderer!.root.findAll((node) => (node.type as unknown) === "Text" && label(node) === "!813 待你审批")[0]!;
  expect(approval.props.style.color).toBe("statusWarning");

  await press(renderer!, "展开 WORK-494");
  expect(setExpansion).toHaveBeenCalledWith({ agentId: "agent", issueId: "root", expanded: true });
  expect(text(renderer!).split("删除会话 MR").length).toBe(3);

  await press(renderer!, "搁置");
  expect(text(renderer!)).toContain("旧任务\n未开始");

  await press(renderer!, "WORK-496 删除会话 MR 待验收");
  expect(openTask).toHaveBeenCalledWith("https://app.example/acme/issues/WORK-496", "WORK-496");
  await press(renderer!, "WORK-470 slime 修复 !813 待你审批");
  expect(openURL).toHaveBeenCalledWith("https://git.woa.com/o/slime/merge_requests/813");

  await press(renderer!, "展开 WORK-497");
  expect(text(renderer!)).toContain("mimikyu!818 清理分支\nCI 运行中");
  expect(text(renderer!)).not.toContain("mimikyu!700");
  await press(renderer!, "mimikyu!818 清理分支 CI 运行中");
  expect(openURL).toHaveBeenLastCalledWith("https://git.woa.com/o/mimikyu/merge_requests/818");

  await press(renderer!, "全部");
  expect(getBoard).toHaveBeenLastCalledWith({ agentId: "agent", scope: "all", refresh: false });
  const all = text(renderer!);
  expect(all).not.toContain("进行中");
  expect(all.indexOf("Paseo")).toBeLessThan(all.indexOf("统一 Leader 视图"));
  await press(renderer!, "Paseo");
  expect(text(renderer!)).not.toContain("统一 Leader 视图");

  await press(renderer!, "刷新");
  expect(getBoard).toHaveBeenLastCalledWith({ agentId: "agent", scope: "all", refresh: true });
  await act(async () => { renderer!.unmount(); });
});

it("strips only a conventional-commit prefix and names the current batch on the parent", () => {
  expect(displayTitle("review(release): 冻结 v2")).toBe("冻结 v2");
  expect(displayTitle("fix!: 修复")).toBe("修复");
  expect(displayTitle("Paseo: 按会话展示")).toBe("Paseo: 按会话展示");
  expect(displayTitle("统一 Leader 视图")).toBe("统一 Leader 视图");
  const child = (id: string, stage: number, done: boolean) => ({ ...base, id, identifier: id, title: id, status: done ? "done" : "todo", statusCategory: done ? "completed" : "unstarted", parentId: "p", stage });
  const parent = { ...base, id: "p", identifier: "P", title: "P", status: "in_progress" };
  const board = buildBoard({ issues: [parent, child("a", 1, true), child("b", 2, false), child("c", 3, false)], rootIds: ["p"], viewer: { agentId: "x", forgeUsernames: [] } }, Date.now());
  const tree = [...board.sections.active, ...board.sections.parked][0]!;
  expect(treePhrase(tree)).toEqual({ text: "第 2/3 批" });
  expect(treePhrase(tree.stages[1]!.nodes[0]!)).toEqual({ text: "未开始" });
});

it("explains a disconnected session instead of an empty board", async () => {
  getBoard.mockResolvedValueOnce({ state: "disconnected", reason: "Run multica session connect.", checkedAt: "x" } as never);
  let renderer: ReactTestRenderer;
  await act(async () => { renderer = create(<BoardSection agentId="agent" theme={{ colors: {} } as PluginAgentPanelProps["theme"]} openTask={vi.fn()} />); });
  expect(text(renderer!)).toContain("未连接 Multica");
  expect(text(renderer!)).toContain("Run multica session connect.");
  await act(async () => { renderer!.unmount(); });
});

it("reserves red for fresh failures", () => {
  const failed = (days: number) => ({ kind: "anomaly" as const, pullRequests: [], issue: { ...base, id: "f", identifier: "F", title: "F", status: "in_progress", lastRunFailed: true, lastActivityAt: new Date(Date.now() - days * 86400000).toISOString() } });
  expect(todoPhrase(failed(1), Date.now())).toEqual({ text: "运行失败 · 1d", tone: "danger" });
  expect(todoPhrase(failed(9), Date.now())).toEqual({ text: "运行失败 · 9d" });
});

it("folds a task's MRs into one signal", () => {
  const pr = (id: string, state: string, extra = {}) => ({ id, number: 1, repo: "r", title: "t", state, url: "https://git.woa.com/o/r/merge_requests/1", ...extra });
  const task = (pullRequests: ReturnType<typeof pr>[], extra = {}) => ({ ...base, id: "t", identifier: "T", title: "T", status: "in_progress", pullRequests, ...extra });
  expect(prSignal(task([]))).toBeUndefined();
  expect(prSignal(task([pr("a", "merged")]))).toMatchObject({ icon: "GitMerge", count: 0 });
  expect(prSignal(task([pr("a", "merged")], { status: "done", statusCategory: "completed" }))).toBeUndefined();
  expect(prSignal(task([pr("a", "open"), pr("b", "draft", { ci: "failed" })]))).toMatchObject({ icon: "GitPullRequest", count: 2, tone: "danger" });
  expect(prSignal(task([pr("a", "open", { myReview: "approving" })], { contract: { accepted: true } }))).toMatchObject({ tone: "action" });
  expect(prSignal(task([pr("a", "draft")]))).toMatchObject({ icon: "GitPullRequestDraft" });
});

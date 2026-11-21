import React from "react";
import { act, create, type ReactTestRenderer } from "react-test-renderer";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { PluginAgentPanelProps } from "@getpaseo/plugin/client";
import type { BoardIssue, ReadyBoardData } from "../shared/board";

const { openURL } = vi.hoisted(() => ({ openURL: vi.fn() }));
const { platform } = vi.hoisted(() => ({ platform: { OS: "web" } }));
vi.mock("react-native", () => ({ View: "View", Text: "Text", Pressable: "Pressable", ActivityIndicator: "ActivityIndicator", Modal: "Modal", Platform: platform, useWindowDimensions: () => ({ width: 400, height: 600 }), Linking: { openURL } }));
vi.mock("@getpaseo/plugin/client", () => ({ useRpc: (contract: { name: string }) => (contract.name === "session.board" ? getBoard : contract.name === "session.task-action" ? runAction : setExpansion) }));
vi.mock("@getpaseo/plugin/client/ui", () => ({ Icon: "Icon", Loading: "Loading" }));
const HOST = { id: "srv_here", label: "Here" };
const setExpansion = vi.fn(async () => [] as string[]);
const runAction = vi.fn(async (_input: unknown): Promise<{ ok: true } | { ok: false; status: number; reason: string }> => ({ ok: true }));
const recent = new Date(Date.now() - 60 * 60 * 1000).toISOString();
const base = { statusCategory: "started", lastActivityAt: recent, activeRun: false, lastRunFailed: false, pullRequests: [] };
const session = { type: "agent", id: "leader", session_id: "s1" };
const issues: BoardIssue[] = [
  { ...base, id: "root", identifier: "WORK-494", title: "feat(ui): 统一 Leader 视图", status: "backlog", projectId: "p1", url: "https://app.example/acme/issues/WORK-494" },
  { ...base, id: "delivered", identifier: "WORK-496", title: "删除会话 MR", status: "in_review", parentId: "root", stage: 1, url: "https://app.example/acme/issues/WORK-496", contract: { pendingActor: session, acceptor: session, accepted: false } },
  { ...base, id: "approval", identifier: "WORK-470", title: "slime 修复", status: "in_review", projectId: "p1", contract: { accepted: true }, pullRequests: [{ id: "pr", number: 813, repo: "slime", title: "fix", state: "open", url: "https://git.example.com/o/slime/merge_requests/813", ci: "passed", review: "approving", myReview: "approving", mergeable: "mergeable" }] },
  { ...base, id: "old", identifier: "WORK-100", title: "旧任务", status: "backlog" },
  { ...base, id: "mr", identifier: "WORK-497", title: "接入 MR 行", status: "in_progress", projectId: "p1", activeRun: true, pullRequests: [{ id: "pr2", number: 818, repo: "mimikyu", title: "refactor(lint): 清理分支", state: "open", url: "https://git.example.com/o/mimikyu/merge_requests/818", ci: "pending" }, { id: "pr3", number: 700, repo: "mimikyu", title: "旧", state: "merged", url: "https://git.example.com/o/mimikyu/merge_requests/700" }] },
];
const ready = (scope: "session" | "all"): ReadyBoardData => ({ state: "ready", scope, viewer: { agentId: "leader", sessionId: "s1", forgeUsernames: ["me"] }, issues, rootIds: ["root", "approval", "old", "mr"], projects: [{ id: "p1", name: "Paseo" }], truncated: false, checkedAt: "2026-11-17T07:08:00Z" });
const getBoard = vi.fn(async ({ scope }: { scope: "session" | "all" }) => ready(scope));
import { BoardSection, displayTitle, menuHeight, menuPlacement, mergedMark, reasonsOf, todoChip, todoPhrase, treeChip, treePhrase } from "../client/board-panel";
import { taskMenu } from "../shared/task-actions";
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
  await act(async () => { renderer = create(<BoardSection host={HOST} agentId="agent" theme={{ colors } as PluginAgentPanelProps["theme"]} openTask={openTask} />); });
  const first = text(renderer!);
  expect(getBoard).toHaveBeenCalledWith({ agentId: "agent", scope: "session", refresh: false });
  expect(first.indexOf("需要你")).toBeLessThan(first.indexOf("进行中"));
  expect(first).toMatch(/slime 修复\n+!813 待你审批/);
  expect(first.indexOf("需要你")).toBeLessThan(first.indexOf("待本会话处理"));
  expect(first.slice(first.indexOf("待本会话处理"))).toContain("删除会话 MR\n待验收");
  expect(first.split("slime 修复").length).toBe(2);
  expect(first).toContain("统一 Leader 视图");
  expect(first).not.toContain("0/1");
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
  expect(text(renderer!)).toContain("旧任务");

  await press(renderer!, "WORK-496 评审中 删除会话 MR 待验收");
  expect(openTask).toHaveBeenCalledWith("https://app.example/acme/issues/WORK-496", "WORK-496");
  await press(renderer!, "打开 slime!813 !813 待你审批");
  expect(openURL).toHaveBeenCalledWith("https://git.example.com/o/slime/merge_requests/813");

  await press(renderer!, "打开 mimikyu!818 CI 运行中");
  expect(openURL).toHaveBeenLastCalledWith("https://git.example.com/o/mimikyu/merge_requests/818");
  await press(renderer!, "展开 WORK-497");
  expect(text(renderer!)).toContain("mimikyu!700 旧\n已合并");
  expect(text(renderer!)).not.toContain("mimikyu!818 清理分支");

  await press(renderer!, "全部");
  expect(getBoard).toHaveBeenLastCalledWith({ agentId: "agent", scope: "all", refresh: false });
  const all = text(renderer!);
  expect(all.split("\n")).not.toContain("进行中");
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
  expect(treePhrase(tree.stages[1]!.nodes[0]!)).toBeUndefined();
});

it("explains a disconnected session instead of an empty board", async () => {
  getBoard.mockResolvedValueOnce({ state: "disconnected", reason: "Run multica session connect.", checkedAt: "x" } as never);
  let renderer: ReactTestRenderer;
  await act(async () => { renderer = create(<BoardSection host={HOST} agentId="agent" theme={{ colors: {} } as PluginAgentPanelProps["theme"]} openTask={vi.fn()} />); });
  expect(text(renderer!)).toContain("未连接 Multica");
  expect(text(renderer!)).toContain("Run multica session connect.");
  await act(async () => { renderer!.unmount(); });
});

it("reserves red for fresh failures", () => {
  const failed = (days: number) => ({ kind: "anomaly" as const, pullRequests: [], issue: { ...base, id: "f", identifier: "F", title: "F", status: "in_progress", lastRunFailed: true, lastActivityAt: new Date(Date.now() - days * 86400000).toISOString() } });
  expect(todoPhrase(failed(1), Date.now())).toEqual({ text: "运行失败 · 1d", tone: "danger" });
  expect(todoPhrase(failed(9), Date.now())).toEqual({ text: "运行失败 · 9d" });
});

const mr = (id: string, state: string, extra = {}) => ({ id, number: Number(id.replace(/\D/g, "")) || 1, repo: "r", title: `MR ${id}`, state, url: `https://git.example.com/o/r/merge_requests/${id}`, ...extra });
const task = (id: string, pullRequests: ReturnType<typeof mr>[], extra = {}) => ({ ...base, id, identifier: id.toUpperCase(), title: `任务 ${id}`, status: "in_progress", pullRequests, ...extra });
const treeOf = (issues: BoardIssue[], root: string) => {
  const board = buildBoard({ issues, rootIds: [root], viewer: { agentId: "x", forgeUsernames: [] } }, Date.now());
  return Object.values(board.sections).flat()[0]!;
};

it("folds a task's open MRs into one chip and keeps merged ones as a quiet mark", () => {
  const chip = (pullRequests: ReturnType<typeof mr>[], extra = {}) => treeChip(treeOf([task("t", pullRequests, extra)], "t"));
  expect(chip([])).toBeUndefined();
  expect(chip([mr("b", "closed")])).toBeUndefined();
  expect(chip([mr("a1", "merged")])).toBeUndefined();
  expect(mergedMark(task("t", [mr("a1", "merged"), mr("b", "closed")]))).toMatchObject({ text: "!1", label: "已合并 r!1" });
  expect(chip([mr("a1", "merged"), mr("b2", "open")])).toMatchObject({ icon: "GitPullRequest", text: "!2", label: "r!2" });
  expect(chip([mr("b2", "open", { ci: "pending" })])).toMatchObject({ text: "!2 CI 运行中", label: "r!2 CI 运行中" });
  expect(chip([mr("a1", "open"), mr("b2", "draft", { ci: "failed" })])).toMatchObject({ icon: "GitPullRequest", text: "2", tone: "danger" });
  expect(chip([mr("a1", "open", { mergeable: "CONFLICTING" })])).toMatchObject({ tone: "danger" });
  expect(chip([mr("a1", "open", { myReview: "approving" })], { contract: { accepted: true } })).toMatchObject({ tone: "action" });
  expect(chip([mr("a1", "draft")])).toMatchObject({ icon: "GitPullRequestDraft" });
});

it("sums a parent's whole subtree of open MRs, most urgent first", () => {
  const issues = [
    task("p", [mr("p1", "open", { ci: "passed" })]),
    task("c", [mr("c2", "open", { myReview: "approving" })], { parentId: "p", stage: 1, contract: { accepted: true } }),
    task("g", [mr("g3", "open", { ci: "failed" }), mr("g4", "merged")], { parentId: "c" }),
    task("d", [mr("d5", "open")], { parentId: "p", stage: 2, status: "done", statusCategory: "completed" }),
  ];
  const chip = treeChip(treeOf(issues, "p"))!;
  expect(chip).toMatchObject({ text: "4", tone: "danger" });
  expect(chip.entries.map((entry) => `${entry.issue.id}:${entry.pr.id}`)).toEqual(["g:g3", "c:c2", "p:p1", "d:d5"]);
  const shared = treeChip(treeOf([task("p", [mr("p1", "open", { ci: "failed" })]), task("c", [mr("p1", "open", { ci: "failed" })], { parentId: "p" })], "p"))!;
  expect(shared).toMatchObject({ text: "!1 CI 失败", tone: "danger" });
  expect(shared.entries.map((entry) => `${entry.issue.id}:${entry.pr.id}`)).toEqual(["c:p1"]);
  const calm = treeChip(treeOf(issues.slice(0, 2), "p"))!;
  expect(calm).toMatchObject({ text: "2", tone: "action" });
});

it("points every MR to-do at its MR", () => {
  const now = Date.now();
  const done = { status: "done", statusCategory: "completed" };
  const urls = (chip: ReturnType<typeof todoChip>) => chip?.entries.map((entry) => entry.pr.url);
  const merge = (prs: ReturnType<typeof mr>[]) => todoChip({ kind: "merge", issue: task("m", prs, done), pullRequests: prs }, now);
  expect(merge([mr("7", "open")])).toMatchObject({ text: "!7 未合并" });
  expect(merge([mr("7", "open", { mergeable: "CONFLICTING" })])).toMatchObject({ text: "!7 有冲突", tone: "danger" });
  expect(merge([mr("7", "open", { ci: "failed" })])).toMatchObject({ text: "!7 CI 失败", tone: "danger" });
  expect(urls(merge([mr("7", "open"), mr("8", "open", { ci: "failed" })]))).toEqual(["https://git.example.com/o/r/merge_requests/8", "https://git.example.com/o/r/merge_requests/7"]);
  expect(merge([mr("7", "open"), mr("8", "open", { ci: "failed" })])).toMatchObject({ text: "!8 CI 失败 +1" });
  const approve = [mr("5", "open", { myReview: "approving" })];
  expect(todoChip({ kind: "approval", issue: task("a", approve, { contract: { accepted: true } }), pullRequests: approve }, now)).toMatchObject({ text: "!5 待你审批", tone: "action" });
  const ci = [mr("9", "open", { ci: "failed" }), mr("6", "open")];
  const anomaly = todoChip({ kind: "anomaly", issue: task("x", ci), pullRequests: ci }, now)!;
  expect(anomaly).toMatchObject({ text: "!9 CI 失败 · 1h", tone: "danger" });
  expect(urls(anomaly)).toEqual(["https://git.example.com/o/r/merge_requests/9"]);
  expect(todoChip({ kind: "anomaly", issue: task("y", [], { lastRunFailed: true }), pullRequests: [] }, now)).toBeUndefined();
  expect(todoChip({ kind: "user", issue: task("u", approve), pullRequests: approve }, now)).toBeUndefined();
});

it("opens MRs from their chip and the task from the row", async () => {
  const openTask = vi.fn();
  const done = { status: "done", statusCategory: "completed" };
  const parent = task("p", [], { url: "https://app.example/acme/issues/P" });
  const issues = [
    parent,
    task("c", [mr("21", "open", { ci: "failed" })], { parentId: "p", url: "https://app.example/acme/issues/C" }),
    task("d", [mr("16", "open", { mergeable: "CONFLICTING" })], { ...done, parentId: "p", url: "https://app.example/acme/issues/D" }),
    task("e", [mr("33", "open")], { parentId: "p", stage: 2, title: "fix(x): 很长很长的子任务标题" }),
  ];
  getBoard.mockResolvedValueOnce({ ...ready("session"), issues, rootIds: ["p"] });
  const colors = Object.fromEntries(["foreground", "foregroundMuted", "statusDanger", "statusWarning", "border"].map((key) => [key, key]));
  let renderer: ReactTestRenderer;
  await act(async () => { renderer = create(<BoardSection host={HOST} agentId="chips" theme={{ colors } as PluginAgentPanelProps["theme"]} openTask={openTask} />); });
  openURL.mockClear();

  await press(renderer!, "打开 r!16 !16 有冲突");
  expect(openURL).toHaveBeenLastCalledWith("https://git.example.com/o/r/merge_requests/16");
  await press(renderer!, "D 已完成 任务 d");
  expect(openTask).toHaveBeenLastCalledWith("https://app.example/acme/issues/D", "D");
  expect(openURL).toHaveBeenCalledTimes(1);

  const summary = renderer!.root.findAll((node) => (node.type as unknown) === "Pressable" && node.props.accessibilityLabel === "3 个 MR r!21 r!16 r!33")[0]!;
  expect(summary.props.style.borderColor).toBe("statusDanger");
  expect(summary.props.accessibilityState).toEqual({ expanded: false });
  await press(renderer!, "3 个 MR r!21 r!16 r!33");
  const list = text(renderer!);
  expect(list).toMatch(/r!21 · C 任务 c\nCI 失败[\s\S]*r!16 · D 任务 d\n有冲突[\s\S]*r!33 · E 很长很长的子任务标题/);
  await press(renderer!, "r!33 · E 很长很长的子任务标题");
  expect(openURL).toHaveBeenLastCalledWith("https://git.example.com/o/r/merge_requests/33");
  await press(renderer!, "P 进行中 任务 p 1/3");
  expect(openTask).toHaveBeenLastCalledWith("https://app.example/acme/issues/P", "P");
  await act(async () => { renderer!.unmount(); });
});

it("draws a staged parent as a pipeline with only the current batch open", async () => {
  const kid = (id: string, stage: number, done: boolean) => ({ ...base, id, identifier: id, title: `子任务 ${id}`, status: done ? "done" : "todo", statusCategory: done ? "completed" : "unstarted", parentId: "p", stage,
    pullRequests: done ? [{ id: `mr-${id}`, number: 813, repo: "slime", title: "t", state: "merged", url: "https://git.example.com/o/slime/merge_requests/813" }] : [] });
  const parent = { ...base, id: "p", identifier: "P", title: "流水线", status: "in_progress", activeRun: true };
  getBoard.mockResolvedValueOnce({ ...ready("session"), issues: [parent, kid("a", 1, true), kid("b", 2, false), kid("c", 2, false), kid("d", 3, false)], rootIds: ["p"] });
  let renderer: ReactTestRenderer;
  await act(async () => { renderer = create(<BoardSection host={HOST} agentId="graph" theme={{ colors: {} } as PluginAgentPanelProps["theme"]} openTask={vi.fn()} />); });
  expect(text(renderer!)).toMatch(/流水线\s+第 2\/3 批/);
  await press(renderer!, "展开 P");
  const graph = text(renderer!);
  expect(graph).toMatch(/第 1 批[\s\S]*slime!813[\s\S]*第 2 批[\s\S]*0\/2[\s\S]*子任务 b[\s\S]*子任务 c[\s\S]*第 3 批/);
  expect(graph).not.toContain("1/1");
  expect(graph).not.toContain("0/1");
  openURL.mockClear();
  await press(renderer!, "slime!813 已合并");
  expect(openURL).toHaveBeenCalledWith("https://git.example.com/o/slime/merge_requests/813");
  expect(graph).not.toContain("子任务 a");
  expect(graph).not.toContain("子任务 d");
  await press(renderer!, "第 1 批");
  expect(text(renderer!)).not.toContain("子任务 a");
  await press(renderer!, "第 2 批");
  expect(text(renderer!)).not.toContain("子任务 b");
  await act(async () => { renderer!.unmount(); });
});

it("names a closed task's open MR by what blocks it", () => {
  const issue = { ...base, id: "m", identifier: "M", title: "M", status: "done", statusCategory: "completed" };
  const pr = (extra = {}) => ({ id: "p", number: 7, repo: "r", title: "t", state: "open", url: "https://git.example.com/o/r/merge_requests/7", ...extra });
  expect(todoPhrase({ kind: "merge", issue, pullRequests: [pr()] }, Date.now())).toEqual({ text: "!7 未合并" });
  expect(todoPhrase({ kind: "merge", issue, pullRequests: [pr({ ci: "failed" })] }, Date.now())).toEqual({ text: "!7 CI 失败", tone: "danger" });
  expect(todoPhrase({ kind: "merge", issue, pullRequests: [pr({ mergeable: "CONFLICTING" })] }, Date.now())).toEqual({ text: "!7 有冲突", tone: "danger" });
});

it("leads every task with its status glyph and leaves finished work out", async () => {
  const kid = (id: string, status: string, category: string) => ({ ...base, id, identifier: id, title: `子任务 ${id}`, status, statusCategory: category, parentId: "p" });
  const parent = { ...base, id: "p", identifier: "P", title: "父任务", status: "in_progress" };
  const closed = { ...base, id: "x", identifier: "X", title: "早已完成", status: "done", statusCategory: "completed" };
  getBoard.mockResolvedValueOnce({ ...ready("session"), issues: [parent, closed, kid("a", "done", "completed"), kid("b", "blocked", "started")], rootIds: ["p", "x"] });
  const colors = Object.fromEntries(["foreground", "foregroundMuted", "statusDanger", "statusWarning", "statusSuccess"].map((key) => [key, key]));
  let renderer: ReactTestRenderer;
  await act(async () => { renderer = create(<BoardSection host={HOST} agentId="glyph" theme={{ colors } as PluginAgentPanelProps["theme"]} openTask={vi.fn()} />); });
  expect(text(renderer!)).not.toContain("早已完成");
  expect(text(renderer!)).not.toContain("已完成");
  await press(renderer!, "展开 P");
  expect(text(renderer!)).toContain("子任务 b");
  expect(text(renderer!)).not.toContain("子任务 a");
  const glyph = (name: string) => renderer!.root.findAll((node) => (node.type as unknown) === "Pressable" && String(node.props.accessibilityLabel).startsWith(name))[0]!
    .findAll((node) => (node.type as unknown) === "Icon")[0]!.props;
  expect(glyph("b 阻塞")).toMatchObject({ color: "statusDanger" });
  expect(glyph("P 进行中")).toMatchObject({ color: "statusWarning" });
  expect(glyph("P 进行中").icon.svg).toContain("<path");
  await act(async () => { renderer!.unmount(); });
});

it("links the parent from a to-do row but not from a tree child sitting under it", async () => {
  const parent = { ...base, id: "p", identifier: "P", title: "总任务", status: "in_progress", url: "https://app.example/acme/issues/P" };
  const kid = { ...base, id: "b", identifier: "B", title: "子任务 b", status: "blocked", statusCategory: "started", parentId: "p", parent: { identifier: "P", title: "总任务", url: "https://app.example/acme/issues/P", closed: false } };
  getBoard.mockResolvedValueOnce({ ...ready("session"), issues: [parent, kid], rootIds: ["p"] });
  let renderer: ReactTestRenderer;
  await act(async () => { renderer = create(<BoardSection host={HOST} agentId="parent-link" theme={{ colors: {} } as PluginAgentPanelProps["theme"]} openTask={vi.fn()} />); });
  await press(renderer!, "展开 P");
  const expanders = renderer!.root.findAll((node) => (node.type as unknown) === "Pressable" && node.props.accessibilityLabel === "展开 B");
  expect(expanders).toHaveLength(2);
  await act(async () => { expanders[0]!.props.onPress(); });
  const rows = renderer!.root.findAll((node) => (node.type as unknown) === "Pressable" && String(node.props.accessibilityLabel).startsWith("B 阻塞"));
  expect(rows).toHaveLength(2);
  const links = renderer!.root.findAll((node) => (node.type as unknown) === "Pressable" && node.props.accessibilityLabel === "父任务 P 总任务");
  expect(links).toHaveLength(1);
  const all = text(renderer!);
  expect(all.indexOf("父任务")).toBeLessThan(all.indexOf("进行中"));
  await act(async () => { renderer!.unmount(); });
});

it("splits what only you can do from what waits on this session", async () => {
  const pending = (id: string, pendingActor: Record<string, string>, extra = {}) => ({ ...base, id, identifier: id.toUpperCase(), title: `任务 ${id}`, status: "in_progress", contract: { pendingActor, accepted: false }, ...extra });
  const me = { type: "member", id: "me" };
  const issues: BoardIssue[] = [
    pending("user", me),
    pending("session", session),
    pending("accept", session, { contract: { pendingActor: session, acceptor: session, accepted: false } }),
    task("approve", [mr("4", "open", { myReview: "approving" })], { contract: { accepted: true } }),
    task("merge", [mr("5", "open")], { status: "done", statusCategory: "completed" }),
    task("anomaly", [], { status: "blocked" }),
  ];
  getBoard.mockResolvedValueOnce({ ...ready("session"), viewer: { agentId: "leader", sessionId: "s1", userId: "me", forgeUsernames: [] }, issues, rootIds: issues.map((issue) => issue.id), archivedDeliveries: [{ id: "d", issueIdentifier: "WORK-9", issueTitle: "投递", session: { serverId: "srv_here", agentId: "gone" } }] });
  let renderer: ReactTestRenderer;
  await act(async () => { renderer = create(<BoardSection host={HOST} agentId="split" theme={{ colors: {} } as PluginAgentPanelProps["theme"]} openTask={vi.fn()} />); });
  const all = text(renderer!);
  const yours = all.slice(all.indexOf("需要你"), all.indexOf("待本会话处理"));
  const session_ = all.slice(all.indexOf("待本会话处理"));
  expect(all.indexOf("需要你")).toBeLessThan(all.indexOf("待本会话处理"));
  for (const title of ["任务 user", "任务 approve", "WORK-9 投递"]) expect(yours).toContain(title);
  for (const title of ["任务 session", "任务 accept", "任务 merge", "任务 anomaly"]) {
    expect(session_).toContain(title);
    expect(yours).not.toContain(title);
  }
  await act(async () => { renderer!.unmount(); });
});

it("opens a task's executing session from its own target, never another session's or a dead one", async () => {
  const openTask = vi.fn();
  const openAgent = vi.fn();
  const elsewhere = { serverId: "srv_here", agentId: "worker" };
  const issues: BoardIssue[] = [
    task("other", [mr("7", "open")], { url: "https://app.example/acme/issues/OTHER", executionSession: elsewhere }),
    task("remote", [], { executionSession: { serverId: "srv_far", agentId: "here" } }),
    task("self", [], { executionSession: { serverId: "srv_here", agentId: "here" } }),
    task("managed", []),
  ];
  getBoard.mockResolvedValue({ ...ready("session"), issues, rootIds: issues.map((issue) => issue.id), archivedDeliveries: [{ id: "d", issueIdentifier: "WORK-9", issueTitle: "投递", session: { serverId: "srv_here", agentId: "archived" } }] });
  const render = async (navigation?: { openAgent: typeof openAgent }) => {
    let renderer: ReactTestRenderer;
    await act(async () => { renderer = create(<BoardSection host={HOST} agentId="here" navigation={navigation as PluginAgentPanelProps["navigation"]} theme={{ colors: {} } as PluginAgentPanelProps["theme"]} openTask={openTask} />); });
    return renderer!;
  };
  const entries = (renderer: ReactTestRenderer) => renderer.root.findAll((node) => (node.type as unknown) === "Pressable" && /的执行会话$/.test(String(node.props.accessibilityLabel))).map((node) => node.props.accessibilityLabel);

  const renderer = await render({ openAgent: openAgent as never });
  expect(entries(renderer)).toEqual(["打开 OTHER 的执行会话", "打开 REMOTE 的执行会话"]);
  await press(renderer, "打开 OTHER 的执行会话");
  expect(openAgent).toHaveBeenCalledWith(elsewhere);
  expect(openTask).not.toHaveBeenCalled();
  await press(renderer, "打开 REMOTE 的执行会话");
  expect(openAgent).toHaveBeenLastCalledWith({ serverId: "srv_far", agentId: "here" });
  await press(renderer, "WORK-9 投递，投递失败：会话已归档，在 Paseo 中打开");
  expect(openAgent).toHaveBeenLastCalledWith({ serverId: "srv_here", agentId: "archived" });
  expect(openURL).not.toHaveBeenCalledWith(expect.stringMatching(/^paseo:/));
  await act(async () => { renderer.unmount(); });

  const old = await render();
  expect(entries(old)).toEqual([]);
  expect(text(old)).not.toContain("在 Paseo 中打开");
  const delivery = old.root.findAll((node) => (node.type as unknown) === "Pressable" && String(node.props.accessibilityLabel).startsWith("WORK-9"))[0]!;
  expect(delivery.props.disabled).toBe(true);
  await act(async () => { old.unmount(); });
  getBoard.mockImplementation(async ({ scope }: { scope: "session" | "all" }) => ready(scope));
});

const pressExact = async (renderer: ReactTestRenderer, name: string) => {
  const target = renderer.root.findAll((node) => (node.type as unknown) === "Pressable" && node.props.accessibilityLabel === name)[0];
  if (!target) throw new Error(`no pressable named ${name}`);
  await act(async () => { target.props.onPress(); });
};
const pressables = (renderer: ReactTestRenderer, name: string) => renderer.root.findAll((node) => (node.type as unknown) === "Pressable" && node.props.accessibilityLabel === name);

it("offers every status with the current one checked, done held back by an unaccepted contract, and rerun for a failed run", () => {
  const menu = (extra: Partial<BoardIssue>) => taskMenu({ ...base, status: "in_progress", ...extra });
  const all = ["backlog", "todo", "in_progress", "in_review", "blocked", "done", "cancelled"];
  for (const status of [...all, "custom"]) {
    const { statuses } = menu({ status });
    expect(statuses.map((choice) => choice.status)).toEqual(all);
    expect(statuses.filter((choice) => choice.current).map((choice) => choice.status)).toEqual(status === "custom" ? [] : [status]);
  }
  const done = (extra: Partial<BoardIssue>) => menu(extra).statuses.find((choice) => choice.status === "done");
  expect(done({})).toEqual({ status: "done", current: false });
  expect(done({ contract: { accepted: true } })).toEqual({ status: "done", current: false });
  expect(done({ contract: { accepted: false } })).toEqual({ status: "done", current: false, disabledReason: "需先验收合同" });
  expect(done({ status: "done", contract: { accepted: false } })).toEqual({ status: "done", current: true });
  expect(menu({ contract: { accepted: false } }).statuses.filter((choice) => choice.disabledReason).map((choice) => choice.status)).toEqual(["done"]);
  expect(menu({ lastRunFailed: true, lastFailure: { summary: "", taskId: "run-1" } }).rerunTaskId).toBe("run-1");
  // Without the failed run's id the server would rerun the assignee's own run, not the one that failed.
  expect(menu({ lastRunFailed: true }).rerunTaskId).toBeUndefined();
  expect(menu({ lastRunFailed: true, activeRun: true, lastFailure: { summary: "", taskId: "run-1" } }).rerunTaskId).toBeUndefined();
  expect(menu({}).rerunTaskId).toBeUndefined();
});

it("says why a task is here: its failed run, whose turn, how long blocked", () => {
  const viewer = { agentId: "leader", sessionId: "s1", userId: "me", forgeUsernames: [] };
  const now = Date.parse("2026-11-20T12:00:00Z");
  const days = (n: number) => new Date(now - n * 86400000).toISOString();
  const issue = (extra: Partial<BoardIssue>) => ({ ...base, id: "x", identifier: "X", title: "X", status: "in_progress", ...extra });
  expect(reasonsOf(issue({ lastRunFailed: true, lastFailure: { summary: "agent exited 1", at: days(10) } }), viewer, now)).toEqual(["运行失败（10d 前）：agent exited 1"]);
  expect(reasonsOf(issue({ lastFailure: { summary: "" } }), viewer, now)).toEqual(["运行失败"]);
  expect(reasonsOf(issue({ contract: { pendingActor: { type: "member", id: "me" }, accepted: false } }), viewer, now)).toEqual(["轮到：你"]);
  expect(reasonsOf(issue({ contract: { pendingActor: session, accepted: false } }), viewer, now)).toEqual(["轮到：本会话"]);
  expect(reasonsOf(issue({ contract: { pendingActor: { type: "agent", id: "rev" }, pendingName: "审查", accepted: false } }), viewer, now)).toEqual(["轮到：审查"]);
  expect(reasonsOf(issue({ contract: { pendingActor: { type: "agent", id: "rev" }, accepted: false } }), viewer, now)).toEqual(["轮到：其他 Agent"]);
  expect(reasonsOf(issue({ status: "blocked", blockedSince: days(3) }), viewer, now)).toEqual(["已阻塞 3d"]);
  expect(reasonsOf(issue({ blockedSince: days(3) }), viewer, now)).toEqual([]);
});

/** react-test-renderer has no layout; ⋯ measures itself at a fixed spot. */
const createNodeMock = () => ({ measureInWindow: (done: (x: number, y: number, w: number, h: number) => void) => done(360, 100, 22, 22) });
const COLORS = Object.fromEntries(["surface0", "surface1", "surface2", "border", "foreground", "foregroundMuted", "statusDanger", "statusWarning", "statusSuccess"].map((key) => [key, key]));

describe("a task row's ⋯ menu", () => {
  const failedAt = new Date(Date.now() - 10 * 86400000).toISOString();
  const failing: BoardIssue = {
    ...base, id: "fail", identifier: "WORK-60", title: "跑训练", status: "in_progress", url: "https://app.example/acme/issues/WORK-60",
    lastRunFailed: true, lastFailure: { summary: "OOM on rank 3", at: failedAt, taskId: "run-60" }, parentId: "parent-off-board",
    parent: { identifier: "WORK-50", title: "feat(train): 训练总任务", url: "https://app.example/acme/issues/WORK-50", closed: false },
    contract: { pendingActor: { type: "agent", id: "worker" }, pendingName: "开发 · Opus", accepted: false },
    pullRequests: [mr("61", "open", { ci: "failed" })], executionSession: { serverId: "srv_here", agentId: "worker" },
  };
  const blocked: BoardIssue = { ...base, id: "blk", identifier: "WORK-70", title: "等数据", status: "blocked", blockedSince: new Date(Date.now() - 4 * 86400000).toISOString() };
  const openTask = vi.fn();
  const openAgent = vi.fn();
  const render = async () => {
    getBoard.mockResolvedValue({ ...ready("session"), issues: [failing, blocked], rootIds: ["fail", "blk"] });
    let renderer: ReactTestRenderer;
        await act(async () => { renderer = create(<BoardSection host={HOST} agentId="acts" navigation={{ openAgent } as unknown as PluginAgentPanelProps["navigation"]} theme={{ colors: COLORS } as PluginAgentPanelProps["theme"]} openTask={openTask} />, { createNodeMock }); });
    return renderer!;
  };
  const menus = (renderer: ReactTestRenderer) => renderer.root.findAll((node) => (node.type as unknown) === "Modal");
  const items = (renderer: ReactTestRenderer) => renderer.root.findAll((node) => (node.type as unknown) === "Pressable" && node.props.accessibilityRole === "menuitem").map((node) => node.props.accessibilityLabel as string);
  const trigger = (renderer: ReactTestRenderer, identifier: string) => pressables(renderer, `${identifier} 的操作`)[0]!;
  afterEach(() => {
    getBoard.mockReset();
    getBoard.mockImplementation(async ({ scope }: { scope: "session" | "all" }) => ready(scope));
    runAction.mockReset();
    runAction.mockImplementation(async () => ({ ok: true }));
    openTask.mockClear(); openAgent.mockClear(); openURL.mockClear();
    platform.OS = "web";
  });

  it("sits on every row without expanding it, hidden until the row is hovered or ⋯ is focused", async () => {
    const renderer = await render();
    // WORK-60 is a to-do and a tree row; WORK-70 too: each row has its own ⋯.
    expect(pressables(renderer, "WORK-60 的操作").length).toBeGreaterThanOrEqual(1);
    expect(pressables(renderer, "WORK-70 的操作").length).toBeGreaterThanOrEqual(1);
    expect(trigger(renderer, "WORK-60").props.style.opacity).toBe(0);
    const main = pressables(renderer, "WORK-60 进行中 跑训练")[0]!;
    await act(async () => { main.props.onHoverIn(); });
    expect(trigger(renderer, "WORK-60").props.style.opacity).toBe(1);
    await act(async () => { main.props.onHoverOut(); });
    expect(trigger(renderer, "WORK-60").props.style.opacity).toBe(0);
    await act(async () => { pressables(renderer, "打开 WORK-60 的执行会话")[0]!.props.onHoverIn(); });
    expect(trigger(renderer, "WORK-60").props.style.opacity).toBe(1);
    await act(async () => { pressables(renderer, "打开 WORK-60 的执行会话")[0]!.props.onHoverOut(); });
    await act(async () => { trigger(renderer, "WORK-60").props.onFocus(); });
    expect(trigger(renderer, "WORK-60").props.style.opacity).toBe(1);
    await act(async () => { trigger(renderer, "WORK-60").props.onBlur(); });
    expect(trigger(renderer, "WORK-60").props.style.opacity).toBe(0);
    await act(async () => { renderer.unmount(); });
  });

  it("is always visible on touch screens", async () => {
    platform.OS = "ios";
    const renderer = await render();
    expect(trigger(renderer, "WORK-60").props.style.opacity).toBe(1);
    await act(async () => { renderer.unmount(); });
  });

  it("lists every status, the current one checked, cancelled last in danger, rerun on top for a failed run", async () => {
    const renderer = await render();
    await act(async () => { trigger(renderer, "WORK-60").props.onPress(); });
    expect(menus(renderer)).toHaveLength(1);
    expect(trigger(renderer, "WORK-60").props.accessibilityState.expanded).toBe(true);
    expect(items(renderer)).toEqual([
      "重跑 WORK-60", "待规划 WORK-60", "未开始 WORK-60", "进行中 WORK-60", "评审中 WORK-60", "阻塞 WORK-60", "已完成 WORK-60，需先验收合同", "已取消 WORK-60",
    ]);
    expect(text(renderer)).not.toMatch(/改为|放回|转为|恢复/);
    const menu = menus(renderer)[0]!;
    const children = menu.findByProps({ accessibilityRole: "menu" }).children as Instance[];
    expect(children.map((node) => (node.type as unknown) === "View" ? "|" : "item").join("")).toBe("item|itemitemitemitemitemitem|item");
    const item = (name: string) => menu.findAll((node) => (node.type as unknown) === "Pressable" && node.props.accessibilityLabel === name)[0]!;
    const current = item("进行中 WORK-60");
    expect(current.props.disabled).toBe(true);
    expect(current.props.accessibilityState).toEqual({ checked: true, disabled: true });
    expect(current.findAll((node) => (node.type as unknown) === "Icon" && node.props.name === "Check")).toHaveLength(1);
    expect(item("阻塞 WORK-60").props.disabled).toBe(false);
    expect(item("阻塞 WORK-60").findAll((node) => (node.type as unknown) === "Icon" && node.props.name === "Check")).toHaveLength(0);
    const done = item("已完成 WORK-60，需先验收合同");
    expect(done.props.disabled).toBe(true);
    expect(label(done)).toContain("需先验收合同");
    expect(done.props.style.opacity).toBeLessThan(1);
    const cancel = menu.findAll((node) => (node.type as unknown) === "Text" && label(node) === "已取消")[0]!;
    expect(cancel.props.style.color).toBe("statusDanger");
    expect(menu.findAll((node) => (node.type as unknown) === "Text" && label(node) === "阻塞")[0]!.props.style.color).toBe("foreground");
    await act(async () => { menus(renderer)[0]!.props.onRequestClose(); });
    // No failed run and no contract: no rerun, and done is open.
    await act(async () => { pressables(renderer, "WORK-70 的操作")[0]!.props.onPress(); });
    expect(items(renderer)).toEqual(["待规划 WORK-70", "未开始 WORK-70", "进行中 WORK-70", "评审中 WORK-70", "阻塞 WORK-70", "已完成 WORK-70", "已取消 WORK-70"]);
    expect(pressables(renderer, "已完成 WORK-70")[0]!.props.disabled).toBe(false);
    expect(pressables(renderer, "阻塞 WORK-70")[0]!.props.accessibilityState.checked).toBe(true);
    await act(async () => { renderer.unmount(); });
  });

  it("closes on an outside press, on Esc, and on a second press of ⋯", async () => {
    const renderer = await render();
    await act(async () => { trigger(renderer, "WORK-60").props.onPress(); });
    await pressExact(renderer, "关闭菜单");
    expect(menus(renderer)).toHaveLength(0);
    await act(async () => { trigger(renderer, "WORK-60").props.onPress(); });
    await act(async () => { menus(renderer)[0]!.props.onRequestClose(); });
    expect(menus(renderer)).toHaveLength(0);
    await act(async () => { trigger(renderer, "WORK-60").props.onPress(); });
    await act(async () => { trigger(renderer, "WORK-60").props.onPress(); });
    expect(menus(renderer)).toHaveLength(0);
    expect(runAction).not.toHaveBeenCalled();
    await act(async () => { renderer.unmount(); });
  });

  it("runs each action through the plugin with its issue, then refreshes the board", async () => {
    const renderer = await render();
    for (const [name, action] of [
      ["重跑 WORK-60", { kind: "rerun", taskId: "run-60" }], ["阻塞 WORK-60", { kind: "status", status: "blocked" }], ["评审中 WORK-60", { kind: "status", status: "in_review" }],
      ["进行中 WORK-70", { kind: "status", status: "in_progress" }], ["待规划 WORK-70", { kind: "status", status: "backlog" }], ["未开始 WORK-70", { kind: "status", status: "todo" }], ["已完成 WORK-70", { kind: "status", status: "done" }],
    ] as const) {
      getBoard.mockClear();
      await act(async () => { trigger(renderer, name.split(" ")[1]!).props.onPress(); });
      await pressExact(renderer, name);
      expect(menus(renderer)).toHaveLength(0);
      expect(runAction).toHaveBeenLastCalledWith({ agentId: "acts", issueId: name.endsWith("60") ? "fail" : "blk", action });
      expect(getBoard).toHaveBeenCalledWith({ agentId: "acts", scope: "session", refresh: true });
    }
    expect(openTask).not.toHaveBeenCalled();
    expect(openURL).not.toHaveBeenCalled();
    expect(openAgent).not.toHaveBeenCalled();
    await act(async () => { renderer.unmount(); });
  });

  it("asks once more inside the menu before cancelling", async () => {
    const renderer = await render();
    await act(async () => { trigger(renderer, "WORK-60").props.onPress(); });
    await pressExact(renderer, "已取消 WORK-60");
    expect(runAction).not.toHaveBeenCalled();
    expect(menus(renderer)).toHaveLength(1);
    expect(items(renderer).at(-1)).toBe("确认取消 WORK-60");
    expect(items(renderer)).toHaveLength(8);
    expect(text(renderer)).toContain("确认取消？");
    // Closing forgets the pending confirmation.
    await pressExact(renderer, "关闭菜单");
    await act(async () => { trigger(renderer, "WORK-60").props.onPress(); });
    expect(items(renderer).at(-1)).toBe("已取消 WORK-60");
    await pressExact(renderer, "已取消 WORK-60");
    await pressExact(renderer, "确认取消 WORK-60");
    expect(runAction).toHaveBeenCalledWith({ agentId: "acts", issueId: "fail", action: { kind: "status", status: "cancelled" } });
    expect(menus(renderer)).toHaveLength(0);
    await act(async () => { renderer.unmount(); });
  });

  it("spins ⋯ while an action runs and keeps the menu shut", async () => {
    const renderer = await render();
    let settle!: (value: { ok: true }) => void;
    runAction.mockImplementationOnce(() => new Promise((resolve) => { settle = resolve; }));
    await act(async () => { trigger(renderer, "WORK-70").props.onPress(); });
    await pressExact(renderer, "进行中 WORK-70");
    const busy = pressables(renderer, "WORK-70 正在改为进行中")[0]!;
    expect(busy.props.disabled).toBe(true);
    expect(busy.props.style.opacity).toBe(1);
    expect(busy.findAll((node) => (node.type as unknown) === "ActivityIndicator")).toHaveLength(1);
    expect(menus(renderer)).toHaveLength(0);
    await act(async () => { settle({ ok: true }); });
    expect(pressables(renderer, "WORK-70 正在改为进行中")).toHaveLength(0);
    expect(trigger(renderer, "WORK-70").props.disabled).toBe(false);
    await act(async () => { renderer.unmount(); });
  });

  it("shows a failure, the server's own reason included, as one dismissible line under the row until the board reloads", async () => {
    const renderer = await render();
    // The board may not know a contract the server still enforces; its refusal is shown as given.
    runAction.mockResolvedValueOnce({ ok: false, status: 409, reason: "issue_contract_acceptance_required" });
    getBoard.mockClear();
    await act(async () => { trigger(renderer, "WORK-70").props.onPress(); });
    await pressExact(renderer, "已完成 WORK-70");
    expect(runAction).toHaveBeenLastCalledWith({ agentId: "acts", issueId: "blk", action: { kind: "status", status: "done" } });
    expect(text(renderer)).toContain("已完成：issue_contract_acceptance_required");
    expect(getBoard).not.toHaveBeenCalled();
    await pressExact(renderer, "关闭 WORK-70 的错误");
    expect(text(renderer)).not.toContain("已完成：");
    runAction.mockRejectedValueOnce(new Error("rpc unavailable"));
    await act(async () => { trigger(renderer, "WORK-70").props.onPress(); });
    await pressExact(renderer, "待规划 WORK-70");
    expect(text(renderer)).toContain("待规划：rpc unavailable");
    await pressExact(renderer, "刷新");
    expect(text(renderer)).not.toContain("待规划：");
    await act(async () => { renderer.unmount(); });
  });

  it("keeps ⋯, the row, its MR chip and its session entry apart", async () => {
    const renderer = await render();
    await act(async () => { trigger(renderer, "WORK-60").props.onPress(); });
    expect(openTask).not.toHaveBeenCalled();
    expect(openURL).not.toHaveBeenCalled();
    expect(openAgent).not.toHaveBeenCalled();
    await pressExact(renderer, "关闭菜单");
    await pressExact(renderer, "WORK-60 进行中 跑训练");
    expect(openTask).toHaveBeenCalledWith("https://app.example/acme/issues/WORK-60", "WORK-60");
    await pressExact(renderer, "打开 r!61 !61 CI 失败 · 1h");
    expect(openURL).toHaveBeenCalledWith("https://git.example.com/o/r/merge_requests/61");
    await pressExact(renderer, "打开 WORK-60 的执行会话");
    expect(openAgent).toHaveBeenCalledWith({ serverId: "srv_here", agentId: "worker" });
    expect(menus(renderer)).toHaveLength(0);
    expect(runAction).not.toHaveBeenCalled();
    expect(openTask).toHaveBeenCalledTimes(1);
    await act(async () => { renderer.unmount(); });
  });

  it("expands to information only: reason, parent, merged MRs, children, no action buttons", async () => {
    const renderer = await render();
    expect(text(renderer)).not.toContain("OOM on rank 3");
    await pressExact(renderer, "展开 WORK-60");
    const details = text(renderer);
    expect(details).toMatch(/运行失败（10d 前）：OOM on rank 3\n轮到：开发 · Opus[\s\S]*父任务\n+WORK-50 训练总任务/);
    expect(details).not.toMatch(/重跑|已取消/);
    expect(renderer.root.findAll((node) => (node.type as unknown) === "Pressable" && node.props.accessibilityRole === "menuitem")).toHaveLength(0);
    await pressExact(renderer, "父任务 WORK-50 训练总任务");
    expect(openTask).toHaveBeenLastCalledWith("https://app.example/acme/issues/WORK-50", "WORK-50");
    await pressExact(renderer, "展开 WORK-70");
    expect(text(renderer)).toContain("已阻塞 4d");
    expect(text(renderer)).not.toContain("已取消");
    await act(async () => { renderer.unmount(); });
  });
});

it("folds left-over tasks under a closed parent into 遗留 after 搁置, out of the to-do blocks, with the menu still there", async () => {
  const gone = { identifier: "WORK-40", title: "feat(x): 已结束的总任务", url: "https://app.example/acme/issues/WORK-40", closed: true };
  const left: BoardIssue = { ...base, id: "left", identifier: "WORK-41", title: "遗留子任务", status: "blocked", parentId: "gone", parent: gone };
  const review: BoardIssue = { ...base, id: "rev", identifier: "WORK-42", title: "遗留评审", status: "in_review", parentId: "gone", parent: gone, contract: { pendingActor: session, acceptor: session, accepted: false } };
  const parked: BoardIssue = { ...base, id: "park", identifier: "WORK-43", title: "搁置任务", status: "backlog" };
  getBoard.mockResolvedValueOnce({ ...ready("all"), issues: [left, review, parked], rootIds: ["left", "rev", "park"] });
  const openTask = vi.fn();
  let renderer: ReactTestRenderer;
  await act(async () => { renderer = create(<BoardSection host={HOST} agentId="legacy" theme={{ colors: COLORS } as PluginAgentPanelProps["theme"]} openTask={openTask} />, { createNodeMock }); });
  const heading = pressables(renderer!, "遗留")[0]!;
  expect(heading.props.accessibilityState).toEqual({ expanded: false });
  const before = text(renderer!);
  expect(before).not.toMatch(/需要你|待本会话处理|进行中\n/);
  expect(before.indexOf("搁置")).toBeLessThan(before.indexOf("遗留"));
  expect(before).toMatch(/遗留\n2/);
  expect(before).not.toContain("WORK-41");
  expect(pressables(renderer!, "WORK-41 的操作")).toHaveLength(0);
  await act(async () => { heading.props.onPress(); });
  expect(pressables(renderer!, "遗留")[0]!.props.accessibilityState).toEqual({ expanded: true });
  expect(pressables(renderer!, "WORK-41 阻塞 遗留子任务 父任务已结束")).toHaveLength(1);
  expect(pressables(renderer!, "WORK-42 评审中 遗留评审 父任务已结束")).toHaveLength(1);
  await pressExact(renderer!, "展开 WORK-41");
  await pressExact(renderer!, "父任务 WORK-40 已结束的总任务");
  expect(openTask).toHaveBeenLastCalledWith("https://app.example/acme/issues/WORK-40", "WORK-40");
  await act(async () => { pressables(renderer!, "WORK-41 的操作")[0]!.props.onPress(); });
  await pressExact(renderer!, "已完成 WORK-41");
  expect(runAction).toHaveBeenLastCalledWith({ agentId: "legacy", issueId: "left", action: { kind: "status", status: "done" } });
  await act(async () => { renderer!.unmount(); });
  runAction.mockClear();
});

it("opens the menu below ⋯, or above it near the bottom, and keeps it inside the window", () => {
  const plain = taskMenu({ ...base, status: "in_progress" });
  const size = { width: 168, height: menuHeight(plain) };
  // Seven statuses, padding, and the separator before cancelled.
  expect(size.height).toBe(7 * 30 + 8 + 9);
  const window = { width: 400, height: 600 };
  expect(menuPlacement({ x: 360, y: 100, width: 22, height: 22 }, size, window)).toEqual({ left: 382 - 168, top: 126, upward: false });
  const low = menuPlacement({ x: 360, y: 560, width: 22, height: 22 }, size, window);
  expect(low).toEqual({ left: 214, top: 560 - 4 - size.height, upward: true });
  expect(menuPlacement({ x: 0, y: 100, width: 22, height: 22 }, size, window).left).toBe(8);
  expect(menuPlacement({ x: 395, y: 100, width: 22, height: 22 }, size, window).left).toBe(400 - 168 - 8);
  // A window too short for either side opens downward rather than off the top.
  expect(menuPlacement({ x: 360, y: 40, width: 22, height: 22 }, size, { width: 400, height: 120 }).upward).toBe(false);
  // Rerun adds an item and its own separator.
  expect(menuHeight(taskMenu({ ...base, status: "in_progress", lastRunFailed: true, lastFailure: { summary: "", taskId: "run-1" } }))).toBe(8 * 30 + 8 + 2 * 9);
});

it("opens a task listed both as a to-do and in the tree in both places at once", async () => {
  const parent: BoardIssue = { ...base, id: "p", identifier: "P", title: "父", status: "blocked" };
  const child: BoardIssue = { ...base, id: "c", identifier: "C", title: "子", status: "in_progress", parentId: "p", parent: { identifier: "P", title: "父", closed: false } };
  getBoard.mockResolvedValueOnce({ ...ready("session"), issues: [parent, child], rootIds: ["p"] });
  let renderer: ReactTestRenderer;
  await act(async () => { renderer = create(<BoardSection host={HOST} agentId="both" theme={{ colors: {} } as PluginAgentPanelProps["theme"]} openTask={vi.fn()} />, { createNodeMock }); });
  expect(pressables(renderer!, "展开 P")).toHaveLength(2);
  await act(async () => { pressables(renderer!, "展开 P")[0]!.props.onPress(); });
  expect(setExpansion).toHaveBeenLastCalledWith({ agentId: "both", issueId: "p", expanded: true });
  expect(pressables(renderer!, "收起 P")).toHaveLength(2);
  expect(pressables(renderer!, "P 的操作")).toHaveLength(2);
  let settle!: (value: { ok: false; status: number; reason: string }) => void;
  runAction.mockImplementationOnce(() => new Promise((resolve) => { settle = resolve; }));
  await act(async () => { pressables(renderer!, "P 的操作")[0]!.props.onPress(); });
  await act(async () => { pressables(renderer!, "待规划 P")[0]!.props.onPress(); });
  expect(pressables(renderer!, "P 正在改为待规划").map((node) => node.props.disabled)).toEqual([true, true]);
  await act(async () => { settle({ ok: false, status: 409, reason: "busy" }); });
  expect(text(renderer!).split("待规划：busy").length).toBe(3);
  await act(async () => { pressables(renderer!, "关闭 P 的错误")[1]!.props.onPress(); });
  expect(text(renderer!)).not.toContain("待规划：");
  expect(runAction).toHaveBeenCalledTimes(1);
  await act(async () => { renderer!.unmount(); });
});

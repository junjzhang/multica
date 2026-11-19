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
import { BoardSection, displayTitle, mergedMark, todoChip, todoPhrase, treeChip, treePhrase } from "../client/board-panel";
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
  expect(first).toMatch(/slime 修复\n+!813 待你审批/);
  expect(first).toContain("删除会话 MR\n待验收");
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
  expect(openURL).toHaveBeenCalledWith("https://git.woa.com/o/slime/merge_requests/813");

  await press(renderer!, "打开 mimikyu!818");
  expect(openURL).toHaveBeenLastCalledWith("https://git.woa.com/o/mimikyu/merge_requests/818");
  await press(renderer!, "展开 WORK-497");
  expect(text(renderer!)).toContain("mimikyu!700 旧\n已合并");
  expect(text(renderer!)).not.toContain("mimikyu!818 清理分支");

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
  expect(treePhrase(tree.stages[1]!.nodes[0]!)).toBeUndefined();
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

const mr = (id: string, state: string, extra = {}) => ({ id, number: Number(id.replace(/\D/g, "")) || 1, repo: "r", title: `MR ${id}`, state, url: `https://git.woa.com/o/r/merge_requests/${id}`, ...extra });
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
  expect(chip([mr("a1", "merged"), mr("b2", "open")])).toMatchObject({ icon: "GitPullRequest", text: "!2" });
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
  expect(urls(merge([mr("7", "open"), mr("8", "open", { ci: "failed" })]))).toEqual(["https://git.woa.com/o/r/merge_requests/8", "https://git.woa.com/o/r/merge_requests/7"]);
  expect(merge([mr("7", "open"), mr("8", "open", { ci: "failed" })])).toMatchObject({ text: "!8 +1 CI 失败" });
  const approve = [mr("5", "open", { myReview: "approving" })];
  expect(todoChip({ kind: "approval", issue: task("a", approve, { contract: { accepted: true } }), pullRequests: approve }, now)).toMatchObject({ text: "!5 待你审批", tone: "action" });
  const ci = [mr("9", "open", { ci: "failed" }), mr("6", "open")];
  const anomaly = todoChip({ kind: "anomaly", issue: task("x", ci), pullRequests: ci }, now)!;
  expect(anomaly).toMatchObject({ text: "!9 CI 失败 · 1h", tone: "danger" });
  expect(urls(anomaly)).toEqual(["https://git.woa.com/o/r/merge_requests/9"]);
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
  await act(async () => { renderer = create(<BoardSection agentId="chips" theme={{ colors } as PluginAgentPanelProps["theme"]} openTask={openTask} />); });
  openURL.mockClear();

  await press(renderer!, "打开 r!16 !16 有冲突");
  expect(openURL).toHaveBeenLastCalledWith("https://git.woa.com/o/r/merge_requests/16");
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
  expect(openURL).toHaveBeenLastCalledWith("https://git.woa.com/o/r/merge_requests/33");
  await press(renderer!, "P 进行中 任务 p 1/3");
  expect(openTask).toHaveBeenLastCalledWith("https://app.example/acme/issues/P", "P");
  await act(async () => { renderer!.unmount(); });
});

it("draws a staged parent as a pipeline with only the current batch open", async () => {
  const kid = (id: string, stage: number, done: boolean) => ({ ...base, id, identifier: id, title: `子任务 ${id}`, status: done ? "done" : "todo", statusCategory: done ? "completed" : "unstarted", parentId: "p", stage,
    pullRequests: done ? [{ id: `mr-${id}`, number: 813, repo: "slime", title: "t", state: "merged", url: "https://git.woa.com/o/slime/merge_requests/813" }] : [] });
  const parent = { ...base, id: "p", identifier: "P", title: "流水线", status: "in_progress", activeRun: true };
  getBoard.mockResolvedValueOnce({ ...ready("session"), issues: [parent, kid("a", 1, true), kid("b", 2, false), kid("c", 2, false), kid("d", 3, false)], rootIds: ["p"] });
  let renderer: ReactTestRenderer;
  await act(async () => { renderer = create(<BoardSection agentId="graph" theme={{ colors: {} } as PluginAgentPanelProps["theme"]} openTask={vi.fn()} />); });
  expect(text(renderer!)).toMatch(/流水线\s+第 2\/3 批/);
  await press(renderer!, "展开 P");
  const graph = text(renderer!);
  expect(graph).toMatch(/第 1 批[\s\S]*slime!813[\s\S]*第 2 批[\s\S]*0\/2[\s\S]*子任务 b[\s\S]*子任务 c[\s\S]*第 3 批/);
  expect(graph).not.toContain("1/1");
  expect(graph).not.toContain("0/1");
  openURL.mockClear();
  await press(renderer!, "slime!813 已合并");
  expect(openURL).toHaveBeenCalledWith("https://git.woa.com/o/slime/merge_requests/813");
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
  const pr = (extra = {}) => ({ id: "p", number: 7, repo: "r", title: "t", state: "open", url: "https://git.woa.com/o/r/merge_requests/7", ...extra });
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
  await act(async () => { renderer = create(<BoardSection agentId="glyph" theme={{ colors } as PluginAgentPanelProps["theme"]} openTask={vi.fn()} />); });
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

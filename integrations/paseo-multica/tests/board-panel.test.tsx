import React from "react";
import { act, create, type ReactTestRenderer } from "react-test-renderer";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { PluginAgentPanelProps } from "@getpaseo/plugin/client";
import type { BoardIssue, ReadyBoardData } from "../shared/board";

const { openURL } = vi.hoisted(() => ({ openURL: vi.fn() }));
vi.mock("react-native", () => ({ View: "View", Text: "Text", Pressable: "Pressable", ActivityIndicator: "ActivityIndicator", Linking: { openURL } }));
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
import { BoardSection, displayTitle, mergedMark, reasonsOf, todoChip, todoPhrase, treeChip, treePhrase } from "../client/board-panel";
import { quickActions } from "../shared/task-actions";
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

it("gives each state only the actions that fit it, never done", () => {
  const labels = (extra: Partial<BoardIssue>) => quickActions({ ...base, status: "in_progress", ...extra }).map((action) => [action.label, action.action, !!action.confirm]);
  expect(labels({ lastRunFailed: true })).toEqual([
    ["重跑", { kind: "rerun" }, false], ["转为阻塞", { kind: "status", status: "blocked" }, false], ["取消", { kind: "status", status: "cancelled" }, true],
  ]);
  expect(labels({ status: "blocked", lastRunFailed: true })).toEqual([
    ["恢复进行中", { kind: "status", status: "in_progress" }, false], ["放回待规划", { kind: "status", status: "backlog" }, false], ["取消", { kind: "status", status: "cancelled" }, true],
  ]);
  expect(labels({}).map(([label]) => label)).toEqual(["改为阻塞", "放回待规划", "取消"]);
  expect(labels({ status: "backlog" }).map(([label]) => label)).toEqual(["改为进行中", "改为阻塞", "取消"]);
  expect(labels({ status: "in_review" }).map(([label]) => label)).toEqual(["改为进行中", "改为阻塞", "放回待规划", "取消"]);
  expect(labels({ status: "done", statusCategory: "completed" })).toEqual([]);
  expect(labels({ status: "cancelled", statusCategory: "cancelled" })).toEqual([]);
  for (const status of ["in_progress", "blocked", "backlog", "todo"]) {
    expect(quickActions({ ...base, status }).some((action) => action.action.kind === "status" && (action.action.status as string) === "done")).toBe(false);
  }
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

describe("an expanded task row", () => {
  const failedAt = new Date(Date.now() - 10 * 86400000).toISOString();
  const failing: BoardIssue = {
    ...base, id: "fail", identifier: "WORK-60", title: "跑训练", status: "in_progress", url: "https://app.example/acme/issues/WORK-60",
    lastRunFailed: true, lastFailure: { summary: "OOM on rank 3", at: failedAt }, parentId: "parent-off-board",
    parent: { identifier: "WORK-50", title: "feat(train): 训练总任务", url: "https://app.example/acme/issues/WORK-50" },
    contract: { pendingActor: { type: "agent", id: "worker" }, pendingName: "开发 · Opus", accepted: false },
    pullRequests: [mr("61", "open", { ci: "failed" })], executionSession: { serverId: "srv_here", agentId: "worker" },
  };
  const blocked: BoardIssue = { ...base, id: "blk", identifier: "WORK-70", title: "等数据", status: "blocked", blockedSince: new Date(Date.now() - 4 * 86400000).toISOString() };
  const openTask = vi.fn();
  const openAgent = vi.fn();
  const render = async () => {
    getBoard.mockResolvedValue({ ...ready("session"), issues: [failing, blocked], rootIds: ["fail", "blk"] });
    let renderer: ReactTestRenderer;
    await act(async () => { renderer = create(<BoardSection host={HOST} agentId="acts" navigation={{ openAgent } as unknown as PluginAgentPanelProps["navigation"]} theme={{ colors: {} } as PluginAgentPanelProps["theme"]} openTask={openTask} />); });
    return renderer!;
  };
  afterEach(() => {
    getBoard.mockReset();
    getBoard.mockImplementation(async ({ scope }: { scope: "session" | "all" }) => ready(scope));
    runAction.mockReset();
    runAction.mockImplementation(async () => ({ ok: true }));
    openTask.mockClear(); openAgent.mockClear(); openURL.mockClear();
  });

  it("opens a to-do to its reason, parent and actions, and the parent is a link", async () => {
    const renderer = await render();
    expect(text(renderer)).not.toContain("OOM on rank 3");
    await pressExact(renderer, "展开 WORK-60");
    const details = text(renderer);
    expect(details).toMatch(/运行失败（10d 前）：OOM on rank 3\n轮到：开发 · Opus[\s\S]*父任务\n+WORK-50 训练总任务[\s\S]*重跑[\s\S]*转为阻塞[\s\S]*取消/);
    expect(details).not.toContain("改为进行中");
    await pressExact(renderer, "父任务 WORK-50 训练总任务");
    expect(openTask).toHaveBeenLastCalledWith("https://app.example/acme/issues/WORK-50", "WORK-50");
    await pressExact(renderer, "展开 WORK-70");
    expect(text(renderer)).toMatch(/已阻塞 4d[\s\S]*恢复进行中[\s\S]*放回待规划/);
    await act(async () => { renderer.unmount(); });
  });

  it("runs each action through the plugin with its issue, then refreshes the board", async () => {
    const renderer = await render();
    await pressExact(renderer, "展开 WORK-60");
    await pressExact(renderer, "展开 WORK-70");
    for (const [label, action] of [["重跑 WORK-60", { kind: "rerun" }], ["转为阻塞 WORK-60", { kind: "status", status: "blocked" }], ["恢复进行中 WORK-70", { kind: "status", status: "in_progress" }], ["放回待规划 WORK-70", { kind: "status", status: "backlog" }]] as const) {
      getBoard.mockClear();
      await pressExact(renderer, label);
      expect(runAction).toHaveBeenLastCalledWith({ agentId: "acts", issueId: label.endsWith("60") ? "fail" : "blk", action });
      expect(getBoard).toHaveBeenCalledWith({ agentId: "acts", scope: "session", refresh: true });
    }
    expect(openTask).not.toHaveBeenCalled();
    expect(openURL).not.toHaveBeenCalled();
    expect(openAgent).not.toHaveBeenCalled();
    await act(async () => { renderer.unmount(); });
  });

  it("asks once more before cancelling", async () => {
    const renderer = await render();
    await pressExact(renderer, "展开 WORK-60");
    await pressExact(renderer, "取消 WORK-60");
    expect(runAction).not.toHaveBeenCalled();
    expect(text(renderer)).toContain("确认取消 WORK-60？");
    expect(pressables(renderer, "重跑 WORK-60")).toHaveLength(0);
    await pressExact(renderer, "不取消 WORK-60");
    expect(runAction).not.toHaveBeenCalled();
    expect(text(renderer)).not.toContain("确认取消 WORK-60？");
    await pressExact(renderer, "取消 WORK-60");
    await pressExact(renderer, "确认取消 WORK-60");
    expect(runAction).toHaveBeenCalledWith({ agentId: "acts", issueId: "fail", action: { kind: "status", status: "cancelled" } });
    await act(async () => { renderer.unmount(); });
  });

  it("shows the server's reason for a refusal and leaves the board alone", async () => {
    const renderer = await render();
    await pressExact(renderer, "展开 WORK-70");
    runAction.mockResolvedValueOnce({ ok: false, status: 409, reason: "accept the current contract before completing this issue" });
    getBoard.mockClear();
    await pressExact(renderer, "恢复进行中 WORK-70");
    expect(text(renderer)).toContain("恢复进行中失败：accept the current contract before completing this issue");
    expect(getBoard).not.toHaveBeenCalled();
    expect(text(renderer)).toContain("等数据");
    runAction.mockRejectedValueOnce(new Error("rpc unavailable"));
    await pressExact(renderer, "放回待规划 WORK-70");
    expect(text(renderer)).toContain("放回待规划失败：rpc unavailable");
    await act(async () => { renderer.unmount(); });
  });

  it("marks the running action and holds the others until it settles", async () => {
    const renderer = await render();
    await pressExact(renderer, "展开 WORK-70");
    let settle!: (value: { ok: true }) => void;
    runAction.mockImplementationOnce(() => new Promise((resolve) => { settle = resolve; }));
    await pressExact(renderer, "恢复进行中 WORK-70");
    expect(text(renderer)).toContain("恢复进行中中…");
    expect(pressables(renderer, "放回待规划 WORK-70")[0]!.props.disabled).toBe(true);
    await act(async () => { settle({ ok: true }); });
    expect(text(renderer)).not.toContain("恢复进行中中…");
    expect(pressables(renderer, "放回待规划 WORK-70")[0]!.props.disabled).toBe(false);
    await act(async () => { renderer.unmount(); });
  });

  it("keeps the row, its MR chip and its session entry apart from the actions", async () => {
    const renderer = await render();
    await pressExact(renderer, "展开 WORK-60");
    await pressExact(renderer, "WORK-60 进行中 跑训练");
    expect(openTask).toHaveBeenCalledWith("https://app.example/acme/issues/WORK-60", "WORK-60");
    await pressExact(renderer, "打开 r!61 !61 CI 失败 · 1h");
    expect(openURL).toHaveBeenCalledWith("https://git.example.com/o/r/merge_requests/61");
    await pressExact(renderer, "打开 WORK-60 的执行会话");
    expect(openAgent).toHaveBeenCalledWith({ serverId: "srv_here", agentId: "worker" });
    expect(runAction).not.toHaveBeenCalled();
    expect(openTask).toHaveBeenCalledTimes(1);
    await act(async () => { renderer.unmount(); });
  });
});

it("opens a task listed both as a to-do and in the tree in both places at once", async () => {
  const parent: BoardIssue = { ...base, id: "p", identifier: "P", title: "父", status: "blocked" };
  const child: BoardIssue = { ...base, id: "c", identifier: "C", title: "子", status: "in_progress", parentId: "p", parent: { identifier: "P", title: "父" } };
  getBoard.mockResolvedValueOnce({ ...ready("session"), issues: [parent, child], rootIds: ["p"] });
  let renderer: ReactTestRenderer;
  await act(async () => { renderer = create(<BoardSection host={HOST} agentId="both" theme={{ colors: {} } as PluginAgentPanelProps["theme"]} openTask={vi.fn()} />); });
  expect(pressables(renderer!, "展开 P")).toHaveLength(2);
  await act(async () => { pressables(renderer!, "展开 P")[0]!.props.onPress(); });
  expect(setExpansion).toHaveBeenLastCalledWith({ agentId: "both", issueId: "p", expanded: true });
  expect(pressables(renderer!, "收起 P")).toHaveLength(2);
  expect(text(renderer!).split("恢复进行中").length).toBe(3);
  await act(async () => { renderer!.unmount(); });
});

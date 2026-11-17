import React from "react";
import { act, create, type ReactTestRenderer } from "react-test-renderer";
import { expect, it, vi } from "vitest";
import type { PluginAgentPanelProps } from "@getpaseo/plugin/client";
import type { BoardIssue, ReadyBoardData } from "../shared/board";

const { openURL } = vi.hoisted(() => ({ openURL: vi.fn() }));
vi.mock("react-native", () => ({ View: "View", Text: "Text", Pressable: "Pressable", Linking: { openURL } }));
vi.mock("@getpaseo/plugin/client", () => ({ useRpc: (contract: { name: string }) => (contract.name === "session.board" ? getBoard : setExpansion) }));
const setExpansion = vi.fn(async () => [] as string[]);
const recent = new Date(Date.now() - 60 * 60 * 1000).toISOString();
const base = { statusCategory: "started", lastActivityAt: recent, activeRun: false, lastRunFailed: false, pullRequests: [] };
const session = { type: "agent", id: "leader", session_id: "s1" };
const issues: BoardIssue[] = [
  { ...base, id: "root", identifier: "WORK-494", title: "统一 Leader 视图", status: "backlog", projectId: "p1", url: "https://app.example/acme/issues/WORK-494" },
  { ...base, id: "delivered", identifier: "WORK-496", title: "删除会话 MR", status: "in_review", parentId: "root", stage: 1, url: "https://app.example/acme/issues/WORK-496", contract: { pendingActor: session, acceptor: session, accepted: false } },
  { ...base, id: "approval", identifier: "WORK-470", title: "slime 修复", status: "in_review", projectId: "p1", contract: { accepted: true }, pullRequests: [{ id: "pr", number: 813, repo: "slime", title: "fix", state: "open", url: "https://git.woa.com/o/slime/merge_requests/813", ci: "passed", review: "approving", myReview: "approving", mergeable: "mergeable" }] },
  { ...base, id: "old", identifier: "WORK-100", title: "旧任务", status: "backlog" },
];
const ready = (scope: "session" | "all"): ReadyBoardData => ({ state: "ready", scope, viewer: { agentId: "leader", sessionId: "s1", forgeUsernames: ["me"] }, issues, rootIds: ["root", "approval", "old"], projects: [{ id: "p1", name: "Paseo" }], truncated: false, checkedAt: "2026-11-17T07:08:00Z" });
const getBoard = vi.fn(async ({ scope }: { scope: "session" | "all" }) => ready(scope));
import { BoardSection } from "../client/board-panel";

type Rendered = ReturnType<ReactTestRenderer["toJSON"]>;
const leaves = (node: Rendered | string): string => typeof node === "string" ? node : !node ? "" : Array.isArray(node) ? node.map(leaves).join("\n") : (node.children ?? []).map(leaves).join(node.type === "Text" ? "" : "\n");
const text = (renderer: ReactTestRenderer) => leaves(renderer.toJSON());
type Instance = ReactTestRenderer["root"];
const label = (node: Instance): string => node.children.map((child) => (typeof child === "string" ? child : label(child))).join("");
const press = async (renderer: ReactTestRenderer, name: string) => {
  const target = renderer.root.findAll((node) => (node.type as unknown) === "Pressable" && (node.props.accessibilityLabel === name || label(node).includes(name)))[0]!;
  await act(async () => { target.props.onPress(); });
};

it("renders the to-do queue, sections, scope switch and stage expansion", async () => {
  const openTask = vi.fn();
  const colors = Object.fromEntries(["surface0", "border", "foreground", "foregroundMuted", "accent", "statusDanger"].map((key) => [key, key]));
  let renderer: ReactTestRenderer;
  await act(async () => { renderer = create(<BoardSection agentId="agent" theme={{ colors } as PluginAgentPanelProps["theme"]} openTask={openTask} />); });
  const first = text(renderer!);
  expect(getBoard).toHaveBeenCalledWith({ agentId: "agent", scope: "session", refresh: false });
  expect(first).toContain("待办 ");
  expect(first).toContain("等你审批");
  expect(first).toContain("slime !813");
  expect(first).toContain("CI ✓ 审 ⋯ 合 ✓");
  expect(first).toContain("去验收");
  expect(first.indexOf("等你审批")).toBeLessThan(first.indexOf("等验收"));
  expect(first).toContain("需要处理 ");
  expect(first).toContain("搁置 ");
  expect(first).toContain("（Backlog 1 · 7 天无动静 0）");
  expect(first).not.toContain("WORK-100 旧任务");
  expect(first).toContain("◐ WORK-494");
  expect(first).toContain("第1批 0/1");
  expect(first).not.toContain("第 1 批");

  await press(renderer!, "展开 WORK-494");
  expect(setExpansion).toHaveBeenCalledWith({ agentId: "agent", issueId: "root", expanded: true });
  expect(text(renderer!)).toContain("第 1 批 ── 0/1");
  expect(text(renderer!)).toContain("WORK-496 删除会话 MR");

  await press(renderer!, "[展开]");
  expect(text(renderer!)).toContain("WORK-100 旧任务");

  await press(renderer!, "去验收");
  expect(openTask).toHaveBeenCalledWith("https://app.example/acme/issues/WORK-496", "WORK-496");
  await press(renderer!, "打开");
  expect(openURL).toHaveBeenCalledWith("https://git.woa.com/o/slime/merge_requests/813");

  await press(renderer!, "切换范围");
  expect(getBoard).toHaveBeenLastCalledWith({ agentId: "agent", scope: "all", refresh: false });
  expect(text(renderer!)).toContain("全部");
  expect(text(renderer!)).toContain("Paseo");
  expect(text(renderer!)).toContain("无项目");

  await press(renderer!, "刷新");
  expect(getBoard).toHaveBeenLastCalledWith({ agentId: "agent", scope: "all", refresh: true });
  await act(async () => { renderer!.unmount(); });
});

it("explains a disconnected session instead of an empty board", async () => {
  getBoard.mockResolvedValueOnce({ state: "disconnected", reason: "Run multica session connect.", checkedAt: "x" } as never);
  let renderer: ReactTestRenderer;
  await act(async () => { renderer = create(<BoardSection agentId="agent" theme={{ colors: {} } as PluginAgentPanelProps["theme"]} openTask={vi.fn()} />); });
  expect(text(renderer!)).toContain("未连接 Multica。Run multica session connect.");
  await act(async () => { renderer!.unmount(); });
});

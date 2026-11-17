// @vitest-environment node
import type { PluginButton, PluginClientContext } from "@getpaseo/plugin/client";
import { describe, expect, it, vi } from "vitest";

vi.mock("react-native", () => ({ View: "View", ScrollView: "ScrollView", Text: "Text", Pressable: "Pressable", Platform: { OS: "web" }, Linking: {} }));
vi.mock("@getpaseo/plugin/client/ui", () => ({ SidebarRow: () => null, Icon: () => null, Loading: () => null }));
vi.mock("@getpaseo/plugin/client", async (importOriginal) => ({ ...(await importOriginal<object>()), useRpc: () => vi.fn(), openExternalUrl: vi.fn(), useHosts: () => [] }));
import contribute from "../index.client";
import { PILL_ICON, UNAVAILABLE_PILL, progressRingSvg } from "../client/pill";
import type { BoardData, BoardIssue } from "../shared/board";

function treeOf(agentId: string, children: { done: number; total: number } | undefined): BoardData {
  const root = `${agentId}-root`;
  const base = { identifier: "WORK-1", status: "in_progress", statusCategory: "started", activeRun: false, lastRunFailed: false, pullRequests: [] };
  const kids: BoardIssue[] = Array.from({ length: children?.total ?? 0 }, (_, index) => ({
    ...base, id: `${root}-${index}`, title: "child", parentId: root,
    ...(index < (children?.done ?? 0) ? { status: "done", statusCategory: "completed" } : {}),
  }));
  return {
    state: "ready", scope: "session", viewer: { agentId, forgeUsernames: [] },
    issues: [{ ...base, id: root, title: `Parent of ${agentId}` }, ...kids], rootIds: [root], projects: [], truncated: false, checkedAt: "now",
  };
}

function harness(answer: (agentId: string) => Promise<BoardData>) {
  const pills = new Map<string, { button: PluginButton; onPress(): void }>();
  let emit: (message: unknown) => void = () => {};
  const noop = () => () => {};
  const client = new Proxy({
    rpc: vi.fn((_contract: unknown, { agentId }: { agentId: string }) => answer(agentId)),
    openPanel: vi.fn(),
    addComposerPill({ agentId, button }: { agentId: string; button: PluginButton }) {
      const entry = { button: { ...button }, onPress: () => (button.behavior.kind === "action" ? void button.behavior.onPress() : undefined) };
      pills.set(agentId, entry);
      return { update: (patch: Partial<PluginButton>) => Object.assign(entry.button, patch), remove: () => pills.delete(agentId) };
    },
    paseo: {
      agents: {
        list: async () => ({ subscription: { subscribe: (handlers: { snapshot(input: unknown): void; update(message: unknown): void }) => {
          handlers.snapshot({ entries: [{ agent: { id: "a", workspaceId: "ws" } }, { agent: { id: "b", workspaceId: "ws" } }] });
          emit = handlers.update;
        } } }),
      },
    },
  }, { get: (target, key) => (key in target ? target[key as keyof typeof target] : noop) }) as unknown as PluginClientContext;
  contribute(client);
  const settle = () => new Promise((done) => setTimeout(done, 0));
  return { pills, settle, update: (agentId: string) => emit({ type: "agent_update", payload: { kind: "upsert", agent: { id: agentId, workspaceId: "ws" } } }) };
}

describe("composer pill registration", () => {
  it("draws each session's own ring and restores the icon when the next read loses it", async () => {
    const answers = new Map<string, () => Promise<BoardData>>([
      ["a", async () => treeOf("a", { done: 1, total: 4 })],
      ["b", async () => treeOf("b", undefined)],
    ]);
    const { pills, settle, update } = harness((agentId) => answers.get(agentId)!());
    await settle();
    expect(pills.get("a")?.button.icon).toEqual({ svg: progressRingSvg({ done: 1, total: 4 }) });
    expect(pills.get("a")?.button.title).toBe("WORK-1 Parent of a\n已完成 1/4 个子任务");
    expect(pills.get("b")?.button.icon).toBe(PILL_ICON);

    answers.set("a", async () => treeOf("a", { done: 4, total: 4 }));
    update("a");
    await settle();
    expect(pills.get("a")?.button.icon).toEqual({ svg: progressRingSvg({ done: 4, total: 4 }) });

    answers.set("a", async () => { throw new Error("rpc failed"); });
    pills.get("a")?.onPress();
    await settle();
    expect(pills.get("a")?.button).toMatchObject(UNAVAILABLE_PILL);

    answers.set("a", async () => treeOf("a", undefined));
    update("a");
    await settle();
    expect(pills.get("a")?.button).toMatchObject({ icon: PILL_ICON, title: "WORK-1 Parent of a" });
  });

  it("ignores an older read that finishes after a newer one", async () => {
    let releaseOld: (view: BoardData) => void = () => {};
    let calls = 0;
    const { pills, settle, update } = harness(async (agentId) => {
      if (agentId !== "a") return treeOf(agentId, undefined);
      calls += 1;
      if (calls === 1) return new Promise<BoardData>((done) => { releaseOld = done; });
      return treeOf("a", undefined);
    });
    await settle();
    update("a");
    await settle();
    releaseOld(treeOf("a", { done: 2, total: 3 }));
    await settle();
    expect(pills.get("a")?.button.icon).toBe(PILL_ICON);
  });
});

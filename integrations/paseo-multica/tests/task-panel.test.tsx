import type { PluginClientContext, PluginAgentPanelProps } from "@getpaseo/plugin/client";
import React, { useState } from "react";
import { act, create, type ReactTestRenderer } from "react-test-renderer";
import { expect, it, vi } from "vitest";
vi.mock("react-native", () => ({ View: "View", Text: "Text", Pressable: "Pressable" }));
vi.mock("../client/surface", () => ({ MulticaSurface: ({ path }: { path: string }) => React.createElement("Text", null, path) }));
import { createTaskPanels, parseTaskState, TASK_PANEL_ID, visitTask } from "../client/task-panel";
const a = "/acme/issues/WORK-1";
const b = "/acme/issues/WORK-2";
const link = (path: string) => `https://example.test${path}`;
it("validates persisted paths and bounds history", () => {
  for (const state of [null, {}, { paths: ["//evil.test/x"] }, { paths: ["/acme/issues/../x"] }, { paths: ["/acme/issues/%2e%2e"] }, { paths: Array(11).fill(a) }]) expect(parseTaskState(state)).toEqual({ paths: [] });
  expect(parseTaskState({ paths: [a, b] })).toEqual({ paths: [a, b] });
  expect(visitTask({ paths: [a] }, a)).toEqual({ paths: [a] });
  expect(visitTask({ paths: Array(10).fill(a) }, b).paths).toHaveLength(10);
});
it("registers a static panel, restores state after reload, switches and goes back", async () => {
  const remove = vi.fn();
  const client = { addWorkspacePanel: vi.fn(() => remove), openPanel: vi.fn() } as unknown as PluginClientContext;
  let panels = createTaskPanels(client);
  expect(client.addWorkspacePanel).toHaveBeenCalledTimes(1);
  expect(vi.mocked(client.addWorkspacePanel).mock.calls[0]![0].id).toBe(TASK_PANEL_ID);
  let stored: unknown = undefined;
  const mount = async () => {
    const Component = vi.mocked(client.addWorkspacePanel).mock.calls.at(-1)![0].Component as React.ComponentType<PluginAgentPanelProps>;
    function Host() {
      const [state, setState] = useState(stored);
      return <Component {...({ workspaceId: "ws", agentId: "session-a", host: { id: "host", label: "host" }, layout: { compact: false, platform: "web" }, theme: { colors: { surface0: "white", foreground: "black", foregroundMuted: "gray", accent: "blue" } }, tab: { state, setState: (next: unknown) => { stored = next; setState(next); }, setModified: () => {} } } as unknown as PluginAgentPanelProps)} />;
    }
    let renderer: ReactTestRenderer;
    await act(async () => { renderer = create(<Host />); });
    return renderer!;
  };
  panels.open({ host: { id: "host", label: "host" }, workspaceId: "ws", agentId: "session-a" }, link(a));
  let renderer = await mount();
  expect(JSON.stringify(renderer.toJSON())).toContain(a);
  await act(async () => { panels.open({ host: { id: "host", label: "host" }, workspaceId: "ws", agentId: "session-a" }, link(b)); });
  expect(JSON.stringify(renderer.toJSON())).toContain(b);
  expect(stored).toEqual({ scope: JSON.stringify(["host", "ws", "session-a"]), paths: [a, b] });
  await act(async () => { renderer.root.findByProps({ accessibilityRole: "button" }).props.onPress(); });
  expect(stored).toEqual({ scope: JSON.stringify(["host", "ws", "session-a"]), paths: [a] });
  await act(async () => { renderer.unmount(); });
  panels.dispose();
  panels = createTaskPanels(client);
  renderer = await mount();
  expect(JSON.stringify(renderer.toJSON())).toContain(a);
  expect(vi.mocked(client.openPanel).mock.calls).toEqual([[TASK_PANEL_ID, { workspaceId: "ws", agentId: "session-a", location: "explorer" }], [TASK_PANEL_ID, { workspaceId: "ws", agentId: "session-a", location: "explorer" }]]);
  await act(async () => { renderer.unmount(); });
  panels.dispose();
});

it("updates every mounted instance and keeps the remaining listener after one closes", async () => {
  const client = { addWorkspacePanel: vi.fn(() => vi.fn()), openPanel: vi.fn() } as unknown as PluginClientContext;
  const panels = createTaskPanels(client);
  const Component = vi.mocked(client.addWorkspacePanel).mock.calls[0]![0].Component as React.ComponentType<PluginAgentPanelProps>;
  const scope = JSON.stringify(["host", "ws", "session-a"]);
  const states: unknown[] = [{ scope, paths: [a] }, { scope, paths: [a] }];
  function Host({ index }: { index: number }) {
    const [state, setState] = useState(states[index]);
    return <Component {...({ workspaceId: "ws", agentId: "session-a", host: { id: "host", label: "host" }, layout: { compact: false, platform: "web" }, theme: { colors: { surface0: "white", foreground: "black", foregroundMuted: "gray", accent: "blue" } }, tab: { state, setState: (next: unknown) => { states[index] = next; setState(next); }, setModified: () => {} } } as unknown as PluginAgentPanelProps)} />;
  }
  let first: ReactTestRenderer, second: ReactTestRenderer;
  await act(async () => { first = create(<Host index={0} />); second = create(<Host index={1} />); });
  await act(async () => { panels.open({ host: { id: "host", label: "host" }, workspaceId: "ws", agentId: "session-a" }, link(b)); });
  expect(states).toEqual([{ scope, paths: [a, b] }, { scope, paths: [a, b] }]);
  const firstPage = first!.root.findByType(vi.mocked((await import("../client/surface")).MulticaSurface));
  const secondPage = second!.root.findByType(vi.mocked((await import("../client/surface")).MulticaSurface));
  expect(firstPage.props.pageKey).not.toBe(secondPage.props.pageKey);
  await act(async () => { second!.unmount(); });
  await act(async () => { panels.open({ host: { id: "host", label: "host" }, workspaceId: "ws", agentId: "session-a" }, link(a)); });
  expect(states[0]).toEqual({ scope, paths: [a, b, a] });
  await act(async () => { second = create(<Host index={1} />); });
  expect(states[1]).toEqual({ scope, paths: [a, b, a] });
  await act(async () => { first!.unmount(); second!.unmount(); });
  panels.dispose();
});


it("isolates two sessions in the same workspace and rejects another session’s saved history", async () => {
  const client = { addWorkspacePanel: vi.fn(() => vi.fn()), openPanel: vi.fn() } as unknown as PluginClientContext;
  const panels = createTaskPanels(client);
  const Component = vi.mocked(client.addWorkspacePanel).mock.calls[0]![0].Component as React.ComponentType<PluginAgentPanelProps>;
  const states: Record<string, unknown> = { "session-b": { scope: JSON.stringify(["host", "ws", "session-a"]), paths: [a] } };
  function Host({ agentId }: { agentId: string }) {
    const [state, setState] = useState(states[agentId]);
    return <Component {...({ workspaceId: "ws", agentId, host: { id: "host" }, theme: { colors: {} }, tab: { state, setState: (next: unknown) => { states[agentId] = next; setState(next); } } } as unknown as PluginAgentPanelProps)} />;
  }
  panels.open({ host: { id: "host", label: "host" }, workspaceId: "ws", agentId: "session-a" }, link(a));
  let first: ReactTestRenderer, second: ReactTestRenderer;
  await act(async () => { first = create(<Host agentId="session-a" />); second = create(<Host agentId="session-b" />); });
  expect(JSON.stringify(first!.toJSON())).toContain(a);
  expect(JSON.stringify(second!.toJSON())).not.toContain(a);
  await act(async () => { panels.open({ host: { id: "host", label: "host" }, workspaceId: "ws", agentId: "session-b" }, link(b)); });
  expect(JSON.stringify(first!.toJSON())).not.toContain(b);
  expect(JSON.stringify(second!.toJSON())).toContain(b);
  expect(states["session-b"]).toEqual({ scope: JSON.stringify(["host", "ws", "session-b"]), paths: [b] });
  await act(async () => { first!.unmount(); second!.unmount(); });
  panels.dispose();
});

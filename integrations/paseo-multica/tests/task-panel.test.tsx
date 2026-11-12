import type { PluginClientContext } from "@getpaseo/plugin/client";
import { expect, it, vi } from "vitest";
vi.mock("../client/surface", () => ({ MulticaSurface: () => null }));
import { createTaskPanels } from "../client/task-panel";

it("opens and reuses a panel for each task", () => {
  const remove = vi.fn();
  const client = { addWorkspacePanel: vi.fn(() => remove), openPanel: vi.fn() } as unknown as PluginClientContext;
  const panels = createTaskPanels(client);
  panels.open("workspace", "https://example.test/acme/issues/WORK-1", "WORK-1");
  panels.open("workspace", "https://example.test/acme/issues/WORK-1", "WORK-1");
  panels.open("workspace", "https://example.test/acme/issues/WORK-2", "WORK-2");
  expect(client.addWorkspacePanel).toHaveBeenCalledTimes(2);
  expect(client.openPanel).toHaveBeenCalledTimes(3);
  expect(vi.mocked(client.openPanel).mock.calls[0]).toEqual(vi.mocked(client.openPanel).mock.calls[1]);
  expect(vi.mocked(client.openPanel).mock.calls[2]).not.toEqual(vi.mocked(client.openPanel).mock.calls[1]);
  panels.dispose();
  expect(remove).toHaveBeenCalledTimes(2);
});

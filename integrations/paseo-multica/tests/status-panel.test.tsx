import React from "react";
import { act, create, type ReactTestRenderer } from "react-test-renderer";
import { expect, it, vi } from "vitest";
import type { PluginAgentPanelProps } from "@getpaseo/plugin/client";

vi.mock("react-native", () => ({ View: "View", ScrollView: "ScrollView", Text: "Text", Pressable: "Pressable" }));
vi.mock("@getpaseo/plugin/client", () => ({ useRpc: () => getStatus }));
vi.mock("../client/board-panel", () => ({ BoardSection: () => null }));
const getStatus = vi.fn(async () => ({ state: "connected", agentId: "agent-id", workspaceId: "workspace-id", expiresAt: "2027-02-01T00:00:00Z" }));
import { StatusPanel } from "../client/status-panel";

const colorsFor = (mode: string) => Object.fromEntries(["surface0", "surface1", "surface2", "border", "foreground", "foregroundMuted", "accent", "accentForeground", "statusSuccess", "statusWarning", "statusDanger"].map((key) => [key, `${mode}-${key}`]));
const render = async (colors: Record<string, string>) => {
  const props = { host: { id: "host" }, workspaceId: "workspace", agentId: "agent", theme: { colors } } as PluginAgentPanelProps;
  let renderer: ReactTestRenderer;
  await act(async () => { renderer = create(<StatusPanel {...props} openTask={vi.fn()} />); });
  return renderer!;
};

it("adds no connection footer when the board can explain the session", async () => {
  for (const state of ["connected", "disconnected"]) {
    getStatus.mockResolvedValueOnce({ state, agentId: "agent-id", workspaceId: "workspace-id", expiresAt: "2027-02-01T00:00:00Z" });
    const renderer = await render(colorsFor("light"));
    expect(JSON.stringify(renderer.toJSON())).not.toContain("连接");
    await act(async () => { renderer.unmount(); });
  }
});

for (const mode of ["light", "dark"]) {
  it(`collapses details of a binding that needs attention using ${mode} colors`, async () => {
    getStatus.mockResolvedValueOnce({ state: "invalid", reason: "Credential revoked", agentId: "agent-id", workspaceId: "workspace-id", expiresAt: "2027-02-01T00:00:00Z" } as never);
    const renderer = await render(colorsFor(mode));
    const before = JSON.stringify(renderer.toJSON());
    expect(before).toContain("连接需要处理");
    expect(before).toContain("Credential revoked");
    expect(before).toContain(`${mode}-foregroundMuted`);
    expect(before).not.toContain("workspace-id");
    await act(async () => { renderer.root.findAllByProps({ accessibilityRole: "button" })[0]!.props.onPress(); });
    expect(JSON.stringify(renderer.toJSON())).toContain("workspace-id");
    await act(async () => { renderer.unmount(); });
  });
}

it("keeps connection errors visible while details are collapsed", async () => {
  getStatus.mockRejectedValueOnce(new Error("Reconnect this session"));
  const renderer = await render(Object.fromEntries(["surface0", "foregroundMuted", "statusDanger"].map((key) => [key, key])));
  expect(JSON.stringify(renderer.toJSON())).toContain("Reconnect this session");
  await act(async () => { renderer.unmount(); });
});

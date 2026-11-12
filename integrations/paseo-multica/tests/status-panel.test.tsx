import React from "react";
import { act, create, type ReactTestRenderer } from "react-test-renderer";
import { expect, it, vi } from "vitest";
import type { PluginAgentPanelProps } from "@getpaseo/plugin/client";

vi.mock("react-native", () => ({ View: "View", ScrollView: "ScrollView", Text: "Text", Pressable: "Pressable" }));
vi.mock("@getpaseo/plugin/client", () => ({ useRpc: () => getStatus }));
vi.mock("../client/session-tasks", () => ({ SessionTasksSection: () => null }));
const getStatus = vi.fn(async () => ({ state: "connected", agentId: "agent-id", workspaceId: "workspace-id", expiresAt: "2027-02-01T00:00:00Z" }));
import { StatusPanel } from "../client/status-panel";

for (const mode of ["light", "dark"]) {
  it(`collapses connection details using ${mode} colors`, async () => {
    const colors = Object.fromEntries(["surface0", "surface1", "surface2", "border", "foreground", "foregroundMuted", "accent", "accentForeground", "statusSuccess", "statusWarning", "statusDanger"].map((key) => [key, `${mode}-${key}`]));
    const props = { agentId: "agent", theme: { colors } } as PluginAgentPanelProps;
    let renderer: ReactTestRenderer;
    await act(async () => { renderer = create(<StatusPanel {...props} />); });
    const before = JSON.stringify(renderer!.toJSON());
    expect(before).toContain("Credential expires 2027-02");
    expect(before).toContain(`${mode}-foregroundMuted`);
    expect(before).not.toContain("workspace-id");
    await act(async () => { renderer!.root.findByProps({ accessibilityRole: "button" }).props.onPress(); });
    expect(JSON.stringify(renderer!.toJSON())).toContain("workspace-id");
    await act(async () => { renderer!.unmount(); });
  });
}

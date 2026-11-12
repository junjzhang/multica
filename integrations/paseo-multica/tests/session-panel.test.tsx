import React from "react";
import { act, create, type ReactTestRenderer } from "react-test-renderer";
import { expect, it, vi } from "vitest";
import type { PluginAgentPanelProps } from "@getpaseo/plugin/client";
import { makeTasksWire } from "./fixtures";
import { toTasksView } from "../shared/tasks";

const { openURL } = vi.hoisted(() => ({ openURL: vi.fn() }));
vi.mock("react-native", () => ({ View: "View", Text: "Text", Pressable: "Pressable", Linking: { openURL } }));
vi.mock("@getpaseo/plugin/client", () => ({ useRpc: () => getTasks }));
const getTasks = vi.fn(async () => toTasksView(makeTasksWire(), "2026-11-11T12:00:00Z"));
import { SessionTasksSection } from "../client/session-tasks";

for (const mode of ["light", "dark"]) {
  it(`shows task groups with ${mode} theme colors and opens a task`, async () => {
    const colors = Object.fromEntries(["surface0", "surface1", "surface2", "border", "foreground", "foregroundMuted", "accent", "accentForeground", "statusSuccess", "statusWarning", "statusDanger"].map((key) => [key, `${mode}-${key}`]));
    const theme = { colors } as PluginAgentPanelProps["theme"];
    let renderer: ReactTestRenderer;
    await act(async () => { renderer = create(<SessionTasksSection agentId="agent" theme={theme} />); });
    const output = JSON.stringify(renderer!.toJSON());
    expect(output.indexOf("Needs your attention")).toBeLessThan(output.indexOf("Delegated by you"));
    expect(output.indexOf("Delegated by you")).toBeLessThan(output.indexOf("Your tasks"));
    expect(output).toContain(`${mode}-surface1`);
    expect(output).toContain(`${mode}-foreground`);
    const links = renderer!.root.findAllByProps({ accessibilityRole: "link" });
    expect(links.length).toBeGreaterThan(0);
    await act(async () => { links[0]!.props.onPress(); });
    expect(openURL).toHaveBeenCalled();
    await act(async () => { renderer!.unmount(); });
  });
}

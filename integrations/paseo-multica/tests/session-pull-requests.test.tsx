import React from "react";
import { act, create, type ReactTestRenderer } from "react-test-renderer";
import { expect, it, vi } from "vitest";
import type { PluginAgentPanelProps } from "@getpaseo/plugin/client";

const { fetch, openURL } = vi.hoisted(() => ({ fetch: vi.fn(), openURL: vi.fn() }));
vi.mock("react-native", () => ({ View: "View", Text: "Text", Pressable: "Pressable", Linking: { openURL } }));
vi.mock("@getpaseo/plugin/client", () => ({ useRpc: () => fetch }));
import { SessionPullRequests } from "../client/session-pull-requests";

it("loads on expansion, prioritizes actionable PRs, and opens both destinations", async () => {
  const base = { repo_owner: "acme", repo_name: "repo", state: "draft", source_identifier: "WORK-2", source_url: "https://example.test/task" };
  fetch.mockResolvedValue({ pull_requests: [
    { ...base, id: "ok", number: 1, title: "OK", html_url: "https://example.test/1" },
    { ...base, id: "bad", number: 2, title: "Failed", html_url: "https://example.test/2", checks_failed: 1, snapshot_stale: true },
  ] });
  const openTask = vi.fn();
  const theme = { colors: { foreground: "fg", foregroundMuted: "muted", accent: "accent", statusDanger: "danger" } } as PluginAgentPanelProps["theme"];
  let renderer: ReactTestRenderer;
  await act(async () => { renderer = create(<SessionPullRequests agentId="session" theme={theme} openTask={openTask} />); });
  expect(fetch).not.toHaveBeenCalled();
  await act(async () => { renderer!.root.findByProps({ accessibilityRole: "button" }).props.onPress(); });
  const output = JSON.stringify(renderer!.toJSON());
  expect(output.indexOf("Failed")).toBeLessThan(output.indexOf("OK"));
  expect(output).toContain("Snapshot stale");
  const links = renderer!.root.findAllByProps({ accessibilityRole: "link" });
  await act(async () => { links[0]!.props.onPress(); links[1]!.props.onPress(); });
  expect(openURL).toHaveBeenCalledWith("https://example.test/2");
  expect(openTask).toHaveBeenCalledWith("https://example.test/task", "WORK-2");
  await act(async () => { renderer!.unmount(); });
});

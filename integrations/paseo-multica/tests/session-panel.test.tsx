import React from "react";
import { act, create, type ReactTestRenderer } from "react-test-renderer";
import { afterEach, expect, it, vi } from "vitest";
import type { PluginAgentPanelProps } from "@getpaseo/plugin/client";
import { makeTasksWire } from "./fixtures";
import { toTasksView } from "../shared/tasks";
import type { DetailResult } from "../shared/task-details";

const { openURL, copyText } = vi.hoisted(() => ({ openURL: vi.fn(), copyText: vi.fn(async () => {}) }));
vi.mock("react-native", () => ({ View: "View", Text: "Text", Pressable: "Pressable", Linking: { openURL } }));
vi.mock("@getpaseo/plugin/client", () => ({ useRpc: (contract: { name: string }) => contract.name === "session.tasks" ? getTasks : contract.name === "session.task-expansion" ? getExpansion : getDetail }));
vi.mock("@getpaseo/plugin/client/ui", () => ({ copyText }));
const getExpansion = vi.fn(async () => [] as string[]);
const getDetail = vi.fn(async ({ section }: { issueId: string; section: string }): Promise<DetailResult> => ({ ok: true, data: ({ contract: { contract: null, accepted: [] }, runs: [], comments: [], prs: { pull_requests: [] }, children: { issues: [] } } as Record<string, unknown>)[section] }));
afterEach(() => { getDetail.mockClear(); });
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
    expect(getDetail).not.toHaveBeenCalled();
    const card = renderer!.root.findAll((node) => node.props.accessibilityState?.expanded === false).at(-1)!;
    await act(async () => { card.props.onPress(); });
    const links = renderer!.root.findAllByProps({ accessibilityRole: "link" });
    expect(links.length).toBeGreaterThan(0);
    await act(async () => { links[0]!.props.onPress(); });
    expect(openURL).toHaveBeenCalled();
    const copy = renderer!.root.findAll((node) => node.props.accessibilityRole === "button" && node.findAll((child) => child.props.children === "Copy ID").length > 0)[0]!;
    await act(async () => { copy.props.onPress(); });
    expect(copyText).toHaveBeenCalledWith("WORK-1");
    await act(async () => { renderer!.unmount(); });
    getDetail.mockClear();
  });
}

it("shows request evidence and the source of a blocked reason", async () => {
  const base = makeTasksWire();
  const request = { ...base.returning[0]!, role: "request" as const, request_id: "comment-1", request_at: "2026-11-11T12:00:00Z", url: "https://example.test/task" };
  const delegated = { ...base.returning[0]!, status: "blocked", blocked_reason: "Waiting for input" };
  getTasks.mockResolvedValueOnce(toTasksView({ ...base, requests: [request], delegated: [delegated] }, "2026-11-11T12:00:00Z"));
  const colors = Object.fromEntries(["surface0", "surface1", "surface2", "border", "foreground", "foregroundMuted", "accent", "accentForeground", "statusSuccess", "statusWarning", "statusDanger"].map((key) => [key, key]));
  let renderer: ReactTestRenderer;
  await act(async () => { renderer = create(<SessionTasksSection agentId="agent" theme={{ colors } as PluginAgentPanelProps["theme"]} />); });
  const output = JSON.stringify(renderer!.toJSON());
  expect(output).toContain("Executor mentioned you · awaiting your reply");
  expect(output).toContain("From executor's latest comment: Waiting for input");
  await act(async () => { renderer!.unmount(); });
});

it("renders empty groups as a single muted line", async () => {
  const base = makeTasksWire();
  getTasks.mockResolvedValueOnce(toTasksView({ ...base, owned: [], accepting: [], returning: [], delegated: [], requests: [] }, "2026-11-11T12:00:00Z"));
  const colors = Object.fromEntries(["surface0", "surface1", "surface2", "border", "foreground", "foregroundMuted", "accent", "accentForeground", "statusSuccess", "statusWarning", "statusDanger"].map((key) => [key, key]));
  let renderer: ReactTestRenderer;
  await act(async () => { renderer = create(<SessionTasksSection agentId="agent" theme={{ colors } as PluginAgentPanelProps["theme"]} />); });
  const output = JSON.stringify(renderer!.toJSON());
  expect(output).toContain("Needs your attention");
  expect(output).not.toContain("None");
  expect(renderer!.root.findAll((node) => node.props.style?.color === "foregroundMuted" && Array.isArray(node.props.children))).toHaveLength(3);
  await act(async () => { renderer!.unmount(); });
});

it("loads one level at a time, isolates errors, and restores expanded cards", async () => {
  const parent = makeTasksWire().owned[0]!;
  const childId = "0199a0b1-7777-7777-8777-777777777777";
  let expanded: string[] = [];
  getExpansion.mockImplementation(async (input?: { issueId?: string; expanded?: boolean }) => {
    if (input?.issueId) expanded = input.expanded ? [...expanded, input.issueId] : expanded.filter((id) => id !== input.issueId);
    return expanded;
  });
  getTasks.mockResolvedValue(toTasksView(makeTasksWire({ owned: [parent], accepting: [], returning: [] }), "2026-11-11T12:00:00Z"));
  getDetail.mockImplementation(async ({ issueId, section }: { issueId: string; section: string }) => {
    if (section === "runs") return { ok: false, error: "runs unavailable" };
    const data: Record<string, unknown> = {
      contract: { contract: { versions: [{ number: 2, criteria: ["a", "b"] }], deliveries: [{ version: 2, created_at: "2026-11-11T12:00:00Z" }] }, accepted: [0], pending_actor: { id: "acceptor", type: "agent" }, acceptor: { id: "acceptor", type: "agent" } },
      comments: [{ author_type: "agent", author_id: "a", author_name: "Alice", content: "first line\nsecond line", created_at: "2026-11-11T12:00:00Z" }],
      prs: { pull_requests: [{ number: 42, title: "Fix", html_url: "https://example.test/pr" }] },
      children: { issues: issueId === parent.issue_id ? [{ id: childId, identifier: "WORK-3", title: "Child", status: "todo", assignee_name: "Bob" }] : [] },
    };
    return { ok: true, data: data[section] };
  });
  const colors = Object.fromEntries(["surface0", "surface1", "surface2", "border", "foreground", "foregroundMuted", "accent", "accentForeground", "statusSuccess", "statusWarning", "statusDanger"].map((key) => [key, key]));
  let renderer: ReactTestRenderer;
  const mount = async () => { await act(async () => { renderer = create(<SessionTasksSection agentId="agent" theme={{ colors } as PluginAgentPanelProps["theme"]} />); }); };
  await mount();
  expect(getDetail).not.toHaveBeenCalled();
  await act(async () => { renderer!.root.findAll((node) => node.props.accessibilityState?.expanded === false)[0]!.props.onPress(); });
  let output = JSON.stringify(renderer!.toJSON());
  expect(output).toContain("Contract v");
  expect(output).toContain("runs unavailable");
  expect(output).toContain("Alice");
  expect(output).toContain("Child");
  expect(output).toContain("Bob");
  expect(getDetail.mock.calls.every(([input]) => input.issueId === parent.issue_id)).toBe(true);
  await act(async () => { renderer!.root.findAll((node) => node.props.accessibilityState?.expanded === false)[0]!.props.onPress(); });
  expect(getDetail.mock.calls.some(([input]) => input.issueId === childId)).toBe(true);
  await act(async () => { renderer!.unmount(); });
  getDetail.mockClear();
  await mount();
  output = JSON.stringify(renderer!.toJSON());
  expect(output).toContain("Child");
  expect(getDetail.mock.calls.some(([input]) => input.issueId === childId)).toBe(true);
  await act(async () => { renderer!.unmount(); });
});

it("labels stale contracts without misidentifying the pending actor", async () => {
  getTasks.mockResolvedValueOnce(toTasksView(makeTasksWire({ accepting: [], returning: [] }), "2026-11-11T12:00:00Z"));
  getExpansion.mockResolvedValueOnce([]);
  getDetail.mockImplementation(async ({ section }) => ({ ok: true, data: section === "contract" ? { stale: true, contract: { versions: [{ number: 1, criteria: ["a"] }], deliveries: [] }, accepted: [], pending_actor: { type: "agent", id: "a" }, acceptor: { type: "agent", id: "a" } } : ({ runs: [], comments: [], prs: { pull_requests: [] }, children: { issues: [] } } as Record<string, unknown>)[section] }));
  const colors = Object.fromEntries(["surface0", "surface1", "surface2", "border", "foreground", "foregroundMuted", "accent", "accentForeground", "statusSuccess", "statusWarning", "statusDanger"].map((key) => [key, key]));
  let renderer: ReactTestRenderer;
  await act(async () => { renderer = create(<SessionTasksSection agentId="agent" theme={{ colors } as PluginAgentPanelProps["theme"]} />); });
  await act(async () => { renderer!.root.findAll((node) => node.props.accessibilityState?.expanded === false)[0]!.props.onPress(); });
  const output = JSON.stringify(renderer!.toJSON());
  expect(output).toContain("Contract stale");
  expect(output).not.toContain("Pending acceptor");
  await act(async () => { renderer!.unmount(); });
});

import type { PluginClientContext, PluginWorkspacePanelProps } from "@getpaseo/plugin/client";
import { useEffect, useId, useRef } from "react";
import { Pressable, Text, View } from "react-native";
import { MulticaSurface } from "./surface";

export const TASK_PANEL_ID = "task-detail";
const MAX_HISTORY = 10;
const ISSUE_PATH = /^\/[A-Za-z0-9][A-Za-z0-9_-]*\/issues\/[A-Za-z0-9][A-Za-z0-9_-]*$/;

type TaskState = { paths: string[] };
export function parseTaskState(value: unknown): TaskState {
  if (!value || typeof value !== "object" || !Array.isArray((value as TaskState).paths)) return { paths: [] };
  const paths: unknown[] = (value as TaskState).paths;
  if (paths.length > MAX_HISTORY || paths.some((path) => typeof path !== "string" || !ISSUE_PATH.test(path))) return { paths: [] };
  return { paths: paths as string[] };
}
export function issuePath(url: string): string | null {
  try {
    const parsed = new URL(url);
    if (!/^https?:$/.test(parsed.protocol) || parsed.search || parsed.hash || !ISSUE_PATH.test(parsed.pathname)) return null;
    return parsed.pathname;
  } catch { return null; }
}
export function visitTask(state: TaskState, path: string): TaskState {
  if (!ISSUE_PATH.test(path) || state.paths.at(-1) === path) return state;
  return { paths: [...state.paths, path].slice(-MAX_HISTORY) };
}

export function createTaskPanels(client: PluginClientContext) {
  const pending = new Map<string, string[]>();
  const listeners = new Map<string, Set<(path: string) => void>>();
  const Component = (props: PluginWorkspacePanelProps) => {
    const instanceId = useId();
    const state = parseTaskState(props.tab.state);
    const current = useRef(state);
    current.current = state;
    const setState = props.tab.setState;
    useEffect(() => {
      const receive = (path: string) => {
        const next = visitTask(current.current, path);
        current.current = next;
        setState(next);
      };
      const group = listeners.get(props.workspaceId) ?? new Set();
      group.add(receive);
      listeners.set(props.workspaceId, group);
      const queued = pending.get(props.workspaceId) ?? [];
      pending.delete(props.workspaceId);
      for (const path of queued) receive(path);
      return () => { group.delete(receive); if (group.size === 0) listeners.delete(props.workspaceId); };
    }, [props.workspaceId, setState]);
    const path = state.paths.at(-1);
    return <View style={{ flex: 1, backgroundColor: props.theme.colors.surface0 }}>
      <View style={{ flexDirection: "row", alignItems: "center", gap: 12, padding: 10 }}>
        {state.paths.length > 1 ? <Pressable accessibilityRole="button" onPress={() => { const next = { paths: current.current.paths.slice(0, -1) }; current.current = next; setState(next); }}><Text style={{ color: props.theme.colors.accent }}>Back</Text></Pressable> : null}
        <Text style={{ color: props.theme.colors.foreground, fontWeight: "600" }}>{path?.split("/").at(-1) ?? "Task details"}</Text>
      </View>
      {path ? <MulticaSurface key={path} {...props} path={path} pageKey={instanceId} /> : <Text style={{ color: props.theme.colors.foregroundMuted, padding: 10 }}>Select a task to open its details.</Text>}
    </View>;
  };
  const remove = client.addWorkspacePanel({ id: TASK_PANEL_ID, title: "Task details", icon: "ListTodo", context: "workspace", locations: ["explorer"], Component });
  return {
    open(workspaceId: string, url: string) {
      const path = issuePath(url);
      if (!path) return;
      client.openPanel(TASK_PANEL_ID, { workspaceId, location: "explorer" });
      const group = listeners.get(workspaceId);
      if (group?.size) for (const receive of group) receive(path);
      else pending.set(workspaceId, [...(pending.get(workspaceId) ?? []), path].slice(-MAX_HISTORY));
    },
    dispose() { remove(); listeners.clear(); pending.clear(); },
  };
}

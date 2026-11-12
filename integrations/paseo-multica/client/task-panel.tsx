import type { PluginClientContext } from "@getpaseo/plugin/client";
import { MulticaSurface } from "./surface";

export function issuePath(url: string): string | null {
  try {
    const parsed = new URL(url);
    if (!/^https?:$/.test(parsed.protocol) || parsed.search || parsed.hash || !/^\/[A-Za-z0-9][A-Za-z0-9_-]*\/issues\/[A-Za-z0-9][A-Za-z0-9_-]*$/.test(parsed.pathname)) return null;
    return parsed.pathname;
  } catch { return null; }
}

export function createTaskPanels(client: PluginClientContext) {
  const panels = new Map<string, () => void>();
  return {
    open(workspaceId: string, url: string, identifier?: string) {
      const path = issuePath(url);
      if (!path) return;
      const id = `issue-${Array.from(path, (character) => character.charCodeAt(0).toString(16).padStart(2, "0")).join("")}`;
      if (!panels.has(id)) {
        const Component = (props: Parameters<typeof MulticaSurface>[0]) => <MulticaSurface {...props} path={path} />;
        panels.set(id, client.addWorkspacePanel({ id, title: identifier || path.split("/").at(-1)!, icon: "ListTodo", context: "workspace", locations: ["explorer"], Component }));
      }
      client.openPanel(id, { workspaceId, location: "explorer" });
    },
    dispose() { for (const remove of panels.values()) remove(); panels.clear(); },
  };
}

import React, { useState } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { StatusPanel } from "../../client/status-panel";
import { createTaskPanels } from "../../client/task-panel";
import type { PluginClientContext, PluginAgentPanelProps } from "@getpaseo/plugin/client";
let Detail: React.ComponentType<PluginAgentPanelProps>;
const panels = createTaskPanels({ addWorkspacePanel: ({ Component }: any) => { Detail = Component; return () => {}; }, openPanel: () => {} } as unknown as PluginClientContext);
const themes = {
  light: { colorScheme: "light", colors: { surface0: "#fff", surface1: "#f5f6f8", surface2: "#eaecf0", border: "#dcdfe4", foreground: "#17202a", foregroundMuted: "#5b6573", accent: "#2659cc", accentForeground: "#fff", statusSuccess: "#1f8a4c", statusWarning: "#b26a00", statusDanger: "#c62828" } },
  dark: { colorScheme: "dark", colors: { surface0: "#18181b", surface1: "#232327", surface2: "#2f2f35", border: "#3a3a41", foreground: "#ececf1", foregroundMuted: "#9a9aa5", accent: "#7aa2ff", accentForeground: "#0b1020", statusSuccess: "#3fb950", statusWarning: "#e3a008", statusDanger: "#f0605d" } },
};
function App() {
  const [agentId, select] = useState("session-a");
  const [states, setStates] = useState<Record<string, any>>({});
  const [scheme, setScheme] = useState<"light" | "dark">("light");
  const theme = themes[scheme];
  document.body.style.background = theme.colors.surface0;
  document.body.style.color = theme.colors.foreground;
  const navigation = { openAgent: ({ serverId, agentId: target }: { serverId?: string; agentId: string }) => { document.body.dataset.openedAgent = `${serverId}/${target}`; }, openWorkspace: () => {} };
  const props = { context: "agent", workspaceId: "same-workspace", agentId, host: { id: "same-host" }, navigation, theme, layout: { compact: false }, tab: { state: states[agentId], setState: (next: any) => setStates(old => ({ ...old, [agentId]: next })) } } as unknown as PluginAgentPanelProps;
  return <><nav>{["session-a", "session-b", "empty", "disconnected", "mr", "actions", "legacy"].map(id => <button key={id} onClick={() => select(id)}>{id}</button>)}<button onClick={() => setScheme(scheme === "light" ? "dark" : "light")}>theme</button></nav><h1>This session’s tasks · {agentId}</h1><p>Same Paseo host and workspace · shared external Agent</p><main style={{ display: "flex", gap: 24, height: 620 }}><section style={{ width: 430 }}><StatusPanel {...props} openTask={url => panels.open(props, url)} /></section><aside style={{ display: "flex", flex: 1 }}><Detail {...props} /></aside></main></>;
}
createRoot(document.getElementById("root")!).render(<QueryClientProvider client={new QueryClient()}><App /></QueryClientProvider>);

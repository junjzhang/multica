import React, { useState } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { StatusPanel } from "../../client/status-panel";
import { createTaskPanels } from "../../client/task-panel";
import type { PluginClientContext, PluginAgentPanelProps } from "@getpaseo/plugin/client";
let Detail: React.ComponentType<PluginAgentPanelProps>;
const panels = createTaskPanels({ addWorkspacePanel: ({ Component }: any) => { Detail = Component; return () => {}; }, openPanel: () => {} } as unknown as PluginClientContext);
const theme = { colorScheme: "light", colors: { surface0: "#fff", surface1: "#f5f6f8", border: "#ddd", foreground: "#17202a", foregroundMuted: "#5b6573", accent: "#2659cc", statusDanger: "#b22" } };
function App() {
  const [agentId, select] = useState("session-a");
  const [states, setStates] = useState<Record<string, any>>({});
  const props = { context: "agent", workspaceId: "same-workspace", agentId, host: { id: "same-host" }, theme, layout: { compact: false }, tab: { state: states[agentId], setState: (next: any) => setStates(old => ({ ...old, [agentId]: next })) } } as unknown as PluginAgentPanelProps;
  return <><nav>{["session-a", "session-b", "empty", "disconnected"].map(id => <button key={id} onClick={() => select(id)}>{id}</button>)}</nav><h1>This session’s tasks · {agentId}</h1><p>Same Paseo host and workspace · shared external Agent</p><main style={{ display: "flex", gap: 24, height: 620 }}><section style={{ width: 430 }}><StatusPanel {...props} openTask={url => panels.open(props, url)} /></section><aside style={{ display: "flex", flex: 1 }}><Detail {...props} /></aside></main></>;
}
createRoot(document.getElementById("root")!).render(<QueryClientProvider client={new QueryClient()}><App /></QueryClientProvider>);

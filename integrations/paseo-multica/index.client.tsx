import type {
  PluginButtonRegistration,
  PluginClientContext,
  PluginSidebarItemProps,
} from "@getpaseo/plugin/client";
import { SidebarRow } from "@getpaseo/plugin/client/ui";
import { STATUS_PANEL_ID, runMulticaCommand, type SessionContract } from "./shared/commands";
import { connectSession, tasksSummary, type StatusView } from "./shared/contracts";
import { DELIVERY_ITEM_KIND, DELIVERY_ITEM_VERSION, deliveryDataSchema } from "./shared/delivery";
import { taskSummaryLabel } from "./shared/tasks";
import { ConnectionSettingsScreen } from "./client/settings-screen";
import { DeliveryCard } from "./client/delivery-card";
import { deliveryTransformer } from "./client/timeline";
import { StatusPanel } from "./client/status-panel";
import { MulticaSurface } from "./client/surface";
import { WEB_PANEL_ID, WEB_SCREEN_ID } from "./shared/web";
import { createTaskPanels } from "./client/task-panel";

const TASKS_PILL_ID = "multica-tasks";

function MulticaSidebarItem({ currentScreen, openScreen }: PluginSidebarItemProps) {
  return (
    <SidebarRow
      icon="ListTodo"
      active={currentScreen?.screenId === WEB_SCREEN_ID}
      onPress={() => openScreen({ screenId: WEB_SCREEN_ID })}
    />
  );
}

export default function contribute(client: PluginClientContext) {
  const taskPanels = createTaskPanels(client);
  const agentWorkspaces = new Map<string, string>();
  client.addScreen({ id: WEB_SCREEN_ID, title: "Multica", Component: MulticaSurface });
  client.addSidebarHeaderItem({ id: WEB_SCREEN_ID, title: "Multica", Component: MulticaSidebarItem });
  client.addWorkspacePanel({
    id: WEB_PANEL_ID, title: "Multica", icon: "ListTodo", context: "workspace",
    locations: ["workspace", "explorer"], Component: MulticaSurface,
  });
  client.addCommandCenterItem({
    id: "open-multica", title: "Open Multica", icon: "ListTodo",
    keywords: ["multica", "issues", "board", "任务"], context: "global",
    onSelect: ({ openScreen }) => openScreen({ screenId: WEB_SCREEN_ID }),
  });
  client.addSlashCommand({
    name: "multica",
    description: "Connect this Paseo session to Multica",
    argumentHint: "connect | status | disconnect",
    context: "agent",
    async onSubmit(context) {
      await runMulticaCommand(
        {
          async rpc(contract: SessionContract, input: { agentId: string }): Promise<StatusView> {
            return (await context.rpc(contract as typeof connectSession, input)) as StatusView;
          },
          openPanel: (id, options) => context.openPanel(id, options),
        },
        context.agent.id,
        context.args,
      );
    },
  });

  client.addWorkspacePanel({
    id: STATUS_PANEL_ID,
    title: "Multica",
    icon: "Plug",
    context: "agent",
    locations: ["workspace", "explorer"],
    Component: (props) => <StatusPanel {...props} openTask={(url, identifier) => taskPanels.open(props.workspaceId, url, identifier)} />,
  });

  client.addSettingsScreen({
    id: "connection",
    title: "Paseo Multica",
    icon: "Settings",
    Component: ConnectionSettingsScreen,
  });

  // One composer pill per agent on this host. The pill's label is the session's
  // task summary; a session with no usable Multica binding keeps the quiet
  // "Multica" label and the panel explains what is missing. It is registered
  // from the agent directory because a pill belongs to one agent's composer.
  const pills = new Map<string, PluginButtonRegistration>();
  const lifetime = new AbortController();
  let stopped = false;

  const refreshPill = async (agentId: string): Promise<void> => {
    const pill = pills.get(agentId);
    if (!pill || stopped) return;
    try {
      const view = await client.rpc(tasksSummary, { agentId });
      const summary = taskSummaryLabel(view);
      pill.update({ title: summary.title, label: summary.label });
    } catch {
      // The panel owns the explanation; the pill only avoids a stale count.
      pill.update({ title: "Multica tasks unavailable", label: "Multica" });
    }
  };

  const register = (agent: { id: string; workspaceId?: string | null }): void => {
    if (stopped || !agent.workspaceId) return;
    const workspaceId = agent.workspaceId;
    agentWorkspaces.set(agent.id, workspaceId);
    if (pills.has(agent.id)) {
      void refreshPill(agent.id);
      return;
    }
    let pill: PluginButtonRegistration;
    try {
      pill = client.addComposerPill({
        id: TASKS_PILL_ID,
        workspaceId,
        agentId: agent.id,
        button: {
          title: "Multica tasks",
          icon: "ListTodo",
          label: "Multica",
          behavior: {
            kind: "action",
            onPress() {
              client.openPanel(STATUS_PANEL_ID, { workspaceId, agentId: agent.id, location: "explorer" });
              void refreshPill(agent.id);
            },
          },
        },
      });
    } catch {
      // An older host without composer pills keeps every other surface.
      return;
    }
    pills.set(agent.id, pill);
    void refreshPill(agent.id);
  };

  const remove = (agentId: string): void => {
    agentWorkspaces.delete(agentId);
    pills.get(agentId)?.remove();
    pills.delete(agentId);
  };

  void client.paseo.agents
    .list({ subscribe: {}, signal: lifetime.signal })
    .then(({ subscription }) => {
      subscription.subscribe({
        snapshot: ({ entries }) => {
          for (const pill of pills.values()) pill.remove();
          pills.clear();
          agentWorkspaces.clear();
          for (const { agent } of entries) register(agent);
        },
        update: (message) => {
          if (message.type !== "agent_update") return;
          const update = message.payload;
          if (update.kind === "remove") remove(update.agentId);
          else register(update.agent);
        },
      });
      return undefined;
    })
    .catch(() => {
      // The host's agent directory is optional for the rest of the plugin.
    });

  const removeTimeline = [
    client.addTimelineTransformer(deliveryTransformer),
    client.addTimelineRenderer({
      kind: DELIVERY_ITEM_KIND,
      version: DELIVERY_ITEM_VERSION,
      schema: deliveryDataSchema,
      Component: (props) => <DeliveryCard {...props} openTask={(url, identifier) => { const workspaceId = agentWorkspaces.get(props.agentId); if (workspaceId) taskPanels.open(workspaceId, url, identifier); }} />,
    }),
  ];

  return () => {
    stopped = true;
    taskPanels.dispose();
    lifetime.abort();
    for (const pill of pills.values()) pill.remove();
    pills.clear();
    for (const removeContribution of removeTimeline) void removeContribution();
  };
}

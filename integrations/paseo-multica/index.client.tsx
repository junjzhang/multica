import type { PluginClientContext } from "@getpaseo/plugin/client";
import { STATUS_PANEL_ID, runMulticaCommand, type SessionContract } from "./shared/commands";
import { connectSession, type StatusView } from "./shared/contracts";
import { DELIVERY_ITEM_KIND, DELIVERY_ITEM_VERSION, deliveryDataSchema } from "./shared/delivery";
import { ConnectionSettingsScreen } from "./client/settings-screen";
import { DeliveryCard } from "./client/delivery-card";
import { deliveryTransformer } from "./client/timeline";
import { StatusPanel } from "./client/status-panel";

export default function contribute(client: PluginClientContext) {
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
    Component: StatusPanel,
  });

  client.addSettingsScreen({
    id: "connection",
    title: "Paseo Multica",
    icon: "Settings",
    Component: ConnectionSettingsScreen,
  });

  const cleanup = [
    client.addTimelineTransformer(deliveryTransformer),
    client.addTimelineRenderer({
      kind: DELIVERY_ITEM_KIND,
      version: DELIVERY_ITEM_VERSION,
      schema: deliveryDataSchema,
      Component: DeliveryCard,
    }),
  ];

  return () => {
    for (const remove of cleanup) void remove();
  };
}

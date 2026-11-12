import { homedir } from "node:os";
import type { PluginServerContext } from "@getpaseo/plugin/server";
import { connectSession, disconnectSession, statusSession, tasksSummary } from "./shared/contracts";
import { connectionSettings } from "./shared/settings";
import { ConfigurationError } from "./server/owner-profile";
import { paseoHostAdapter } from "./server/host";
import { createSessionService, type ConnectionConfig } from "./server/service";
import { sessionOpenEnv } from "./server/session-open";

export default function contribute(server: PluginServerContext) {
  const settings = server.registerSettings(connectionSettings);

  async function readSettings(): Promise<ConnectionConfig> {
    const state = await settings.read();
    if (state.status !== "ready") {
      throw new ConfigurationError(
        "The Paseo Multica settings on this host are invalid. Open Settings → Plugins → Paseo Multica to repair them.",
      );
    }
    return state.values;
  }

  function service(context: { paseo: Parameters<typeof paseoHostAdapter>[0] }) {
    return createSessionService({
      home: homedir(),
      paseo: paseoHostAdapter(context.paseo),
      readSettings,
    });
  }

  server.handle(connectSession, async ({ agentId }, context) => service(context).connect(agentId));
  server.handle(statusSession, async ({ agentId }, context) => service(context).status(agentId));
  server.handle(disconnectSession, async ({ agentId }, context) => service(context).disconnect(agentId));
  server.handle(tasksSummary, async ({ agentId }, context) => service(context).tasks(agentId));

  // Only the context path, only for an existing binding, and never for history
  // replay. Existing sessions already carry PASEO_AGENT_ID and need no reload.
  server.before("agent.session_open", async ({ request }) => {
    const env = await sessionOpenEnv(homedir(), request);
    return env ? { ...request, env } : undefined;
  });

  return () => {};
}

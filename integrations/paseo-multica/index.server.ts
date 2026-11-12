import { homedir } from "node:os";
import type { PluginServerContext } from "@getpaseo/plugin/server";
import { connectSession, disconnectSession, statusSession, tasksSummary, taskDetail, taskExpansion } from "./shared/contracts";
import { connectionSettings } from "./shared/settings";
import { ConfigurationError, loadOwnerProfile } from "./server/owner-profile";
import { z } from "zod";
import { paseoHostAdapter } from "./server/host";
import { createSessionService, type ConnectionConfig } from "./server/service";
import { sessionOpenEnv } from "./server/session-open";
import { openWeb } from "./shared/web";
import { webSignInLink } from "./server/web";
import { expansion } from "./server/task-expansion";

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

  server.handle(openWeb, async ({ path }) => {
    if (path === undefined) return webSignInLink();
    const owner = await loadOwnerProfile(homedir(), (await readSettings()).ownerProfile);
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 30_000);
    try {
      const response = await fetch(`${owner.serverUrl}/api/workspaces/${encodeURIComponent(owner.workspaceId)}`, {
        headers: { Authorization: `Bearer ${owner.token}`, "X-Workspace-ID": owner.workspaceId },
        signal: controller.signal,
      });
      if (!response.ok) throw new ConfigurationError("Could not resolve the Multica workspace.");
      const workspace = z.object({ slug: z.string() }).parse(await response.json());
      return await webSignInLink(undefined, path, workspace.slug);
    } finally {
      clearTimeout(timer);
    }
  });
  server.handle(connectSession, async ({ agentId }, context) => service(context).connect(agentId));
  server.handle(statusSession, async ({ agentId }, context) => service(context).status(agentId));
  server.handle(disconnectSession, async ({ agentId }, context) => service(context).disconnect(agentId));
  server.handle(tasksSummary, async ({ agentId }, context) => service(context).tasks(agentId));
  server.handle(taskDetail, async ({ agentId, issueId, section }, context) => service(context).taskDetail(agentId, issueId, section));
  server.handle(taskExpansion, async ({ agentId, issueId, expanded }) => expansion(homedir(), agentId, issueId, expanded));

  // Only the context path, only for an existing binding, and never for history
  // replay. Existing sessions already carry PASEO_AGENT_ID and need no reload.
  server.before("agent.session_open", async ({ request }) => {
    const env = await sessionOpenEnv(homedir(), request);
    return env ? { ...request, env } : undefined;
  });

  return () => {};
}

import type { PaseoApi } from "@getpaseo/client";
import { isLocator } from "../shared/session";

export interface HostAgentSnapshot {
  id: string;
  title: string | null;
  archivedAt: string | null;
}

export interface PaseoHostAdapter {
  listHosts(): Promise<{ self: { serverId: string; label: string } }>;
  refreshAgent(agentId: string): Promise<{ agent: HostAgentSnapshot } | null>;
}

export interface HostSession {
  hostId: string;
  hostLabel: string;
  sessionId: string;
  title: string | null;
}

/** Wraps only the two host reads this plugin needs, so tests need no SDK fake. */
export function paseoHostAdapter(paseo: PaseoApi): PaseoHostAdapter {
  return {
    async listHosts() {
      const directory = await paseo.hosts.list();
      return { self: { serverId: directory.self.serverId, label: directory.self.label } };
    },
    async refreshAgent(agentId) {
      const result = await paseo.agents.ref(agentId).refresh();
      if (!result) return null;
      const agent = result.agent;
      return {
        agent: {
          id: agent.id,
          title: agent.title,
          archivedAt: agent.archivedAt ?? null,
        },
      };
    },
  };
}

/**
 * The host id is read from the daemon that actually runs this plugin; the
 * agent id comes from the composer and is verified against that same host.
 * Neither is taken from model text.
 */
export async function resolveHostSession(adapter: PaseoHostAdapter, agentId: string): Promise<HostSession> {
  if (!isLocator(agentId)) {
    throw new Error("The current Paseo session id is invalid.");
  }
  const directory = await adapter.listHosts();
  const hostId = directory.self.serverId.trim();
  if (!isLocator(hostId)) {
    throw new Error("The Paseo host did not report a valid server id.");
  }
  const refreshed = await adapter.refreshAgent(agentId);
  if (!refreshed) {
    throw new Error("This Paseo session no longer exists. Open it again before connecting.");
  }
  if (refreshed.agent.archivedAt) {
    throw new Error("This Paseo session is archived. Restore it before connecting.");
  }
  if (refreshed.agent.id !== agentId) {
    throw new Error("The Paseo host returned a different session id. Refusing to bind this session.");
  }
  return {
    hostId,
    hostLabel: directory.self.label.trim(),
    sessionId: refreshed.agent.id,
    title: refreshed.agent.title,
  };
}

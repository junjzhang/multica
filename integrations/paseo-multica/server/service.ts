import type { StatusView } from "../shared/contracts";
import { describeError } from "../shared/redaction";
import { isExpired, isLocator, returnAddress, type SessionContext } from "../shared/session";
import { createMulticaApi, type ApiCredential, type MulticaApi, type RegisterExternalAgentInput } from "./api";
import { resolveHostSession, type HostSession, type PaseoHostAdapter } from "./host";
import { ConfigurationError, loadOwnerProfile, type OwnerProfile } from "./owner-profile";
import {
  readSessionContext,
  removeSessionContext,
  writeSessionContext,
  type ContextRead,
} from "./store";

export interface ConnectionConfig {
  ownerProfile: string;
  daemonId: string;
}

export interface SessionStore {
  read(home: string, sessionId: string): Promise<ContextRead>;
  write(home: string, context: SessionContext): Promise<void>;
  remove(home: string, sessionId: string): Promise<boolean>;
}

export interface SessionServiceDeps {
  home: string;
  paseo: PaseoHostAdapter;
  api?: MulticaApi;
  store?: SessionStore;
  readSettings(): Promise<ConnectionConfig>;
  now?: () => Date;
  tokenTtlMs?: number;
}

export interface SessionService {
  connect(agentId: string): Promise<StatusView>;
  status(agentId: string): Promise<StatusView>;
  disconnect(agentId: string): Promise<StatusView>;
}

const DEFAULT_TOKEN_TTL_MS = 90 * 24 * 60 * 60 * 1000;
const locks = new Map<string, Promise<unknown>>();

/**
 * Serializes connect/status/disconnect per session. Concurrent explicit
 * connects therefore observe the first one's binding instead of minting a
 * second credential or overwriting it.
 */
function withAgentLock<T>(agentId: string, task: () => Promise<T>): Promise<T> {
  const previous = locks.get(agentId) ?? Promise.resolve();
  const run = previous.catch(() => {}).then(task);
  const tracked = run
    .catch(() => {})
    .finally(() => {
      if (locks.get(agentId) === tracked) locks.delete(agentId);
    });
  locks.set(agentId, tracked);
  return run;
}

export function createSessionService(deps: SessionServiceDeps): SessionService {
  const api = deps.api ?? createMulticaApi();
  const store: SessionStore = deps.store ?? {
    read: readSessionContext,
    write: writeSessionContext,
    remove: removeSessionContext,
  };
  const now = deps.now ?? (() => new Date());
  const tokenTtlMs = deps.tokenTtlMs ?? DEFAULT_TOKEN_TTL_MS;

  async function readConfig(): Promise<Required<ConnectionConfig>> {
    const raw = await deps.readSettings();
    const ownerProfile = raw.ownerProfile.trim();
    const daemonId = raw.daemonId.trim();
    if (daemonId === "") {
      throw new ConfigurationError(
        "Set the authorized Multica delivery daemon in Settings → Plugins → Paseo Multica before connecting.",
      );
    }
    if (!isLocator(daemonId)) {
      throw new ConfigurationError(
        "The configured Multica delivery daemon id is invalid. Use the daemon id shown for this host in Multica.",
      );
    }
    return { ownerProfile, daemonId };
  }

  function ownerCredential(owner: OwnerProfile): ApiCredential {
    return { serverUrl: owner.serverUrl, workspaceId: owner.workspaceId, token: owner.token };
  }

  function conflictFor(
    context: SessionContext,
    host: HostSession,
    owner: OwnerProfile,
    daemonId: string,
  ): string | null {
    if (context.session_id !== host.sessionId) {
      return "The local context belongs to a different Paseo session.";
    }
    if (context.host_id !== host.hostId) {
      return `The local context was issued for Paseo host "${context.host_id}", but this session now runs on "${host.hostId}".`;
    }
    if (context.server_url !== owner.serverUrl) {
      return `The local context belongs to Multica server "${context.server_url}", not the configured "${owner.serverUrl}".`;
    }
    if (context.workspace_id !== owner.workspaceId) {
      return `The local context belongs to Multica workspace "${context.workspace_id}", not the configured "${owner.workspaceId}".`;
    }
    if (context.daemon_id !== daemonId) {
      return `The local context delivers through daemon "${context.daemon_id}", but this plugin is configured for "${daemonId}".`;
    }
    return null;
  }

  function viewFor(context: SessionContext, state: StatusView["state"], reason?: string): StatusView {
    return {
      state,
      ...(reason ? { reason } : {}),
      agentId: context.agent_id,
      workspaceId: context.workspace_id,
      serverUrl: context.server_url,
      appUrl: context.app_url,
      hostId: context.host_id,
      sessionId: context.session_id,
      daemonId: context.daemon_id,
      returnAddress: returnAddress(context),
      expiresAt: context.expires_at,
      checkedAt: now().toISOString(),
    };
  }

  function disconnectedView(reason: string): StatusView {
    return {
      state: "disconnected",
      reason,
      checkedAt: now().toISOString(),
    };
  }

  function invalidView(reason: string): StatusView {
    return { state: "invalid", reason, checkedAt: now().toISOString() };
  }

  async function connect(agentId: string): Promise<StatusView> {
    if (!isLocator(agentId)) throw new ConfigurationError("The current Paseo session id is invalid.");
    return withAgentLock(agentId, async () => {
      const config = await readConfig();
      const host = await resolveHostSession(deps.paseo, agentId);
      const owner = await loadOwnerProfile(deps.home, config.ownerProfile);
      const existing = await store.read(deps.home, agentId);

      if (existing.status === "ok") {
        const conflict = conflictFor(existing.context, host, owner, config.daemonId);
        if (conflict) {
          throw new ConfigurationError(
            `${conflict} Refusing to rebind this session. Restore the owner profile that issued the current binding and run /multica disconnect to revoke it, then connect again.`,
          );
        }
        if (!isExpired(existing.context, now())) {
          const validity = await api.verifyExternalAgentToken(
            {
              serverUrl: existing.context.server_url,
              workspaceId: existing.context.workspace_id,
              token: existing.context.token,
            },
            agentId,
          );
          if (validity === "valid") {
            return viewFor(existing.context, "connected");
          }
        }
      } else if (existing.status === "invalid" && existing.kind !== "malformed") {
        throw new ConfigurationError(
          `${existing.reason} Refusing to overwrite it. Inspect and remove "${
            existing.kind === "foreign-session" ? "the foreign context file" : "the incompatible context file"
          }" manually before connecting.`,
        );
      }

      const registration = await api.registerExternalAgent(ownerCredential(owner), registrationFor(host, config.daemonId));
      if (existing.status === "ok" && existing.context.agent_id !== registration.id) {
        throw new ConfigurationError(
          "Multica returned a different agent identity for this session. Refusing to overwrite the existing binding.",
        );
      }

      const issued = await api.createExternalAgentToken(
        ownerCredential(owner),
        registration.id,
        new Date(now().getTime() + tokenTtlMs).toISOString(),
      );
      const context: SessionContext = {
        version: 1,
        provider: "paseo",
        host_id: host.hostId,
        session_id: host.sessionId,
        agent_id: registration.id,
        daemon_id: config.daemonId,
        server_url: owner.serverUrl,
        app_url: owner.appUrl,
        workspace_id: owner.workspaceId,
        token: issued.token,
        token_id: issued.id,
        expires_at: issued.expiresAt,
      };

      try {
        await store.write(deps.home, context);
      } catch (error) {
        try {
          await api.revokeExternalAgentToken(ownerCredential(owner), registration.id, issued.id);
        } catch (revokeError) {
          throw new ConfigurationError(
            `The Multica session binding could not be saved (${describeError(
              error,
            )}), and the newly issued credential could not be revoked (${describeError(
              revokeError,
            )}). Revoke token ${issued.id} for agent ${registration.id} from the owner profile.`,
          );
        }
        throw new ConfigurationError(
          `The Multica session binding could not be saved (${describeError(
            error,
          )}). The newly issued credential was revoked; run /multica connect again.`,
        );
      }
      return viewFor(context, "connected");
    });
  }

  async function status(agentId: string): Promise<StatusView> {
    if (!isLocator(agentId)) throw new ConfigurationError("The current Paseo session id is invalid.");
    return withAgentLock(agentId, async () => {
      let host: HostSession | null = null;
      let hostError: string | null = null;
      try {
        host = await resolveHostSession(deps.paseo, agentId);
      } catch (error) {
        hostError = describeError(error);
      }

      const existing = await store.read(deps.home, agentId);
      if (existing.status === "missing") {
        return hostError
          ? disconnectedView(hostError)
          : disconnectedView("This Paseo session has no Multica binding. Run /multica connect.");
      }
      if (existing.status === "invalid") {
        return invalidView(hostError ? `${existing.reason} ${hostError}` : existing.reason);
      }
      const context = existing.context;
      if (hostError || !host) {
        return viewFor(context, "invalid", hostError ?? "This Paseo session could not be verified.");
      }
      try {
        const config = await readConfig();
        const owner = await loadOwnerProfile(deps.home, config.ownerProfile);
        const conflict = conflictFor(context, host, owner, config.daemonId);
        if (conflict) return viewFor(context, "invalid", `${conflict} Run /multica connect after fixing the configuration.`);
      } catch (error) {
        return viewFor(context, "invalid", describeError(error));
      }
      if (isExpired(context, now())) {
        return viewFor(
          context,
          "invalid",
          `The Multica credential expired at ${context.expires_at}. Run /multica connect to repair it.`,
        );
      }
      return viewFor(context, "connected");
    });
  }

  async function disconnect(agentId: string): Promise<StatusView> {
    if (!isLocator(agentId)) throw new ConfigurationError("The current Paseo session id is invalid.");
    return withAgentLock(agentId, async () => {
      const existing = await store.read(deps.home, agentId);
      if (existing.status === "missing") {
        return disconnectedView("This Paseo session has no Multica binding.");
      }
      if (existing.status === "invalid") {
        if (existing.kind !== "malformed") {
          throw new ConfigurationError(
            `${existing.reason} Refusing to remove it automatically; inspect the file in the Multica external-session directory first.`,
          );
        }
        await store.remove(deps.home, agentId);
        return disconnectedView(
          "Removed an unreadable local Multica context. No remote credential could be revoked from it.",
        );
      }

      const context = existing.context;
      if (context.session_id !== agentId) {
        throw new ConfigurationError(
          "The local context belongs to a different Paseo session. Refusing to revoke or remove it.",
        );
      }
      const config = await readConfig();
      const owner = await loadOwnerProfile(deps.home, config.ownerProfile);

      if (owner.serverUrl !== context.server_url || owner.workspaceId !== context.workspace_id) {
        throw new ConfigurationError(
          `The configured owner profile cannot revoke this session's Multica credential: agent ${context.agent_id}, token ${context.token_id}, issued by "${context.server_url}" in workspace "${context.workspace_id}". Restore the owner profile that issued it and run /multica disconnect again. The local context was kept.`,
        );
      }

      try {
        await api.revokeExternalAgentToken(ownerCredential(owner), context.agent_id, context.token_id);
      } catch (error) {
        throw new ConfigurationError(
          `Could not revoke this session's Multica credential (${describeError(
            error,
          )}). The local context was kept so /multica disconnect can be retried.`,
        );
      }

      await store.remove(deps.home, agentId);
      return disconnectedView(
        "Revoked this session's Multica credential and removed its local context. The Paseo conversation and history were left intact.",
      );
    });
  }

  return { connect, status, disconnect };
}

function registrationFor(host: HostSession, daemonId: string): RegisterExternalAgentInput {
  const displayName = truncate(host.title?.trim() || host.sessionId, 200);
  const name = truncate(`${host.hostLabel || "Paseo"} · ${displayName}`, 120);
  return {
    name,
    hostId: host.hostId,
    sessionId: host.sessionId,
    displayName,
    url: returnAddress({ host_id: host.hostId, session_id: host.sessionId }),
    daemonId,
  };
}

function truncate(value: string, max: number): string {
  const characters = Array.from(value);
  return characters.length <= max ? value : characters.slice(0, max).join("");
}

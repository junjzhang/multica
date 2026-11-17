import type { BoardData, BoardScope } from "../shared/board";
import type { StatusView } from "../shared/contracts";
import { parseForgeUsernames } from "../shared/settings";
import { describeError, SESSION_CONNECT_ERROR_PREFIX } from "../shared/redaction";
import { isExpired, isLocator, returnAddress, type SessionContext } from "../shared/session";
import {
  createMulticaApi,
  MulticaApiError,
  type ApiCredential,
  type MulticaApi,
  type ExternalAgentRegistration,
  type RegisterExternalAgentInput,
} from "./api";
import { loadBoard } from "./board";
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
  forgeUsernames?: string;
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
  board(agentId: string, scope: BoardScope, refresh?: boolean): Promise<BoardData>;
}

const DEFAULT_TOKEN_TTL_MS = 90 * 24 * 60 * 60 * 1000;
const CONNECT_HINT = "Run multica session connect (or /multica connect in the composer)";
const locks = new Map<string, Promise<unknown>>();
/** The pill and the panel read the same board; a short cache keeps them from doubling the fan-out. */
const BOARD_CACHE_MS = 15_000;
const boards = new Map<string, { at: number; value: Promise<BoardData> }>();

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

  async function readConfig(): Promise<Pick<ConnectionConfig, "ownerProfile" | "daemonId">> {
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

  function contextCredential(context: SessionContext): ApiCredential {
    return {
      serverUrl: context.server_url,
      workspaceId: context.workspace_id,
      token: context.token,
    };
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
            `${conflict} Refusing to rebind this session. Restore the owner profile that issued the current binding and run /multica disconnect to revoke it. ${CONNECT_HINT}.`,
          );
        }
        if (!isExpired(existing.context, now())) {
          const validity = await api.verifyExternalAgentToken(
            contextCredential(existing.context),
            existing.context.agent_id,
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

      const registration = await registerSession(api, ownerCredential(owner), host, config.daemonId);
      if (existing.status === "ok" && existing.context.agent_id !== registration.id) {
        throw new ConfigurationError(
          "Multica returned a different agent identity for this session. Refusing to overwrite the existing binding.",
        );
      }

      const issued = await api.createExternalAgentToken(
        ownerCredential(owner),
        registration.id,
        registration.sessionId,
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
          )}). The newly issued credential was revoked; ${CONNECT_HINT}.`,
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
          : disconnectedView(`This Paseo session has no Multica binding. ${CONNECT_HINT}.`);
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
        if (conflict) return viewFor(context, "invalid", `${conflict} ${CONNECT_HINT} after fixing the configuration.`);
      } catch (error) {
        return viewFor(context, "invalid", describeError(error));
      }
      if (isExpired(context, now())) {
        return viewFor(
          context,
          "invalid",
          `The Multica credential expired at ${context.expires_at}. ${CONNECT_HINT} to repair it.`,
        );
      }
      try {
        const validity = await api.verifyExternalAgentToken(contextCredential(context), context.agent_id);
        if (validity === "invalid") {
          return viewFor(
            context,
            "invalid",
            `Multica no longer accepts this session's credential (it was revoked, expired, or its external agent is gone). ${CONNECT_HINT} to repair it.`,
          );
        }
      } catch (error) {
        return viewFor(
          context,
          "unverified",
          `Could not verify this session's Multica credential: ${describeError(
            error,
          )}. The local binding was left unchanged; run /multica status to retry.`,
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

  /**
   * Reads the session's board. A missing or unusable binding degrades to an
   * explanatory view instead of an RPC error: the pill and panel are a
   * convenience surface and must never break the composer.
   */
  async function readBoard(agentId: string, scope: BoardScope): Promise<BoardData> {
    const existing = await store.read(deps.home, agentId);
    const checkedAt = now().toISOString();
    if (existing.status === "missing") {
      return { state: "disconnected", reason: `This Paseo session has no Multica binding. ${CONNECT_HINT}.`, checkedAt };
    }
    if (existing.status === "invalid") return { state: "disconnected", reason: existing.reason, checkedAt };
    const context = existing.context;
    if (isExpired(context, now())) {
      return { state: "unavailable", reason: `The Multica credential expired at ${context.expires_at}. ${CONNECT_HINT} to repair it.`, checkedAt };
    }
    const credential = contextCredential(context);
    let sessionTasks: Awaited<ReturnType<MulticaApi["listSessionTasks"]>>;
    try {
      sessionTasks = await api.listSessionTasks(credential, context.agent_id);
    } catch (error) {
      return { state: "unavailable", reason: unavailableReason(error), checkedAt };
    }
    try {
      const settings = await deps.readSettings();
      return await loadBoard(api, {
        scope,
        credential,
        appUrl: context.app_url,
        agentId: context.agent_id,
        sessionTasks,
        forgeUsernames: parseForgeUsernames(settings.forgeUsernames ?? ""),
        checkedAt,
      });
    } catch (error) {
      return { state: "unavailable", reason: `Could not read this session's Multica tasks: ${describeError(error)}.`, checkedAt };
    }
  }

  return {
    async connect(agentId) {
      try {
        return await connect(agentId);
      } catch (error) {
        // The CLI may expose only marked, redacted plugin-authored diagnostics.
        throw new ConfigurationError(`${SESSION_CONNECT_ERROR_PREFIX}${describeError(error)}`);
      }
    },
    status,
    disconnect,
    async board(agentId, scope, refresh = false) {
      if (!isLocator(agentId)) throw new ConfigurationError("The current Paseo session id is invalid.");
      const key = JSON.stringify([deps.home, agentId, scope]);
      const cached = boards.get(key);
      if (!refresh && cached && now().getTime() - cached.at < BOARD_CACHE_MS) return cached.value;
      const value = readBoard(agentId, scope);
      boards.set(key, { at: now().getTime(), value });
      return value;
    },
  };
}

function unavailableReason(error: unknown): string {
  if (error instanceof MulticaApiError) {
    if (error.status === 403) {
      return "This Multica agent has no registered session, so it has no session task summary. Reconnect this session to issue a session credential.";
    }
    if (error.status === 404) return "This Multica server does not expose session task summaries yet. Update the server to use the task panel.";
    if (error.status === 401) return `Multica no longer accepts this session's credential. ${CONNECT_HINT} to repair it.`;
  }
  return `Could not read this session's Multica tasks: ${describeError(error)}.`;
}

function registrationFor(host: HostSession, daemonId: string): RegisterExternalAgentInput {
  const displayName = truncate(host.title?.trim() || host.sessionId, 200);
  // name stays the per-session name: a legacy server (or a server with
  // template writes still closed) ignores template/template_name and would
  // otherwise collide on the second session of one host.
  const name = truncate(`${host.hostLabel || "Paseo"} · ${displayName}`, 120);
  const templateName = truncate(`Paseo · ${host.hostLabel || host.hostId}`, 120);
  return {
    name,
    templateName,
    hostId: host.hostId,
    sessionId: host.sessionId,
    displayName,
    url: returnAddress({ host_id: host.hostId, session_id: host.sessionId }),
    daemonId,
    template: true,
  };
}

/**
 * Registers the session under its host template, falling back to the legacy
 * one-agent-per-session registration while the server has not opened template
 * writes. Only the explicit "not enabled" rejection triggers the fallback: any
 * other failure is reported instead of being retried against a different
 * identity shape.
 */
async function registerSession(
  api: MulticaApi,
  credential: ApiCredential,
  host: HostSession,
  daemonId: string,
): Promise<ExternalAgentRegistration> {
  const input = registrationFor(host, daemonId);
  try {
    return await api.registerExternalAgent(credential, input);
  } catch (error) {
    // The server tags this rejection with a stable code; prose is not a
    // contract. Only this documented rollout gate falls back to legacy.
    if (error instanceof MulticaApiError && error.code === "external_agent_templates_disabled") {
      return api.registerExternalAgent(credential, { ...input, template: false, templateName: undefined });
    }
    throw error;
  }
}

function truncate(value: string, max: number): string {
  const characters = Array.from(value);
  return characters.length <= max ? value : characters.slice(0, max).join("");
}

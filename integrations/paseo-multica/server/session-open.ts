import type { PluginSessionOpenRequest } from "@getpaseo/plugin/server";
import { contextPath, readSessionContext } from "./store";

/**
 * Only the context path is injected, only on an interactive opening of a
 * session that already has a binding, and never for history replay. The path
 * contains no credential, and an existing explicit override wins.
 */
export async function sessionOpenEnv(
  home: string,
  request: PluginSessionOpenRequest,
): Promise<Record<string, string> | undefined> {
  if (request.purpose === "history") return undefined;
  if (request.env.MULTICA_EXTERNAL_SESSION_FILE) return undefined;
  const existing = await readSessionContext(home, request.agentId);
  if (existing.status !== "ok") return undefined;
  return {
    ...request.env,
    MULTICA_EXTERNAL_SESSION_FILE: contextPath(home, request.agentId),
  };
}

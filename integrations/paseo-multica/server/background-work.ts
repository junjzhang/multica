import { readFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { z } from "zod";
import type { PluginHandlerContext } from "@getpaseo/plugin/server";

// COMPAT(externalBackgroundWork): the installed SDK predates the host extension.
// Remove these structural types when the fork publishes its next SDK release.
export interface BackgroundWorkEntry {
  agentId: string; id: string; kind: string; description: string | null; startedAt: string | null; canStop: boolean;
  external: { status: string; running: boolean; deadline: string | null; finishedAt: string | null; issue: {label: string; url: string} | null };
}
export type BackgroundWorkSnapshot = {status: "ready"; observedAt: string; items: BackgroundWorkEntry[]} | {status: "unavailable"; message: string};
export interface BackgroundWorkSourceRegistration {
  id: string; label: string;
  fetch(context: PluginHandlerContext): Promise<BackgroundWorkSnapshot>;
  stop(entry: BackgroundWorkEntry, context: PluginHandlerContext): Promise<void>;
}
import { ConfigurationError, loadOwnerProfile, ownerProfilePath, type OwnerProfile } from "./owner-profile";
import type { ConnectionSettings } from "../shared/settings";

const address = z.object({ workspace_id: z.string(), agent_id: z.string(), session_id: z.string(), daemon_id: z.string(), task_id: z.string().optional(), issue_id: z.string().optional() });
const waitItem = z.object({
  id: z.string(), address, state: z.string(), description: z.string().optional(),
  started_at: z.string().nullable().optional(), finished_at: z.string().nullable().optional(), deadline: z.string(),
  notify_error: z.string().optional(),
  delivery: z.object({ known: z.boolean(), enqueued: z.boolean(), delivery_status: z.string().optional(), processing_status: z.string().optional() }),
});
const waitList = z.object({ items: z.array(waitItem), observed_at: z.string(), snapshot_version: z.string().min(1), next_cursor: z.string().optional() });
const endpointSchema = z.object({ url: z.string(), secret: z.string().min(1), daemon_id: z.string().min(1) });
const sessionsSchema = z.object({ sessions: z.array(z.object({ id: z.string(), provider: z.string(), host_id: z.string(), session_id: z.string(), daemon_id: z.string() })) });
type Wait = z.infer<typeof waitItem>;
type Endpoint = z.infer<typeof endpointSchema>;

interface Environment {
  home: string;
  readSettings(): Promise<ConnectionSettings>;
  fetchImpl?: typeof fetch;
}

export function backgroundWorkSource(environment: Environment): BackgroundWorkSourceRegistration {
  const request = environment.fetchImpl ?? fetch;
  const handledWaits = new Set<string>();
  async function json(url: string, token: string, workspace: string, method = "GET") {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 10_000);
    try {
      const response = await request(url, { method, redirect: "error", signal: controller.signal, headers: { Authorization: `Bearer ${token}`, "X-Workspace-ID": workspace } });
      if (!response.ok) {
        if (response.status === 404) throw new ConfigurationError("Update Multica to a version supporting wait list and registered session reads.");
        throw new ConfigurationError(`Multica background inventory request failed (${response.status}).`);
      }
      return await response.json();
    } catch (error) {
      if (error instanceof ConfigurationError || error instanceof SyntaxError) throw error;
      throw new ConfigurationError("Multica is offline. The last background snapshot is retained.");
    } finally {
      clearTimeout(timer);
    }
  }

  async function config() {
    const settings = await environment.readSettings();
    if (!settings.daemonProfile) throw new ConfigurationError('Set the local wait daemon profile in Settings → Plugins → Paseo Multica. Enter "default" for the default profile.');
    if (!settings.daemonId) throw new ConfigurationError("Set the delivery daemon id in Paseo Multica settings.");
    const owner = await loadOwnerProfile(environment.home, settings.ownerProfile);
    const profile = settings.daemonProfile === "default" ? "" : settings.daemonProfile;
    const directory = dirname(ownerProfilePath(environment.home, profile));
    let endpoint: Endpoint;
    try {
      endpoint = endpointSchema.parse(JSON.parse(await readFile(join(directory, "wait.endpoint.json"), "utf8")));
    } catch {
      throw new ConfigurationError("The selected local Multica daemon profile has no valid wait endpoint. Start that daemon or select its profile.");
    }
    const url = new URL(endpoint.url);
    if (url.protocol !== "http:" || url.hostname !== "127.0.0.1" || url.username || url.password || url.pathname !== "/" || url.search || url.hash)
      throw new ConfigurationError("The local wait endpoint must be a loopback HTTP origin.");
    if (endpoint.daemon_id !== settings.daemonId) throw new ConfigurationError("The selected local profile belongs to a different delivery daemon.");
    return { owner, endpoint: { ...endpoint, url: url.origin } };
  }
  async function inventory(owner: OwnerProfile, endpoint: Endpoint) {
    const items: Wait[] = [];
    const cursors = new Set<string>();
    let cursor = "";
    let version: string | null = null;
    let observedAt = "";
    do {
      const query = new URLSearchParams({ workspace: owner.workspaceId, limit: "200", cursor });
      const page = waitList.parse(await json(`${endpoint.url}/waits?${query}`, endpoint.secret, owner.workspaceId));
      if (version !== null && version !== page.snapshot_version) throw new ConfigurationError("Multica wait inventory changed while paging; retrying.");
      version = page.snapshot_version;
      observedAt = page.observed_at;
      items.push(...page.items);
      cursor = page.next_cursor ?? "";
      if (cursor && cursors.has(cursor)) throw new ConfigurationError("Multica returned a repeated wait cursor.");
      cursors.add(cursor);
    } while (cursor);
    return { items, observedAt };
  }
  async function project(owner: OwnerProfile, endpoint: Endpoint, hostId: string) {
    const snapshot = await inventory(owner, endpoint);
    const relevant = snapshot.items.filter((item) => !item.address.task_id && item.address.workspace_id === owner.workspaceId && item.address.daemon_id === endpoint.daemon_id);
    const identities = [...new Set(relevant.map((item) => item.address.agent_id))];
    const mappings = new Map<string, z.infer<typeof sessionsSchema>["sessions"]>();
    for (const id of identities) {
      const response = sessionsSchema.parse(await json(`${owner.serverUrl}/api/agents/${encodeURIComponent(id)}/sessions`, owner.token, owner.workspaceId));
      mappings.set(id, response.sessions);
    }
    const workspace = z.object({slug: z.string()}).parse(await json(`${owner.serverUrl}/api/workspaces/${encodeURIComponent(owner.workspaceId)}`, owner.token, owner.workspaceId));
    const items: BackgroundWorkEntry[] = [];
    for (const wait of relevant) {
      const session = mappings.get(wait.address.agent_id)?.find((session) => session.id === wait.address.session_id && session.provider === "paseo" && session.host_id === hostId && session.daemon_id === wait.address.daemon_id);
      const identity = JSON.stringify([wait.address.daemon_id, wait.address.workspace_id, wait.address.session_id, wait.id]);
      if (wait.delivery.known && wait.delivery.processing_status === "handled") handledWaits.add(identity);
      if (!session || handledWaits.has(identity)) continue;
      const running = wait.state === "running" || wait.state === "cancelling";
      items.push({
        agentId: session.session_id, id: wait.id, kind: "monitor", description: wait.description ?? "Multica wait", startedAt: wait.started_at ?? null,
        canStop: wait.state === "running",
        external: {
          status: statusText(wait), running, deadline: wait.deadline, finishedAt: wait.finished_at ?? null,
          issue: wait.address.issue_id ? { label: "Issue", url: `${owner.appUrl}/${encodeURIComponent(workspace.slug)}/issues/${encodeURIComponent(wait.address.issue_id)}` } : null,
        },
      });
    }
    return { status: "ready" as const, observedAt: snapshot.observedAt, items };
  }
  return {
    id: "waits", label: "Multica",
    async fetch({ paseo }): Promise<BackgroundWorkSnapshot> {
      try {
        const { owner, endpoint } = await config();
        const hosts = await paseo.hosts.list();
        return await project(owner, endpoint, hosts.self.serverId);
      } catch (error) {
        const message = error instanceof ConfigurationError ? error.message : "Multica returned an invalid background inventory. Update Multica and retry.";
        return { status: "unavailable", message };
      }
    },
    async stop(entry, { paseo }) {
      const { owner, endpoint } = await config();
      const hosts = await paseo.hosts.list();
      const current = await project(owner, endpoint, hosts.self.serverId);
      const target = current.items.find((item) => item.id === entry.id && item.agentId === entry.agentId);
      if (!target) throw new ConfigurationError("This wait no longer belongs to this Paseo session.");
      if (!target.external.running) return;
      await json(`${endpoint.url}/waits/${encodeURIComponent(target.id)}`, endpoint.secret, owner.workspaceId, "DELETE");
    },
  };
}

function statusText(wait: Wait): string {
  if (wait.state === "running") return "Running";
  if (wait.state === "cancelling") return "Cancelling local wait";
  const execution: Record<string, string> = { succeeded: "Succeeded", failed: "Failed", timed_out: "Timed out", cancelled: "Cancelled", lost: "Lost after restart" };
  const state = execution[wait.state] ?? wait.state;
  if (!wait.delivery.known) return `${state} · delivery status unknown`;
  if (!wait.delivery.enqueued) return `${state} · awaiting mailbox${wait.notify_error ? " (notification failed)" : ""}`;
  if (wait.delivery.delivery_status === "delivered") return `${state} · delivered · ${wait.delivery.processing_status ?? "unread"}`;
  return `${state} · delivery ${wait.delivery.delivery_status ?? "pending"} · ${wait.delivery.processing_status ?? "unread"}`;
}

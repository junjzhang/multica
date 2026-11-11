import { z } from "zod";

/**
 * Composer pill and panel data for "this session's tasks". The wire shape is
 * the server's read projection; the view shape is what the plugin renders.
 */
export const TASK_GROUPS = ["owned", "accepting", "returning"] as const;
export type TaskGroup = (typeof TASK_GROUPS)[number];

export const TASK_ROLES = ["executor", "leader", "acceptor", "return"] as const;
export type TaskRole = (typeof TASK_ROLES)[number];

const taskItemSchema = z.object({
  issueId: z.string().min(1),
  identifier: z.string().min(1).optional(),
  title: z.string(),
  status: z.string().min(1),
  statusName: z.string().min(1).optional(),
  statusCategory: z.string(),
  url: z.string().url().optional(),
  role: z.enum(TASK_ROLES),
  updatedAt: z.string().optional(),
  acceptedAt: z.string().optional(),
  taskStatus: z.string().optional(),
});

export type SessionTaskItem = z.infer<typeof taskItemSchema>;

const readySchema = z.object({
  state: z.literal("ready"),
  agentId: z.string().min(1),
  sessionId: z.string().min(1).optional(),
  workspaceId: z.string().min(1),
  owned: z.array(taskItemSchema),
  accepting: z.array(taskItemSchema),
  returning: z.array(taskItemSchema),
  truncated: z.boolean(),
  checkedAt: z.string(),
});

/** The session has no usable Multica binding; the panel explains how to fix it. */
const disconnectedSchema = z.object({
  state: z.literal("disconnected"),
  agentId: z.string().min(1),
  reason: z.string().min(1),
  checkedAt: z.string(),
});

/** A binding exists but this server or credential cannot answer the read. */
const unavailableSchema = z.object({
  state: z.literal("unavailable"),
  agentId: z.string().min(1),
  reason: z.string().min(1),
  checkedAt: z.string(),
});

export const sessionTasksViewSchema = z.discriminatedUnion("state", [
  readySchema,
  disconnectedSchema,
  unavailableSchema,
]);

export type SessionTasksView = z.infer<typeof sessionTasksViewSchema>;
export type ReadyTasksView = z.infer<typeof readySchema>;

const wireItemSchema = z.object({
  issue_id: z.string().min(1),
  identifier: z.string().min(1).optional(),
  title: z.string(),
  status: z.string().min(1),
  status_name: z.string().min(1).optional(),
  status_category: z.string(),
  url: z.string().optional(),
  role: z.enum(TASK_ROLES),
  updated_at: z.string().optional(),
  accepted_at: z.string().optional(),
  task_status: z.string().optional(),
});

/**
 * The server read projection. Parsed before use so an older server, a
 * partially deployed schema or a proxy error page cannot reach the panel as
 * trusted data.
 */
export const sessionTasksWireSchema = z.object({
  agent_id: z.string().min(1),
  session_id: z.string().min(1).optional(),
  workspace_id: z.string().min(1),
  owned: z.array(wireItemSchema),
  accepting: z.array(wireItemSchema),
  returning: z.array(wireItemSchema),
  truncated: z.boolean().optional(),
});

export type SessionTasksWire = z.infer<typeof sessionTasksWireSchema>;

function httpURL(value: string | undefined): string | undefined {
  if (!value) return undefined;
  try {
    const url = new URL(value);
    return url.protocol === "https:" || url.protocol === "http:" ? url.toString() : undefined;
  } catch {
    return undefined;
  }
}

function toItem(item: z.infer<typeof wireItemSchema>): SessionTaskItem {
  return {
    issueId: item.issue_id,
    ...(item.identifier ? { identifier: item.identifier } : {}),
    title: item.title,
    status: item.status,
    ...(item.status_name ? { statusName: item.status_name } : {}),
    statusCategory: item.status_category,
    ...(httpURL(item.url) ? { url: httpURL(item.url) } : {}),
    role: item.role,
    ...(item.updated_at ? { updatedAt: item.updated_at } : {}),
    ...(item.accepted_at ? { acceptedAt: item.accepted_at } : {}),
    ...(item.task_status ? { taskStatus: item.task_status } : {}),
  };
}

/** Converts a parsed server payload into the view the pill and panel render. */
export function toTasksView(wire: SessionTasksWire, checkedAt: string): ReadyTasksView {
  return {
    state: "ready",
    agentId: wire.agent_id,
    ...(wire.session_id ? { sessionId: wire.session_id } : {}),
    workspaceId: wire.workspace_id,
    owned: wire.owned.map(toItem),
    accepting: wire.accepting.map(toItem),
    returning: wire.returning.map(toItem),
    truncated: wire.truncated ?? false,
    checkedAt,
  };
}

export function taskGroupTitle(group: TaskGroup): string {
  switch (group) {
    case "owned":
      return "Your tasks";
    case "accepting":
      return "Waiting for your acceptance";
    case "returning":
      return "Returns pending";
  }
}

/** A stable, human-readable status label; custom statuses keep their name. */
export function taskStatusLabel(item: SessionTaskItem): string {
  return item.statusName ?? item.status.replace(/_/g, " ");
}

/**
 * The composer pill's text. Every non-ready state is shown as the quiet
 * "Multica" pill with an explanatory tooltip, never as an error count the
 * session cannot act on.
 */
export function taskSummaryLabel(view: SessionTasksView): { label: string; title: string } {
  if (view.state === "disconnected") {
    return { label: "Multica", title: `Multica not connected. ${view.reason}` };
  }
  if (view.state === "unavailable") {
    return { label: "Multica", title: `Multica tasks unavailable. ${view.reason}` };
  }
  const total = view.owned.length + view.accepting.length + view.returning.length;
  if (total === 0) {
    return { label: "Multica", title: "No Multica tasks for this session" };
  }
  const parts: string[] = [];
  if (view.owned.length > 0) parts.push(`${view.owned.length} owned`);
  if (view.accepting.length > 0) parts.push(`${view.accepting.length} to accept`);
  if (view.returning.length > 0) parts.push(`${view.returning.length} to return`);
  return {
    label: `${total} task${total === 1 ? "" : "s"}`,
    title: `Multica tasks: ${parts.join(", ")}`,
  };
}

/** The ready view's counts, in one place so the pill and panel agree. */
export function taskCounts(view: ReadyTasksView): Record<TaskGroup, number> {
  return {
    owned: view.owned.length,
    accepting: view.accepting.length,
    returning: view.returning.length,
  };
}

import { defineRpc } from "@getpaseo/plugin";
import { z } from "zod";
import { pullRequestsSchema } from "./pull-requests";
import { sessionTasksViewSchema } from "./tasks";
import { detailResultSchema, detailSection } from "./task-details";

export const statusViewSchema = z.object({
  state: z.enum(["connected", "invalid", "unverified", "disconnected"]),
  /** Actionable explanation for an invalid, unverified, or disconnected state. */
  reason: z.string().optional(),
  agentId: z.string().optional(),
  workspaceId: z.string().optional(),
  serverUrl: z.string().optional(),
  appUrl: z.string().optional(),
  hostId: z.string().optional(),
  sessionId: z.string().optional(),
  daemonId: z.string().optional(),
  returnAddress: z.string().optional(),
  expiresAt: z.string().optional(),
  checkedAt: z.string(),
});

export type StatusView = z.infer<typeof statusViewSchema>;

const agentInput = z.object({ agentId: z.string().min(1).max(200) });

export const connectSession = defineRpc({
  name: "session.connect",
  input: agentInput,
  output: statusViewSchema,
});

export const statusSession = defineRpc({
  name: "session.status",
  input: agentInput,
  output: statusViewSchema,
});

export const disconnectSession = defineRpc({
  name: "session.disconnect",
  input: agentInput,
  output: statusViewSchema,
});

/**
 * "This session's tasks": a read-only projection for the composer pill and the
 * Multica panel. It never mutates an obligation.
 */
export const tasksSummary = defineRpc({
  name: "session.tasks",
  input: agentInput,
  output: sessionTasksViewSchema,
});

export const taskDetail = defineRpc({
  name: "session.task-detail",
  input: agentInput.extend({ issueId: z.string().uuid(), section: detailSection }),
  output: detailResultSchema,
});

export const taskExpansion = defineRpc({
  name: "session.task-expansion",
  input: agentInput.extend({ issueId: z.string().uuid().optional(), expanded: z.boolean().optional() }),
  output: z.array(z.string().uuid()),
});

export const sessionPullRequests = defineRpc({ name: "session.pull-requests", input: agentInput, output: pullRequestsSchema });

export const backgroundWorkSupport = defineRpc({
  name: "background.support",
  input: z.object({}),
  output: z.object({ supported: z.boolean() }),
});

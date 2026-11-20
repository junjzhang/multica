import { defineRpc } from "@getpaseo/plugin";
import { z } from "zod";
import { BOARD_SCOPES, boardDataSchema } from "./board";
import { taskActionResultSchema, taskActionSchema } from "./task-actions";

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
 * The session's to-do queue and task trees: a read-only projection for the
 * composer pill and the Multica panel. It never mutates an obligation.
 * `refresh` bypasses the short server cache shared by the pill and panel.
 */
export const sessionBoard = defineRpc({
  name: "session.board",
  input: agentInput.extend({ scope: z.enum(BOARD_SCOPES).default("session"), refresh: z.boolean().optional() }),
  output: boardDataSchema,
});

export const taskExpansion = defineRpc({
  name: "session.task-expansion",
  input: agentInput.extend({ issueId: z.string().uuid().optional(), expanded: z.boolean().optional() }),
  output: z.array(z.string().uuid()),
});

/** Reruns a task or moves its status, written with the owner profile; a refusal is a result, not a thrown error. */
export const taskAction = defineRpc({
  name: "session.task-action",
  input: agentInput.extend({ issueId: z.string().uuid(), action: taskActionSchema }),
  output: taskActionResultSchema,
});

export const backgroundWorkSupport = defineRpc({
  name: "background.support",
  input: z.object({}),
  output: z.object({ supported: z.boolean() }),
});

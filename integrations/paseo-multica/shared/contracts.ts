import { defineRpc } from "@getpaseo/plugin";
import { z } from "zod";

export const statusViewSchema = z.object({
  state: z.enum(["connected", "invalid", "disconnected"]),
  /** Actionable explanation for an invalid or disconnected state. */
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

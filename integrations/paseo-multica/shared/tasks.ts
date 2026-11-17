import { z } from "zod";

export const TASK_ROLES = ["executor", "leader", "acceptor", "return", "request"] as const;

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
  executor_name: z.string().optional(),
  activity_at: z.string().optional(),
  delivery_at: z.string().optional(),
  child_count: z.number().int().nonnegative().optional(),
  closed_child_count: z.number().int().nonnegative().optional(),
  pending_acceptance: z.boolean().optional(),
  blocked_reason: z.string().optional(),
  request_id: z.string().optional(),
  request_at: z.string().optional(),
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
  delegated: z.array(wireItemSchema).optional(),
  requests: z.array(wireItemSchema).optional(),
  truncated: z.boolean().optional(),
});

export type SessionTasksWire = z.infer<typeof sessionTasksWireSchema>;

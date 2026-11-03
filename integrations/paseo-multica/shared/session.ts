import { z } from "zod";

/** The private binding file shared with the Multica CLI. Bump only with a reader. */
export const SESSION_CONTEXT_VERSION = 1;
export const PASEO_PROVIDER = "paseo";

/** The same locator shape the Multica server accepts for a host/session tuple. */
const LOCATOR_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._:-]{0,199}$/;
const UUID_PATTERN = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;
const RFC3339_PATTERN = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/;

const locator = z.string().regex(LOCATOR_PATTERN, "must be a host/session locator");
const uuid = z.string().regex(UUID_PATTERN, "must be a UUID");
const rfc3339 = z
  .string()
  .regex(RFC3339_PATTERN, "must be an RFC3339 timestamp")
  .refine((value) => !Number.isNaN(Date.parse(value)), "must be a real timestamp");

/**
 * Frozen v1 private credential cache. The plugin writes it; the CLI reads it.
 * It is never a task/queue authority and never contains owner credentials.
 */
export const sessionContextSchema = z
  .object({
    version: z.literal(SESSION_CONTEXT_VERSION),
    provider: z.literal(PASEO_PROVIDER),
    host_id: locator,
    session_id: locator,
    agent_id: uuid,
    daemon_id: locator,
    server_url: z.string().url(),
    app_url: z.string(),
    workspace_id: uuid,
    token: z.string().min(1),
    token_id: uuid,
    expires_at: rfc3339,
  })
  .strict();

export type SessionContext = z.infer<typeof sessionContextSchema>;

export function isLocator(value: string): boolean {
  return LOCATOR_PATTERN.test(value);
}

export function isUuid(value: string): boolean {
  return UUID_PATTERN.test(value);
}

export function isExpired(context: Pick<SessionContext, "expires_at">, now: Date): boolean {
  return Date.parse(context.expires_at) <= now.getTime();
}

export function returnAddress(context: Pick<SessionContext, "host_id" | "session_id">): string {
  return `paseo://h/${context.host_id}/agent/${context.session_id}`;
}

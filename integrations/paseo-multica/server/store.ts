import { randomUUID } from "node:crypto";
import { chmod, mkdir, open, readFile, rename, rm } from "node:fs/promises";
import { isAbsolute, join, normalize, relative, sep } from "node:path";
import {
  SESSION_CONTEXT_VERSION,
  sessionContextSchema,
  type SessionContext,
} from "../shared/session";

export type ContextInvalidKind = "malformed" | "unsupported" | "foreign-session";

export type ContextRead =
  | { status: "missing" }
  | { status: "invalid"; kind: ContextInvalidKind; reason: string }
  | { status: "ok"; context: SessionContext };

const LOCATOR_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._:-]{0,199}$/;

export function contextDirectory(home: string): string {
  return join(home, ".multica", "external-sessions", "paseo");
}

export function contextParentDirectory(home: string): string {
  return join(home, ".multica", "external-sessions");
}

/**
 * The session id is a canonical Paseo agent id and therefore also a filename.
 * Reject anything that could escape the private directory.
 */
export function contextPath(home: string, sessionId: string): string {
  if (!LOCATOR_PATTERN.test(sessionId)) {
    throw new Error("Session id is not a canonical Paseo agent id.");
  }
  const directory = normalize(contextDirectory(home));
  const path = normalize(join(directory, `${sessionId}.json`));
  const withinDirectory = relative(directory, path);
  if (
    !isAbsolute(home) ||
    withinDirectory === "" ||
    withinDirectory.startsWith(`..${sep}`) ||
    isAbsolute(withinDirectory)
  ) {
    throw new Error("Refusing to resolve a session context outside its private directory.");
  }
  return path;
}

export async function readSessionContext(home: string, sessionId: string): Promise<ContextRead> {
  const path = contextPath(home, sessionId);
  let raw: string;
  try {
    raw = await readFile(path, "utf8");
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return { status: "missing" };
    throw error;
  }

  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return { status: "invalid", kind: "malformed", reason: "The local session context is not valid JSON." };
  }
  const result = sessionContextSchema.safeParse(parsed);
  if (!result.success) {
    const record = parsed as Record<string, unknown> | null;
    if (record && typeof record === "object") {
      if (typeof record.session_id === "string" && record.session_id !== sessionId) {
        return {
          status: "invalid",
          kind: "foreign-session",
          reason: "The local context file belongs to a different Paseo session.",
        };
      }
      if (
        record.version !== SESSION_CONTEXT_VERSION ||
        (record.provider !== undefined && record.provider !== "paseo")
      ) {
        return {
          status: "invalid",
          kind: "unsupported",
          reason: "The local session context uses a version or provider this plugin does not support.",
        };
      }
    }
    return {
      status: "invalid",
      kind: "malformed",
      reason: "The local session context is not a valid v1 binding.",
    };
  }
  if (result.data.session_id !== sessionId) {
    return {
      status: "invalid",
      kind: "foreign-session",
      reason: "The local context file belongs to a different Paseo session.",
    };
  }
  return { status: "ok", context: result.data };
}

/**
 * Replaces the whole file atomically. The temporary file lives beside the
 * target, is created 0600, and is renamed only after its contents are synced,
 * so a reader never sees a partial credential.
 */
export async function writeSessionContext(home: string, context: SessionContext): Promise<void> {
  const validated = sessionContextSchema.parse(context);
  const directory = contextDirectory(home);
  const parent = contextParentDirectory(home);
  const path = contextPath(home, validated.session_id);

  await mkdir(parent, { recursive: true, mode: 0o700 });
  await chmod(parent, 0o700);
  await mkdir(directory, { recursive: true, mode: 0o700 });
  await chmod(directory, 0o700);

  const temporary = join(directory, `.${validated.session_id}.${process.pid}.${randomUUID()}.tmp`);
  let handle: Awaited<ReturnType<typeof open>> | undefined;
  try {
    handle = await open(temporary, "wx", 0o600);
    await handle.writeFile(`${JSON.stringify(validated, null, 2)}\n`, "utf8");
    await handle.sync();
    await handle.close();
    handle = undefined;
    await rename(temporary, path);
    await chmod(path, 0o600);
    await syncDirectory(directory);
  } catch (error) {
    if (handle) await handle.close().catch(() => {});
    await rm(temporary, { force: true }).catch(() => {});
    throw error;
  }
}

export async function removeSessionContext(home: string, sessionId: string): Promise<boolean> {
  try {
    await rm(contextPath(home, sessionId));
    return true;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return false;
    throw error;
  }
}

async function syncDirectory(directory: string): Promise<void> {
  let handle: Awaited<ReturnType<typeof open>> | undefined;
  try {
    handle = await open(directory, "r");
    await handle.sync();
  } catch {
    // Directory fsync is not supported everywhere; the rename is still atomic.
  } finally {
    await handle?.close().catch(() => {});
  }
}

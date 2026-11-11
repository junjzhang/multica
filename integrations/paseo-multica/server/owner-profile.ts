import { readFile } from "node:fs/promises";
import { join } from "node:path";
import { isUuid } from "../shared/session";

export class ConfigurationError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ConfigurationError";
  }
}

export interface OwnerProfile {
  profile: string;
  serverUrl: string;
  appUrl: string;
  workspaceId: string;
  /** Human credential. Never surfaced in RPC output, errors, logs, or provider env. */
  token: string;
  configPath: string;
}

const PROFILE_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;

export function ownerProfilePath(home: string, profile: string): string {
  if (profile === "") return join(home, ".multica", "config.json");
  if (!PROFILE_PATTERN.test(profile)) {
    throw new ConfigurationError(
      "The owner profile name is invalid. Use a plain Multica profile name without path separators.",
    );
  }
  return join(home, ".multica", "profiles", profile, "config.json");
}

interface StoredProfile {
  token?: unknown;
  server_url?: unknown;
  app_url?: unknown;
  workspace_id?: unknown;
}

/**
 * Reads the operator's Multica CLI profile. This runs only in the trusted
 * daemon-side backend. The token never leaves this process: callers pass it
 * straight to the Multica API helpers.
 */
export async function loadOwnerProfile(home: string, profile: string): Promise<OwnerProfile> {
  const configPath = ownerProfilePath(home, profile);
  const displayName = profile === "" ? "default" : profile;
  let raw: string;
  try {
    raw = await readFile(configPath, "utf8");
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      throw new ConfigurationError(
        `Multica owner profile "${displayName}" was not found on this host. Sign in with the Multica CLI for that profile.`,
      );
    }
    throw new ConfigurationError(`Multica owner profile "${displayName}" could not be read.`);
  }

  let stored: StoredProfile;
  try {
    stored = JSON.parse(raw) as StoredProfile;
  } catch {
    throw new ConfigurationError(`Multica owner profile "${displayName}" is not valid JSON.`);
  }

  const token = typeof stored.token === "string" ? stored.token.trim() : "";
  if (token === "") {
    throw new ConfigurationError(
      `Multica owner profile "${displayName}" has no credential. Run \`multica auth login\` for that profile.`,
    );
  }
  const serverUrl = normalizeBaseUrl(stored.server_url, `profile "${displayName}"`);
  const workspaceId = typeof stored.workspace_id === "string" ? stored.workspace_id.trim() : "";
  if (!isUuid(workspaceId)) {
    throw new ConfigurationError(
      `Multica owner profile "${displayName}" has no valid workspace. Select a workspace with the Multica CLI.`,
    );
  }
  const appUrl =
    typeof stored.app_url === "string" && stored.app_url.trim() !== ""
      ? normalizeBaseUrl(stored.app_url, `profile "${displayName}" app URL`)
      : serverUrl;

  return { profile, serverUrl, appUrl, workspaceId, token, configPath };
}

export function normalizeBaseUrl(value: unknown, owner: string): string {
  const raw = typeof value === "string" ? value.trim() : "";
  if (raw === "") {
    throw new ConfigurationError(`The ${owner} has no Multica server URL. Sign in again with the Multica CLI.`);
  }
  let url: URL;
  try {
    url = new URL(raw);
  } catch {
    throw new ConfigurationError(`The ${owner} has an invalid Multica server URL.`);
  }
  if (url.protocol !== "http:" && url.protocol !== "https:") {
    throw new ConfigurationError(`The ${owner} Multica server URL must use http or https.`);
  }
  return raw.replace(/\/+$/, "");
}

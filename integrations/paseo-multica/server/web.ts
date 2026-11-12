import { readFile } from "node:fs/promises";
import { homedir } from "node:os";
import { join } from "node:path";
import { z } from "zod";
import { ConfigurationError } from "./owner-profile";

export const SIGN_IN_PATH = "/paseo-login";

const configSchema = z.object({ url: z.url(), key: z.string(), externalUrl: z.url().optional() }).strict();
const KEY_PATTERN = /^[A-Za-z0-9_-]{32,}$/;

export function webConfigPath(home: string = homedir()): string {
  return join(home, ".config", "multica", "paseo-web.json");
}

/**
 * Returns the embedded page's one-time sign-in link and the address an
 * external browser opens. The reverse proxy exchanges the key for its
 * HTTP-only session cookie and redirects, so the key never stays in the page
 * URL. The key is read on the daemon host, travels only through the
 * authenticated plugin RPC, and never reaches an external browser: that link
 * signs in with the proxy password, and may use another origin when the
 * embedded one is unreachable from a phone.
 */
export async function webSignInLink(
  configPath: string = webConfigPath(),
): Promise<{ url: string; externalUrl: string }> {
  let text: string;
  try {
    text = await readFile(configPath, "utf8");
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      throw new ConfigurationError(
        `The Multica web page is not configured on this host. Create ${configPath} with its url and sign-in key.`,
      );
    }
    throw new ConfigurationError("The Multica web page configuration could not be read.");
  }

  let parsed: z.infer<typeof configSchema>;
  try {
    parsed = configSchema.parse(JSON.parse(text));
  } catch {
    throw new ConfigurationError(
      "The Multica web page configuration must be JSON with a url, a key, and an optional externalUrl.",
    );
  }
  if (!KEY_PATTERN.test(parsed.key)) {
    throw new ConfigurationError("The Multica web sign-in key must be at least 32 URL-safe characters.");
  }

  const url = origin(parsed.url, "url");
  const externalUrl = parsed.externalUrl === undefined ? url : origin(parsed.externalUrl, "externalUrl");
  const link = new URL(SIGN_IN_PATH, url);
  link.searchParams.set("k", parsed.key);
  return { url: link.href, externalUrl: externalUrl.href };
}

function origin(value: string, field: string): URL {
  const url = new URL(value);
  const originOnly =
    (url.protocol === "https:" || url.protocol === "http:") &&
    url.pathname === "/" &&
    !url.search &&
    !url.hash &&
    !url.username &&
    !url.password;
  if (!originOnly) {
    throw new ConfigurationError(
      `The Multica web page ${field} must be an HTTP(S) origin without credentials, path, query, or fragment.`,
    );
  }
  return url;
}

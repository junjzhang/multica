import { shell, type BrowserWindow } from "electron";

// True when the URL parses and uses http/https.
export function isSafeExternalHttpUrl(url: string): boolean {
  return getHttpProtocol(url) !== null;
}

/**
 * The ONLY custom scheme allowed through the opener: an exact Paseo session
 * locator. This is deliberately not a scheme allowlist — each identifier must
 * be one canonical percent-encoded path segment, and credentials, query,
 * fragment and extra path segments are rejected. Mirrors the renderer's typed
 * parser so the typed link and the main-process gate agree.
 */
export function isSafePaseoSessionUrl(url: string): boolean {
  if (url.length === 0 || url.length > 2048) return false;
  if (/\s/.test(url)) return false;
  const match = /^paseo:\/\/h\/([^/?#]+)\/agent\/([^/?#]+)$/.exec(url);
  if (!match) return false;
  const serverId = decodeSegment(match[1]!);
  const agentId = decodeSegment(match[2]!);
  if (serverId === null || agentId === null) return false;
  return `paseo://h/${encodeURIComponent(serverId)}/agent/${encodeURIComponent(agentId)}` === url;
}

function decodeSegment(encoded: string): string | null {
  let decoded: string;
  try {
    decoded = decodeURIComponent(encoded);
  } catch {
    return null;
  }
  if (!decoded || decoded === "." || decoded === "..") return null;
  // eslint-disable-next-line no-control-regex -- reject control characters in identifiers
  if (/[\u0000-\u001f\u007f]/.test(decoded)) return null;
  return decoded;
}

// Canonical wrapper around shell.openExternal. All renderer-controlled URLs
// that eventually reach the OS shell MUST flow through here; direct calls
// to `shell.openExternal` elsewhere in the main process are banned by the
// no-restricted-syntax rule in apps/desktop/eslint.config.mjs.
export function openExternalSafely(url: string): Promise<void> | void {
  if (getHttpProtocol(url) === null && !isSafePaseoSessionUrl(url)) {
    console.warn(`[security] blocked openExternal: ${describeScheme(url)}`);
    return;
  }
  return shell.openExternal(url);
}

// Canonical wrapper around webContents.downloadURL. All renderer-controlled
// URLs that trigger a native download MUST flow through here; direct calls
// to `webContents.downloadURL` elsewhere in the main process are banned by
// the no-restricted-syntax rule in apps/desktop/eslint.config.mjs.
// Reuses the same http/https allowlist as openExternalSafely.
export function downloadURLSafely(win: BrowserWindow, url: string): void {
  if (getHttpProtocol(url) === null) {
    console.warn(`[security] blocked downloadURL: ${describeScheme(url)}`);
    return;
  }
  win.webContents.downloadURL(url);
}

function getHttpProtocol(url: string): "http:" | "https:" | null {
  try {
    const { protocol } = new URL(url);
    if (protocol === "http:" || protocol === "https:") return protocol;
    return null;
  } catch {
    return null;
  }
}

function describeScheme(url: string): string {
  try {
    return `scheme=${new URL(url).protocol}`;
  } catch {
    return "invalid URL";
  }
}

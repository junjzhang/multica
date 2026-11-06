/**
 * Strict Paseo session locator. A session URL is exactly
 * `paseo://h/<serverId>/agent/<paseoAgentId>`: the host component must be the
 * literal `h`, both identifiers are single canonical percent-encoded path
 * segments, and nothing else (credentials, query, fragment) is accepted.
 *
 * This is deliberately NOT a general scheme allowlist. Markdown sanitization
 * stays unchanged; only this typed link path may hand a matching URL to the
 * desktop opener.
 */
export const PASEO_SESSION_SCHEME = "paseo";

export interface PaseoSessionParts {
  serverId: string;
  agentId: string;
}

const PASEO_SESSION_PATH_RE = /^paseo:\/\/h\/([^/?#]+)\/agent\/([^/?#]+)$/;
const MAX_URL_LENGTH = 2048;
// Control characters never belong in an identifier, and `.`/`..` would be
// path traversal if any consumer reused the decoded value as a segment.
// Encoded separators are harmless here: path segmentation is decided on the
// encoded text, never re-parsed from the decoded identifier.
// eslint-disable-next-line no-control-regex -- reject control characters in identifiers
const UNSAFE_DECODED_SEGMENT_RE = /[\u0000-\u001f\u007f]/;

function decodeSegment(encoded: string): string | null {
  let decoded: string;
  try {
    decoded = decodeURIComponent(encoded);
  } catch {
    return null;
  }
  if (!decoded || decoded === "." || decoded === "..") return null;
  if (UNSAFE_DECODED_SEGMENT_RE.test(decoded)) return null;
  return decoded;
}

export function buildPaseoSessionUrl(serverId: string, agentId: string): string {
  return `${PASEO_SESSION_SCHEME}://h/${encodeURIComponent(serverId)}/agent/${encodeURIComponent(agentId)}`;
}

/**
 * Parse a Paseo session URL, returning null for any malformed or non-canonical
 * value. Canonical re-encoding rejects double-escaped identifiers and any
 * alternative spelling of the same path.
 */
export function parsePaseoSessionUrl(url: string | null | undefined): PaseoSessionParts | null {
  if (typeof url !== "string" || url.length === 0 || url.length > MAX_URL_LENGTH) return null;
  // `$` matches before a trailing newline in JS regex; reject raw whitespace
  // outright so a canonical URL never carries one.
  if (/\s/.test(url)) return null;
  const match = PASEO_SESSION_PATH_RE.exec(url);
  if (!match) return null;

  const serverId = decodeSegment(match[1]!);
  const agentId = decodeSegment(match[2]!);
  if (serverId === null || agentId === null) return null;

  if (buildPaseoSessionUrl(serverId, agentId) !== url) return null;
  return { serverId, agentId };
}

export function isValidPaseoSessionUrl(url: string | null | undefined): boolean {
  return parsePaseoSessionUrl(url) !== null;
}

const SECRET_PATTERN = /\b(?:mea|mat|mul)_[A-Za-z0-9._-]{8,}\b/g;
const CONTROL_PATTERN = /[\u0000-\u0008\u000b-\u001f\u007f-\u009f]/g;
export const SESSION_CONNECT_ERROR_PREFIX = "[multica:session-connect] ";

/**
 * Controls must be normalized before redaction: removing them afterward could
 * reconstruct a credential in a UI, log, or RPC diagnostic.
 */
export function redactSecrets(text: string): string {
  return text.replace(CONTROL_PATTERN, "").replace(SECRET_PATTERN, "[redacted]");
}

export function describeError(error: unknown): string {
  if (error instanceof Error) return redactSecrets(error.message);
  return redactSecrets(String(error));
}

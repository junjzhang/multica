const SECRET_PATTERN = /\b(?:mea|mat|mul)_[A-Za-z0-9._-]{8,}\b/g;
export const SESSION_CONNECT_ERROR_PREFIX = "[multica:session-connect] ";

/**
 * Removes credential-shaped text from messages that can reach a UI, a log, or
 * an error result. The plugin never builds such a message deliberately; this
 * is the last line before a remote body or filesystem error is surfaced.
 */
export function redactSecrets(text: string): string {
  return text.replace(SECRET_PATTERN, "[redacted]");
}

export function describeError(error: unknown): string {
  if (error instanceof Error) return redactSecrets(error.message);
  return redactSecrets(String(error));
}

import { z } from "zod";

/**
 * Timeline item kind the delivery card owns. Bump the version only together
 * with a reader, because a rendered row remembers the version it was written
 * with.
 */
export const DELIVERY_ITEM_KIND = "multica-delivery";
export const DELIVERY_ITEM_VERSION = 1;

export const deliveryDataSchema = z.object({
  deliveryId: z.string().optional(),
  kind: z.string(),
  kindLabel: z.string(),
  mentionMode: z.string().optional(),
  outcome: z.string().optional(),
  issueId: z.string().optional(),
  issueIdentifier: z.string().optional(),
  issueTitle: z.string().optional(),
  issueUrl: z.string().optional(),
  commentId: z.string().optional(),
  recipientAgentId: z.string().optional(),
  raw: z.string(),
});

export type DeliveryData = z.infer<typeof deliveryDataSchema>;

const REQUEST_HEADER = "Multica has a request for this session.";
const GENERIC_HEADER = /^Multica delivery (\S+)$/;
const KIND_WITH_RAW = /^(.+) \(([^()]+)\)$/;
const TASK_SEPARATOR = " — ";

const FIELD_LINE = /^([A-Za-z][A-Za-z0-9 ]*): ?(.*)$/;

/**
 * Reads the leading "Key: value" lines of an envelope. Reading stops at the
 * first blank line or at the first line that is not a field, which is how the
 * generic envelope's raw payload starts.
 */
function splitFields(lines: readonly string[]): Map<string, string> {
  const fields = new Map<string, string>();
  for (const line of lines) {
    if (line.trim() === "") return fields;
    const matched = FIELD_LINE.exec(line);
    if (!matched) return fields;
    fields.set(matched[1], matched[2]);
  }
  return fields;
}

function parseKindLine(value: string | undefined): { kind: string; label: string } | null {
  if (!value) return null;
  const matched = KIND_WITH_RAW.exec(value);
  if (matched) return { label: matched[1], kind: matched[2] };
  const kind = value.trim();
  if (kind === "") return null;
  return { kind, label: kind.replace(/_/g, " ") };
}

function parseTaskLine(value: string | undefined): { identifier?: string; title?: string } {
  const rest = (value ?? "").trim();
  if (rest === "") return {};
  const separator = rest.indexOf(TASK_SEPARATOR);
  if (separator < 0) return { identifier: rest };
  return {
    identifier: rest.slice(0, separator).trim() || undefined,
    title: rest.slice(separator + TASK_SEPARATOR.length).trim() || undefined,
  };
}

function parseIssueURL(fields: Map<string, string>): string | undefined {
  const link = (fields.get("Link") ?? "").trim();
  if (link === "") return undefined;
  try {
    const url = new URL(link);
    return url.protocol === "https:" || url.protocol === "http:" ? url.toString() : undefined;
  } catch {
    return undefined;
  }
}

function optional(value: string | undefined): string | undefined {
  const trimmed = value?.trim();
  return trimmed ? trimmed : undefined;
}

/**
 * Parses one Multica delivery prompt into the card's data.
 *
 * Returns null for anything this parser does not fully recognize: text written
 * by an older daemon, a partially streamed prompt, or a message that only
 * looks similar. Callers must render the original text in that case, so a
 * delivery is never hidden by a parsing miss.
 */
export function parseDelivery(text: string): DeliveryData | null {
  if (typeof text !== "string" || text === "") return null;
  const lines = text.split("\n");
  const header = (lines[0] ?? "").trimEnd();
  if (header === REQUEST_HEADER) return parseRequestEnvelope(lines);
  const generic = GENERIC_HEADER.exec(header);
  if (generic) return parseGenericEnvelope(lines, generic[1]);
  return null;
}

function parseRequestEnvelope(lines: string[]): DeliveryData | null {
  const fields = splitFields(lines.slice(1));
  const kind = parseKindLine(fields.get("Kind"));
  const issueId = optional(fields.get("Issue ID"));
  const mailboxId = optional(fields.get("Mailbox ID"));
  const recipientAgentId = optional(fields.get("Recipient agent ID"));
  if (!kind || !mailboxId || !issueId || !recipientAgentId) return null;
  const task = parseTaskLine(fields.get("Task"));
  return finalize({
    deliveryId: mailboxId,
    kind: kind.kind,
    kindLabel: kind.label,
    mentionMode: optional(fields.get("Mention mode")),
    issueId,
    issueIdentifier: task.identifier,
    issueTitle: task.title,
    issueUrl: parseIssueURL(fields),
    commentId: optional(fields.get("Comment ID")),
    recipientAgentId,
    raw: lines.join("\n"),
  });
}

function parseGenericEnvelope(lines: string[], deliveryId: string): DeliveryData | null {
  const fields = splitFields(lines.slice(1));
  const kind = parseKindLine(fields.get("Kind"));
  const recipientAgentId = optional(fields.get("Recipient agent ID"));
  if (!kind || !recipientAgentId) return null;
  const task = parseTaskLine(fields.get("Task"));
  let outcome: string | undefined;
  const payloadLine = lines.find((line) => line.startsWith("{"));
  if (payloadLine) {
    try {
      const payload: unknown = JSON.parse(payloadLine);
      const parsed = z.object({ outcome: z.string().min(1).max(100) }).safeParse(payload);
      if (parsed.success) outcome = parsed.data.outcome;
    } catch {
      // The original prompt remains available when the payload is not JSON.
    }
  }
  return finalize({
    deliveryId,
    kind: kind.kind,
    kindLabel: kind.label,
    outcome,
    issueId: optional(fields.get("Issue ID")),
    issueIdentifier: task.identifier,
    issueTitle: task.title,
    issueUrl: parseIssueURL(fields),
    recipientAgentId,
    raw: lines.join("\n"),
  });
}

function finalize(data: DeliveryData): DeliveryData | null {
  const parsed = deliveryDataSchema.safeParse(data);
  return parsed.success ? parsed.data : null;
}
/**
 * Fences arbitrary delivered text so a Markdown renderer shows it byte for
 * byte. A fence longer than any backtick run in the text cannot be closed by
 * the text itself; <thread-id> style fragments therefore stay literal.
 */
export function fencedRaw(text: string): string {
  const runs = text.match(/`+/g) ?? [];
  const longest = runs.reduce((max, run) => Math.max(max, run.length), 0);
  const fence = "`".repeat(Math.max(3, longest + 1));
  return fence + "\n" + text + "\n" + fence;
}

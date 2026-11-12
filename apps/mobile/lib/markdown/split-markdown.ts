/**
 * Split Markdown into native prose, React code blocks, and interactive images.
 * Use lexer token boundaries so fenced/indented code and paragraph images are
 * handled without fragile regex reconstruction. Preserve prose through token.raw.
 *
 * Only top-level code is extracted. Nested list/quote code stays in the native
 * renderer to preserve its surrounding Markdown structure.
 */
import { marked, type Tokens } from "marked";

export type MarkdownSegment =
  | { type: "prose"; content: string }
  | { type: "code"; lang: string | undefined; code: string }
  | { type: "image"; uri: string; alt: string };

type RawToken = { type: string; raw: string; href?: string; tokens?: RawToken[]; items?: RawToken[] };

// Annotate only parsed mention links; code spans and code blocks stay literal.
function labelNotifications(token: RawToken, label: string): string {
  if (token.type === "code" || token.type === "codespan") return token.raw;
  if (token.type === "link" && token.href?.startsWith("mention://agent-notify/")) {
    const marker = token.raw.lastIndexOf("](");
    if (marker >= 0) return `${token.raw.slice(0, marker)} · ${label}${token.raw.slice(marker)}`;
  }
  const children = token.tokens ?? token.items;
  if (!children) return token.raw;
  let result = "";
  let cursor = 0;
  for (const child of children) {
    const at = token.raw.indexOf(child.raw, cursor);
    if (at < 0) continue;
    result += token.raw.slice(cursor, at) + labelNotifications(child, label);
    cursor = at + child.raw.length;
  }
  return result + token.raw.slice(cursor);
}

export function splitMarkdown(input: string, notificationLabel?: string): MarkdownSegment[] {
  if (!input) return [];

  const tokens = marked.lexer(input);
  const out: MarkdownSegment[] = [];
  let proseBuffer = "";

  const flushProse = () => {
    const trimmed = proseBuffer.replace(/^\s+|\s+$/g, "");
    if (trimmed.length > 0) {
      out.push({ type: "prose", content: trimmed });
    }
    proseBuffer = "";
  };

  for (const token of tokens) {
    if (token.type === "code") {
      flushProse();
      const t = token as Tokens.Code;
      out.push({
        type: "code",
        lang: t.lang ? t.lang : undefined,
        code: t.text,
      });
      continue;
    }

    if (token.type === "paragraph") {
      const para = token as Tokens.Paragraph;
      const inline = para.tokens ?? [];
      const hasImage = inline.some((t) => t.type === "image");

      if (!hasImage) {
        proseBuffer += notificationLabel ? labelNotifications(para as RawToken, notificationLabel) : para.raw;
        continue;
      }

      // Split paragraph at each image. Text fragments before/after each
      // image rejoin the prose buffer as their own short paragraph.
      let textBuffer = "";
      const flushText = () => {
        const trimmed = textBuffer.trim();
        if (trimmed.length > 0) {
          // Append as its own paragraph so enriched treats consecutive
          // fragments as separate blocks rather than running them together.
          proseBuffer += trimmed + "\n\n";
        }
        textBuffer = "";
      };

      for (const t of inline) {
        if (t.type === "image") {
          flushText();
          flushProse();
          const img = t as Tokens.Image;
          out.push({
            type: "image",
            uri: img.href,
            alt: img.text ?? "",
          });
        } else {
          textBuffer += notificationLabel ? labelNotifications(t as RawToken, notificationLabel) : (t as { raw?: string }).raw ?? "";
        }
      }
      flushText();
      continue;
    }

    // Every other top-level block (heading, list, blockquote, table,
    // hr, html, space, def, …) goes to prose verbatim via token.raw.
    proseBuffer += notificationLabel ? labelNotifications(token as RawToken, notificationLabel) : (token as { raw?: string }).raw ?? "";
  }

  flushProse();
  return out;
}

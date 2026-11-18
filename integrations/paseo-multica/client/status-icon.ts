/**
 * Multica's issue status glyphs (packages/views/issues/components/status-icon.tsx)
 * as SVG strings, so the panel speaks the same visual language as the board.
 * Shapes carry the status on their own; color only reinforces it.
 */
const C = 7;
const R = 6;
const FILL_R = 3.5;
const RING = `<circle cx="${C}" cy="${C}" r="${R}" fill="none" stroke="currentColor" stroke-width="1.5"/>`;

function pie(progress: number): string {
  const angle = 2 * Math.PI * progress;
  const x = (C + FILL_R * Math.sin(angle)).toFixed(3);
  const y = (C - FILL_R * Math.cos(angle)).toFixed(3);
  return `<path d="M${C},${C} L${C},${C - FILL_R} A${FILL_R},${FILL_R} 0 ${progress > 0.5 ? 1 : 0},1 ${x},${y} Z" fill="currentColor"/>`;
}

function dots(): string {
  return Array.from({ length: 16 }, (_, i) => {
    const angle = (i / 16) * Math.PI * 2 - Math.PI / 2;
    return `<circle cx="${(C + R * Math.cos(angle)).toFixed(3)}" cy="${(C + R * Math.sin(angle)).toFixed(3)}" r="0.55" fill="currentColor"/>`;
  }).join("");
}

const svg = (body: string) => `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 14 14" fill="none">${body}</svg>`;
const stroke = `stroke="currentColor" stroke-width="1.5" stroke-linecap="round"`;

export type StatusKey = "backlog" | "todo" | "in_progress" | "in_review" | "done" | "blocked" | "cancelled";

export const STATUS_SVG: Record<StatusKey, string> = {
  backlog: svg(dots()),
  todo: svg(RING),
  in_progress: svg(RING + pie(0.5)),
  in_review: svg(RING + pie(0.75)),
  done: svg(`<circle cx="${C}" cy="${C}" r="${R}" fill="currentColor"/><path d="M4.2 7.2 L6.1 9 L9.8 5.2" fill="none" stroke="#fff" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/>`),
  blocked: svg(`${RING}<line x1="4.525" y1="4.525" x2="9.475" y2="9.475" ${stroke}/>`),
  cancelled: svg(`${RING}<path d="M5 5 L9 9 M9 5 L5 9" ${stroke}/>`),
};

export const STATUS_LABEL: Record<StatusKey, string> = {
  backlog: "待规划", todo: "未开始", in_progress: "进行中", in_review: "评审中", done: "已完成", blocked: "阻塞", cancelled: "已取消",
};

const CATEGORY_STATUS: Record<string, StatusKey> = {
  unstarted: "todo", backlog: "backlog", started: "in_progress", in_progress: "in_progress", completed: "done", done: "done", cancelled: "cancelled", canceled: "cancelled", closed: "cancelled",
};

/** A built-in status keeps its own glyph; a custom one falls back to its category's. */
export function statusKey(issue: { status: string; statusCategory: string }): StatusKey {
  return (issue.status in STATUS_SVG ? issue.status : CATEGORY_STATUS[issue.statusCategory]) as StatusKey ?? "todo";
}

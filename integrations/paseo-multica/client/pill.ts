import type { PluginSerializableIcon } from "@getpaseo/plugin/client";
import { pillLabel, type BoardData } from "../shared/board";

export const PILL_ICON = "ListTodo";

export interface PillPresentation {
  label: string;
  title: string;
  icon: PluginSerializableIcon;
}

const CENTER = 8;
const RADIUS = 5.5;
const STROKE = 2.75;

function round(value: number): number {
  return Math.round(value * 1000) / 1000;
}

/**
 * A static ring in `currentColor`, so the host's icon color themes it. The
 * arc starts at twelve o'clock and runs clockwise; a full set of closed
 * children draws a full circle and nothing more.
 */
export function progressRingSvg({ done, total }: { done: number; total: number }): string {
  const fraction = Math.min(Math.max(done / total, 0), 1);
  const circle = (opacity: string) =>
    `<circle cx="${CENTER}" cy="${CENTER}" r="${RADIUS}" fill="none" stroke="currentColor" stroke-width="${STROKE}"${opacity}/>`;
  let arc = "";
  if (fraction === 1) {
    arc = circle("");
  } else if (fraction > 0) {
    const angle = fraction * 2 * Math.PI;
    const x = round(CENTER + RADIUS * Math.sin(angle));
    const y = round(CENTER - RADIUS * Math.cos(angle));
    const large = fraction > 0.5 ? 1 : 0;
    arc = `<path d="M${CENTER} ${CENTER - RADIUS} A${RADIUS} ${RADIUS} 0 ${large} 1 ${x} ${y}" fill="none" stroke="currentColor" stroke-width="${STROKE}"/>`;
  }
  return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16">${circle(' stroke-opacity="0.32"')}${arc}</svg>`;
}

/**
 * Width budget of the tooltip's task line, in Latin-character units; East
 * Asian wide characters count as two. Sized for the host's 280px tooltip at
 * its label font so the line never wraps; a different tooltip width or font
 * needs a new budget.
 */
export const TASK_LINE_UNITS = 32;
const WIDE = /[ᄀ-ᅟ⺀-꓏가-힣豈-﫿︰-﹏＀-｠￠-￦]/u;

const charWidth = (char: string) => (WIDE.test(char) ? 2 : 1);

export function clipLine(text: string, units = TASK_LINE_UNITS): string {
  const chars = [...text];
  if (chars.reduce((sum, char) => sum + charWidth(char), 0) <= units) return text;
  let used = 1;
  let kept = "";
  for (const char of chars) {
    if (used + charWidth(char) > units) break;
    used += charWidth(char);
    kept += char;
  }
  return `${kept.trimEnd()}…`;
}

/** The focus tree's child progress becomes a ring icon and a second tooltip line. */
export function taskPill(data: BoardData, now: number): PillPresentation {
  const { label, title, progress } = pillLabel(data, now);
  if (!progress) return { label, title, icon: PILL_ICON };
  return {
    label,
    title: `${clipLine(title)}\n已完成 ${progress.done}/${progress.total} 个子任务`,
    icon: { svg: progressRingSvg(progress) },
  };
}

export const UNAVAILABLE_PILL: PillPresentation = {
  label: "Multica · 不可用",
  title: "Multica tasks unavailable",
  icon: PILL_ICON,
};

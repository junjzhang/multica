import { z } from "zod";
import { isTerminal, type BoardIssue } from "./board";

/** Statuses a quick action may write. Done is absent on purpose: completing a task goes through acceptance. */
export const ACTION_STATUSES = ["in_progress", "blocked", "backlog", "cancelled"] as const;
export type ActionStatus = (typeof ACTION_STATUSES)[number];

export const taskActionSchema = z.discriminatedUnion("kind", [
  z.object({ kind: z.literal("rerun") }),
  z.object({ kind: z.literal("status"), status: z.enum(ACTION_STATUSES) }),
]);
export type TaskAction = z.infer<typeof taskActionSchema>;

/** A refused write carries the server's own reason; the credential never appears in it. */
export const taskActionResultSchema = z.discriminatedUnion("ok", [
  z.object({ ok: z.literal(true) }),
  z.object({ ok: z.literal(false), status: z.number(), reason: z.string() }),
]);
export type TaskActionResult = z.infer<typeof taskActionResultSchema>;

export interface QuickAction {
  action: TaskAction;
  label: string;
  /** Cancelling needs a second press; every other action runs at once. */
  confirm?: boolean;
}

const RERUN: QuickAction = { action: { kind: "rerun" }, label: "重跑" };
const status = (to: ActionStatus, label: string): QuickAction => ({ action: { kind: "status", status: to }, label, ...(to === "cancelled" ? { confirm: true } : {}) });
const CANCEL = status("cancelled", "取消");
const SWITCH_LABEL: Record<ActionStatus, string> = { in_progress: "改为进行中", blocked: "改为阻塞", backlog: "放回待规划", cancelled: "取消" };

/** The few actions that fit the task's state; a closed task has none. */
export function quickActions(issue: Pick<BoardIssue, "status" | "statusCategory" | "lastRunFailed" | "activeRun">): QuickAction[] {
  if (isTerminal(issue)) return [];
  if (issue.status === "blocked") return [status("in_progress", "恢复进行中"), status("backlog", "放回待规划"), CANCEL];
  if (issue.lastRunFailed && !issue.activeRun) return [RERUN, status("blocked", "转为阻塞"), CANCEL];
  return ACTION_STATUSES.filter((to) => to !== issue.status).map((to) => status(to, SWITCH_LABEL[to]));
}

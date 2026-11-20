import { z } from "zod";
import type { BoardIssue } from "./board";

/** Multica's built-in statuses in board order; cancelled is last because the menu sets it apart. */
export const ACTION_STATUSES = ["backlog", "todo", "in_progress", "in_review", "blocked", "done", "cancelled"] as const;
export type ActionStatus = (typeof ACTION_STATUSES)[number];

export const taskActionSchema = z.discriminatedUnion("kind", [
  /** Repeats that failed run; without a task id the server would pick the assignee's own run instead. */
  z.object({ kind: z.literal("rerun"), taskId: z.string().uuid() }),
  z.object({ kind: z.literal("status"), status: z.enum(ACTION_STATUSES) }),
]);
export type TaskAction = z.infer<typeof taskActionSchema>;

/** A refused write carries the server's own reason; the credential never appears in it. */
export const taskActionResultSchema = z.discriminatedUnion("ok", [
  z.object({ ok: z.literal(true) }),
  z.object({ ok: z.literal(false), status: z.number(), reason: z.string() }),
]);
export type TaskActionResult = z.infer<typeof taskActionResultSchema>;

export interface StatusChoice {
  status: ActionStatus;
  current: boolean;
  /** Why this status cannot be chosen now; the current status needs no reason. */
  blocked?: string;
}

export interface TaskMenuModel {
  /** The failed run a rerun repeats, when the latest run failed and its id is known. */
  rerunTaskId?: string;
  statuses: StatusChoice[];
}

/** The server refuses done (`issue_contract_acceptance_required`) until every criterion of the current contract is accepted. */
const CONTRACT_PENDING = "需先验收合同";

export function taskMenu(issue: Pick<BoardIssue, "status" | "contract" | "lastRunFailed" | "lastFailure" | "activeRun">): TaskMenuModel {
  const taskId = issue.lastRunFailed && !issue.activeRun ? issue.lastFailure?.taskId : undefined;
  const contractPending = !!issue.contract && !issue.contract.accepted;
  return {
    ...(taskId ? { rerunTaskId: taskId } : {}),
    statuses: ACTION_STATUSES.map((status) => {
      const current = status === issue.status;
      return { status, current, ...(!current && status === "done" && contractPending ? { blocked: CONTRACT_PENDING } : {}) };
    }),
  };
}

import type { Agent } from "../types";
import { isAgentRuntimeBound } from "./runtime-binding";

/**
 * How an agent executes. A missing execution_kind predates the WORK-9 field
 * and stays managed (existing behavior); an unknown value is not executable
 * by either boundary and must not be guessed into one.
 */
export type AgentExecutionKindState = "managed" | "external" | "unknown";

export function agentExecutionKind(
  agent: Pick<Agent, "execution_kind"> | null | undefined,
): AgentExecutionKindState {
  if (!agent) return "unknown";
  const kind = agent.execution_kind;
  // Only an omitted field is the legacy managed default. A present null,
  // empty or unrecognized value carries no execution boundary at all.
  if (kind === undefined) return "managed";
  if (kind === "managed" || kind === "external") return kind;
  return "unknown";
}

export function isExternalAgent(
  agent: Pick<Agent, "execution_kind"> | null | undefined,
): boolean {
  return agentExecutionKind(agent) === "external";
}

/**
 * Managed agents without a runtime keep their real repair path. External
 * agents are never flagged this way, even when their runtime columns are
 * empty — their host session is their execution boundary.
 */
export function agentNeedsRuntime(
  agent:
    | (Pick<Agent, "execution_kind"> & {
        runtime_id: string;
        runtime_bound?: boolean;
      })
    | null
    | undefined,
): boolean {
  if (!agent || agentExecutionKind(agent) !== "managed") return false;
  return !isAgentRuntimeBound(agent);
}

/**
 * Dispatch of an explicit mention. Absent means a native run for older
 * servers; anything other than the two known values is unknown and must not
 * be presented as a native run.
 */
export type MentionDispatchKind = "run" | "mailbox" | "unknown";

export function mentionDispatchKind(kind: string | null | undefined): MentionDispatchKind {
  // Only an omitted field predates dispatch_kind and means a native run.
  if (kind === undefined) return "run";
  if (kind === "run" || kind === "mailbox") return kind;
  return "unknown";
}

export function isMailboxDispatch(kind: string | null | undefined): boolean {
  return mentionDispatchKind(kind) === "mailbox";
}

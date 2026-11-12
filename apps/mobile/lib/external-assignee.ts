import type { Agent, ExternalSession, Issue } from "@multica/core/types";

/** An external Agent executes through a registered host session, not a bound
 *  runtime. Mobile reads its identity but never opens a new dispatch for it. */
export function isExternalExecutionAgent(agent: Agent): boolean {
  return agent.execution_kind === "external";
}

/** The external Agent assigned to this issue, if any. */
export function externalAssigneeAgent(
  issue: Issue,
  agents: Agent[],
): Agent | null {
  if (issue.assignee_type !== "agent" || !issue.assignee_id) return null;
  const agent = agents.find((candidate) => candidate.id === issue.assignee_id);
  return agent && isExternalExecutionAgent(agent) ? agent : null;
}

/**
 * The session bound to this exact issue for the agent.
 *
 * `undefined` means the server omitted the projection and the target is
 * unknown; `null` means the issue authoritatively has no binding. Never fall
 * back to the agent's oldest session — that would guess the wrong identity for
 * a template with several sessions.
 */
export function boundExternalSession(
  issue: Issue,
  agentId: string,
): ExternalSession | null | undefined {
  if (issue.external_session_bindings === undefined) return undefined;
  return (
    issue.external_session_bindings.find((binding) => binding.agent_id === agentId)
      ?.session ?? null
  );
}

/** "template › session" label for a read-only external assignee chip. */
export function externalAssigneeChipLabel(
  agent: Agent,
  session: ExternalSession | null | undefined,
  unknownSession: string,
): string {
  const template =
    agent.external_template?.name || agent.name;
  const sessionLabel = session?.display_name || session?.session_id;
  return `${template} › ${sessionLabel || unknownSession}`;
}

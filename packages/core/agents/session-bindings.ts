"use client";

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import { issueKeys } from "../issues/queries";

export const agentSessionKeys = {
  sessions: (wsId: string, agentId: string) =>
    ["workspaces", wsId, "agents", agentId, "sessions"] as const,
  issueBinding: (wsId: string, agentId: string, issueId: string) =>
    ["workspaces", wsId, "agents", agentId, "session-binding", issueId] as const,
};

/** Registered sessions for one agent; availability is computed server-side. */
export function agentSessionsOptions(wsId: string, agentId: string) {
  return queryOptions({
    queryKey: agentSessionKeys.sessions(wsId, agentId),
    queryFn: () => api.listAgentSessions(agentId),
    enabled: !!wsId && !!agentId,
    staleTime: 30_000,
  });
}

/** The exact active target for one issue, or null when none is recorded. */
export function agentIssueSessionBindingOptions(
  wsId: string,
  agentId: string,
  issueId: string,
) {
  return queryOptions({
    queryKey: agentSessionKeys.issueBinding(wsId, agentId, issueId),
    queryFn: () => api.getAgentIssueSessionBinding(agentId, issueId),
    enabled: !!wsId && !!agentId && !!issueId,
    staleTime: 10_000,
  });
}

/**
 * Saves the target for future notifications. It is deliberately not
 * optimistic: a rejected save leaves the displayed binding untouched, and the
 * issue projection is re-read only after the server confirms the write.
 */
export function useBindAgentSession(wsId: string) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({
      agentId,
      issueId,
      sessionId,
    }: {
      agentId: string;
      issueId: string;
      sessionId: string;
    }) => api.bindAgentSession(agentId, { issue_id: issueId, session_id: sessionId }),
    onSuccess: (_binding, variables) => {
      void queryClient.invalidateQueries({
        queryKey: agentSessionKeys.issueBinding(
          wsId,
          variables.agentId,
          variables.issueId,
        ),
      });
      void queryClient.invalidateQueries({
        queryKey: issueKeys.detail(wsId, variables.issueId),
      });
    },
  });
}

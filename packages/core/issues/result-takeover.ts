"use client";

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import type { ResultTakeoverRequest } from "../types/result-takeover";
import { issueKeys } from "./queries";
import { agentSessionKeys } from "../agents/session-bindings";

export const resultTakeoverKeys = {
  issue: (wsId: string, issueId: string) => ["workspaces", wsId, "issues", issueId, "result-takeover-options"] as const,
  options: (wsId: string, issueId: string, resultTaskId: string, fromReturnId: string) =>
    [...resultTakeoverKeys.issue(wsId, issueId), resultTaskId, fromReturnId] as const,
};

export function resultTakeoverOptions(wsId: string, issueId: string, resultTaskId: string, fromReturnId: string) {
  return queryOptions({
    queryKey: resultTakeoverKeys.options(wsId, issueId, resultTaskId, fromReturnId),
    queryFn: () => api.getResultTakeoverOptions(issueId, resultTaskId, fromReturnId),
    enabled: !!wsId && !!issueId && !!resultTaskId && !!fromReturnId,
    staleTime: 0,
    retry: false,
  });
}

export function useTakeoverIssueResult(wsId: string, issueId: string, agentId: string) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (request: ResultTakeoverRequest) => api.takeoverIssueResult(issueId, request),
    retry: false,
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: resultTakeoverKeys.issue(wsId, issueId) });
      void queryClient.invalidateQueries({ queryKey: issueKeys.tasks(issueId) });
      void queryClient.invalidateQueries({ queryKey: issueKeys.detail(wsId, issueId) });
      void queryClient.invalidateQueries({ queryKey: agentSessionKeys.issueBinding(wsId, agentId, issueId) });
    },
  });
}

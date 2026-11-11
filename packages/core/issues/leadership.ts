"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import type { IssueLeadershipHandoffRequest } from "../types";
import { workspaceKeys } from "../workspace/queries";
import { issueKeys } from "./queries";

/**
 * Explicit task-leader handoff. The server resolves a subtask to its root.
 * Invalidation is deliberately broad: the handoff
 * changes the root's leader, the issue's assignee and recipient, and
 * may stale affected contracts, so projections that embed the leader are
 * refreshed on settle rather than patched from the request.
 */
export function useHandoffIssueLeadership(wsId: string, issueId: string, rootIssueId: string) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (request: IssueLeadershipHandoffRequest) =>
      api.handoffIssueLeadership(issueId, request),
    retry: false,
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: issueKeys.detail(wsId, rootIssueId) });
      void queryClient.invalidateQueries({ queryKey: issueKeys.all(wsId) });
      void queryClient.invalidateQueries({ queryKey: workspaceKeys.agents(wsId) });
    },
  });
}

export function useHandoffIssueExecutor(wsId: string, issueId: string) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (request: Omit<IssueLeadershipHandoffRequest, "expected_revision">) =>
      api.handoffIssueExecutor(issueId, request),
    retry: false,
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: issueKeys.detail(wsId, issueId) });
      void queryClient.invalidateQueries({ queryKey: issueKeys.all(wsId) });
      void queryClient.invalidateQueries({ queryKey: workspaceKeys.agents(wsId) });
    },
  });
}

"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import type { IssueLeadershipHandoffRequest } from "../types";
import { workspaceKeys } from "../workspace/queries";
import { issueKeys } from "./queries";

/**
 * Explicit task-leader handoff. The endpoint always addresses the chain's
 * leadership root, so callers pass `rootIssueId` even when the dialog is
 * opened from a subtask. Invalidation is deliberately broad: the handoff
 * changes the root's leader, the issue's assignee and recipient, and
 * may stale affected contracts, so projections that embed the leader are
 * refreshed on settle rather than patched from the request.
 */
export function useHandoffIssueLeadership(wsId: string, rootIssueId: string) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (request: IssueLeadershipHandoffRequest) =>
      api.handoffIssueLeadership(rootIssueId, request),
    retry: false,
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: issueKeys.detail(wsId, rootIssueId) });
      void queryClient.invalidateQueries({ queryKey: issueKeys.all(wsId) });
      void queryClient.invalidateQueries({ queryKey: workspaceKeys.agents(wsId) });
    },
  });
}

import {
  queryOptions,
  useMutation,
  useQueryClient,
} from "@tanstack/react-query";
import { api } from "../api";
import type { ContractRequest } from "../api/issue-contract-schema";
import { issueKeys } from "./queries";

export function issueContractOptions(wsId: string, issueId: string) {
  return queryOptions({
    queryKey: [...issueKeys.all(wsId), "contract", issueId],
    queryFn: () => api.getIssueContract(issueId),
    enabled: !!wsId && !!issueId,
  });
}

export function useUpdateIssueContract(wsId: string, issueId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (request: ContractRequest) =>
      api.updateIssueContract(issueId, request),
    onSuccess: (data) => {
      qc.setQueryData(issueContractOptions(wsId, issueId).queryKey, data);
    },
    onSettled: () => qc.invalidateQueries({ queryKey: issueKeys.all(wsId) }),
  });
}

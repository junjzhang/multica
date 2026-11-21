import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";
import { issueKeys } from "./queries";

export function issueRecordsQueryKey(wsId: string, issueId: string) {
  return [...issueKeys.all(wsId), "records", issueId] as const;
}

export function issueRecordsOptions(wsId: string, issueId: string) {
  return queryOptions({
    queryKey: issueRecordsQueryKey(wsId, issueId),
    queryFn: () => api.listIssueRecords(issueId),
    enabled: !!wsId && !!issueId,
  });
}

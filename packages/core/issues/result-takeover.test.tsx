// @vitest-environment jsdom
import { act, renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { PropsWithChildren } from "react";
import { api } from "../api";
import { issueKeys } from "./queries";
import { useTakeoverIssueResult } from "./result-takeover";

vi.mock("../api", () => ({ api: { takeoverIssueResult: vi.fn() } }));

const request = {
  client_request_id: "one-intent", result_task_id: "executor-C", from_return_id: "A", expected_current_return_id: "A",
  target: { kind: "managed_task" as const, source_task_id: "source-B", selection_version: "proof-B" },
};

beforeEach(() => vi.clearAllMocks());
afterEach(() => vi.restoreAllMocks());

describe("takeover mutation", () => {
  it("leaves saved result and unknown A intact until an authoritative refresh", async () => {
    const client = new QueryClient({ defaultOptions: { mutations: { retry: false }, queries: { retry: false } } });
    const original = [{ id: "executor-C", result: { output: "saved" }, result_notifications: [{ return_id: "A", status: "unknown" }] }];
    client.setQueryData(issueKeys.tasks("issue"), original);
    let accept!: (value: Awaited<ReturnType<typeof api.takeoverIssueResult>>) => void;
    vi.spyOn(api, "takeoverIssueResult").mockImplementation(() => new Promise((resolve) => { accept = resolve; }));
    const invalidate = vi.spyOn(client, "invalidateQueries");
    const wrapper = ({ children }: PropsWithChildren) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
    const view = renderHook(() => useTakeoverIssueResult("workspace", "issue", "dispatcher"), { wrapper });
    act(() => view.result.current.mutate(request));
    await waitFor(() => expect(view.result.current.isPending).toBe(true));
    expect(client.getQueryData(issueKeys.tasks("issue"))).toEqual(original);
    await act(async () => accept({ id: "action", ...request, target_return_id: "B", current_return_id: "C", created_at: "now" }));
    await waitFor(() => expect(view.result.current.isSuccess).toBe(true));
    expect(client.getQueryData(issueKeys.tasks("issue"))).toEqual(original);
    expect(invalidate).toHaveBeenCalledWith({ queryKey: issueKeys.tasks("issue") });
    expect(view.result.current.data?.target_return_id).toBe("B");
    expect(view.result.current.data?.current_return_id).toBe("C");
  });

  it("does not silently retry an unknown write or turn it into success", async () => {
    const client = new QueryClient();
    const call = vi.spyOn(api, "takeoverIssueResult").mockRejectedValue(new TypeError("network disconnected"));
    const wrapper = ({ children }: PropsWithChildren) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
    const view = renderHook(() => useTakeoverIssueResult("workspace", "issue", "dispatcher"), { wrapper });
    act(() => view.result.current.mutate(request));
    await waitFor(() => expect(view.result.current.isError).toBe(true));
    expect(call).toHaveBeenCalledTimes(1);
    expect(view.result.current.data).toBeUndefined();
    act(() => view.result.current.mutate(request));
    await waitFor(() => expect(call).toHaveBeenCalledTimes(2));
    expect(call.mock.calls.map((args) => args[1])).toEqual([request, request]);
  });
});

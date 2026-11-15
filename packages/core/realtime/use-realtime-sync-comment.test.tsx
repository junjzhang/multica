/**
 * @vitest-environment jsdom
 */
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { resultTakeoverKeys, resultTakeoverOptions } from "../issues/result-takeover";
import type { WSClient } from "../api/ws-client";
import { issueKeys, issueTasksOptions, type IssueSortParam } from "../issues/queries";
import type { AgentTask, Issue, ListIssuesCache } from "../types";
import { ApiClient, setApiInstance } from "../api";
import { useRealtimeSync, type RealtimeSyncStores } from "./use-realtime-sync";

vi.mock("../platform/workspace-storage", () => ({
  getCurrentWsId: () => "ws-1",
  getCurrentSlug: () => "test-ws",
  // Draft stores are now loaded transitively (storage-cleanup → register-all-drafts)
  // so their persist wiring must resolve against this mock.
  createWorkspaceAwareStorage: (adapter: unknown) => adapter,
  registerForWorkspaceRehydration: () => {},
}));

vi.mock("../paths", () => ({
  useHasOnboarded: () => true,
  resolvePostAuthDestination: () => "/",
}));

// Records every ws.on handler by event name so a test can fire one directly.
function createRecordingWs(): {
  ws: WSClient;
  handlers: Record<string, (p: unknown) => void>;
} {
  const handlers: Record<string, (p: unknown) => void> = {};
  const ws = {
    on: vi.fn((event: string, handler: (p: unknown) => void) => {
      handlers[event] = handler;
      return () => {};
    }),
    onAny: vi.fn(() => () => {}),
    onReconnect: vi.fn(() => () => {}),
  } as unknown as WSClient;
  return { ws, handlers };
}

function createStores(): RealtimeSyncStores {
  return {
    authStore: Object.assign(() => ({}), {
      getState: () => ({ user: { id: "u1" } }),
      subscribe: () => () => {},
      setState: () => {},
      destroy: () => {},
    }),
  } as unknown as RealtimeSyncStores;
}

function createWrapper(qc: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  };
}

const updatedSort: IssueSortParam = {
  sort_by: "updated_at",
  sort_direction: "desc",
};
const positionSort: IssueSortParam = { sort_by: "position" };
const updatedBoardKey = issueKeys.listSorted("ws-1", updatedSort);
const positionBoardKey = issueKeys.listSorted("ws-1", positionSort);
const lastActivityBoardKey = issueKeys.listSorted("ws-1", {
  sort_by: "last_activity",
  sort_direction: "desc",
});

function bucketed(): ListIssuesCache {
  return { byStatus: { unstarted: { issues: [], total: 1 } } };
}

describe("useRealtimeSync — comment activity cache coherence", () => {
  let qc: QueryClient;

  beforeEach(() => {
    qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  });

  afterEach(() => {
    qc.clear();
    vi.clearAllMocks();
  });

  it("invalidates the updated_at-sorted board but leaves the position board", () => {
    qc.setQueryData<ListIssuesCache>(updatedBoardKey, bucketed());
    qc.setQueryData<ListIssuesCache>(positionBoardKey, bucketed());
    qc.setQueryData<ListIssuesCache>(lastActivityBoardKey, bucketed());
    qc.setQueryData(issueKeys.timeline("issue-1"), []);

    const { ws, handlers } = createRecordingWs();
    renderHook(() => useRealtimeSync(ws, createStores()), {
      wrapper: createWrapper(qc),
    });

    handlers["comment:created"]?.({
      comment: { id: "c1", issue_id: "issue-1" },
    });

    expect(qc.getQueryState(updatedBoardKey)?.isInvalidated).toBe(true);
    expect(qc.getQueryState(lastActivityBoardKey)?.isInvalidated).toBe(true);
    expect(qc.getQueryState(positionBoardKey)?.isInvalidated).toBe(false);
    // The per-issue timeline is still invalidated as before.
    expect(
      qc.getQueryState(issueKeys.timeline("issue-1"))?.isInvalidated,
    ).toBe(true);
  });

  it("uses update/delete owner revisions and re-sorts only last_activity", () => {
    const detailKey = issueKeys.detail("ws-1", "issue-1");
    qc.setQueryData<Issue>(detailKey, {
      id: "issue-1",
      workspace_id: "ws-1",
      revision: 1,
    } as Issue);
    qc.setQueryData<ListIssuesCache>(updatedBoardKey, bucketed());
    qc.setQueryData<ListIssuesCache>(positionBoardKey, bucketed());
    qc.setQueryData<ListIssuesCache>(lastActivityBoardKey, bucketed());
    qc.setQueryData(issueKeys.timeline("issue-1"), []);

    const { ws, handlers } = createRecordingWs();
    renderHook(() => useRealtimeSync(ws, createStores()), {
      wrapper: createWrapper(qc),
    });

    handlers["comment:updated"]?.({
      comment: { id: "c1", issue_id: "issue-1" },
      issue_revision: 2,
    });

    expect(qc.getQueryState(detailKey)?.isInvalidated).toBe(true);
    expect(qc.getQueryState(lastActivityBoardKey)?.isInvalidated).toBe(true);
    expect(qc.getQueryState(updatedBoardKey)?.isInvalidated).toBe(false);
    expect(qc.getQueryState(positionBoardKey)?.isInvalidated).toBe(false);

    qc.setQueryData<Issue>(detailKey, {
      id: "issue-1",
      workspace_id: "ws-1",
      revision: 2,
    } as Issue);
    qc.setQueryData<ListIssuesCache>(lastActivityBoardKey, bucketed());

    handlers["comment:deleted"]?.({
      comment_id: "c1",
      issue_id: "issue-1",
      issue_revision: 3,
    });

    expect(qc.getQueryState(detailKey)?.isInvalidated).toBe(true);
    expect(qc.getQueryState(lastActivityBoardKey)?.isInvalidated).toBe(true);
  });

  it("ignores a comment event with no issue_id", () => {
    qc.setQueryData<ListIssuesCache>(updatedBoardKey, bucketed());

    const { handlers } = (() => {
      const rec = createRecordingWs();
      renderHook(() => useRealtimeSync(rec.ws, createStores()), {
        wrapper: createWrapper(qc),
      });
      return rec;
    })();

    handlers["comment:created"]?.({ comment: { id: "c1" } });

    expect(qc.getQueryState(updatedBoardKey)?.isInvalidated).toBe(false);
  });
});


it("invalidates only the affected detail and task runs when mailbox state changes", () => {
  const qc = new QueryClient();
  const current = issueKeys.detail("ws-1", "issue-1");
  const unrelated = issueKeys.detail("ws-1", "issue-2");
  const foreign = issueKeys.detail("ws-2", "issue-1");
  const runs = issueKeys.tasks("issue-1");
  const unrelatedRuns = issueKeys.tasks("issue-2");
  for (const key of [current, unrelated, foreign, runs, unrelatedRuns, positionBoardKey]) qc.setQueryData(key, {});
  const { ws, handlers } = createRecordingWs();
  const view = renderHook(() => useRealtimeSync(ws, createStores()), { wrapper: createWrapper(qc) });
  for (const payload of [null, {}, { issue_id: "" }, { issue_id: 42 }]) {
    handlers["issue_mailbox:changed"]!(payload);
  }
  expect(qc.getQueryState(runs)?.isInvalidated).toBe(false);
  handlers["issue_mailbox:changed"]!({ issue_id: "issue-1" });
  expect(qc.getQueryState(current)?.isInvalidated).toBe(true);
  expect(qc.getQueryState(runs)?.isInvalidated).toBe(true);
  for (const key of [unrelated, unrelatedRuns, foreign, positionBoardKey]) expect(qc.getQueryState(key)?.isInvalidated).toBe(false);
  view.unmount();
  qc.clear();
});

it("refetches an observed fresh task-runs query on the mailbox event without polling", async () => {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const task: AgentTask = {
    id: "run-1", agent_id: "worker", runtime_id: "runtime", issue_id: "issue-1", status: "completed",
    priority: 0, dispatched_at: null, started_at: null, completed_at: null, created_at: "2026-11-03T00:00:00Z",
    result: { output: "Saved result" }, error: null,
    result_notifications: [{ return_id: "return-1", recipient_agent_id: "leader", kind: "external_session", status: "pending" }],
  };
  qc.setQueryData(issueKeys.tasks("issue-1"), [task]);
  qc.setQueryData(issueKeys.tasks("issue-2"), []);
  const client = new ApiClient("https://api.example.test");
  const read = vi.spyOn(client, "listTasksByIssue").mockResolvedValue([{
    ...task,
    result_notifications: [{ ...task.result_notifications![0]!, status: "delivered" }],
  }]);
  setApiInstance(client);
  const { ws, handlers } = createRecordingWs();
  const view = renderHook(() => {
    useRealtimeSync(ws, createStores());
    return useQuery(issueTasksOptions("issue-1"));
  }, { wrapper: createWrapper(qc) });
  try {
    expect(view.result.current.data?.[0]?.result_notifications?.[0]?.status).toBe("pending");
    expect(read).not.toHaveBeenCalled();
    await act(async () => handlers["issue_mailbox:changed"]!({ issue_id: "issue-1" }));
    await waitFor(() => expect(view.result.current.data?.[0]?.result_notifications?.[0]?.status).toBe("delivered"));
    expect(read).toHaveBeenCalledExactlyOnceWith("issue-1", false, { view: "timeline" });
    expect(view.result.current.data?.[0]?.result).toEqual(task.result);
    expect(qc.getQueryState(issueKeys.tasks("issue-2"))?.isInvalidated).toBe(false);
  } finally {
    view.unmount();
    read.mockRestore();
    setApiInstance(undefined as unknown as ApiClient);
    qc.clear();
  }
});

it("refreshes the exact return selection snapshot when another takeover changes the route", async () => {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const key = resultTakeoverKeys.options("ws-1", "issue-1", "result", "A");
  const historical = { return_id: "A", kind: "managed_task" as const, target: null };
  qc.setQueryData(key, { current_return_id: "A", returns: [historical], targets: [] });
  const foreign = resultTakeoverKeys.options("ws-2", "issue-1", "result", "A");
  qc.setQueryData(foreign, { current_return_id: "foreign", returns: [], targets: [] });
  const client = new ApiClient("https://api.example.test");
  const read = vi.spyOn(client, "getResultTakeoverOptions").mockResolvedValue({ current_return_id: "B", returns: [historical], targets: [] });
  setApiInstance(client);
  const { ws, handlers } = createRecordingWs();
  const view = renderHook(() => {
    useRealtimeSync(ws, createStores());
    return useQuery({ ...resultTakeoverOptions("ws-1", "issue-1", "result", "A"), staleTime: Infinity });
  }, { wrapper: createWrapper(qc) });
  try {
    expect(view.result.current.data?.current_return_id).toBe("A");
    expect(read).not.toHaveBeenCalled();
    await act(async () => handlers["issue_mailbox:changed"]!({ issue_id: "issue-1" }));
    await waitFor(() => expect(view.result.current.data?.current_return_id).toBe("B"));
    expect(read).toHaveBeenCalledExactlyOnceWith("issue-1", "result", "A");
    expect(qc.getQueryState(foreign)?.isInvalidated).toBe(false);
  } finally {
    view.unmount(); read.mockRestore(); setApiInstance(undefined as unknown as ApiClient); qc.clear();
  }
});

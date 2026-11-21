// @vitest-environment node

import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient } from "./client";

afterEach(() => vi.unstubAllGlobals());

const notification = {
  return_id: "return-1",
  recipient_agent_id: "leader-1",
  kind: "managed_task",
  handling_task_id: "handling-1",
  status: "queued",
};

async function readRuns(rows: unknown[]) {
  const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify(rows), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  }));
  vi.stubGlobal("fetch", fetchMock);
  const tasks = await new ApiClient("https://api.example.test").listTasksByIssue("issue-1");
  expect(fetchMock.mock.calls[0]?.[0]).toBe("https://api.example.test/api/issues/issue-1/task-runs");
  return tasks;
}

describe("task result notifications at the API read boundary", () => {
  it("distinguishes an unhydrated run from a known empty notification list", async () => {
    const tasks = await readRuns([
      { id: "unhydrated", result: { output: "Done" } },
      { id: "hydrated", result_notifications: [] },
    ]);
    expect(tasks[0]?.result_notifications).toBeUndefined();
    expect(tasks[1]?.result_notifications).toEqual([]);
    expect(tasks[0]?.result).toEqual({ output: "Done" });
  });

  it.each([
    ["managed_task", "pending"],
    ["managed_task", "queued"],
    ["managed_task", "deferred"],
    ["managed_task", "dispatched"],
    ["managed_task", "waiting_local_directory"],
    ["managed_task", "running"],
    ["managed_task", "received"],
    ["managed_task", "cancelled"],
    ["managed_task", "refused"],
    ["managed_task", "suppressed"],
    ["managed_task", "unknown"],
    ["external_session", "pending"],
    ["external_session", "leased"],
    ["external_session", "delivered"],
    ["external_session", "failed"],
    ["external_session", "refused"],
    ["external_session", "suppressed"],
    ["external_session", "unknown"],
  ])("preserves %s notification state %s independently from the completed result", async (kind, status) => {
    const [task] = await readRuns([{
      id: "run-1",
      status: "completed",
      result: { output: "Executor result" },
      result_notifications: [{ ...notification, kind, status }],
    }]);
    expect(task?.status).toBe("completed");
    expect(task?.result).toEqual({ output: "Executor result" });
    expect(task?.result_notifications?.[0]?.status).toBe(status);
  });

  it.each([
    { kind: "future_transport", status: "received", expectedKind: "unknown" },
    { kind: null, status: "delivered", expectedKind: "unknown" },
    { kind: "managed_task", status: "future_state", expectedKind: "managed_task" },
    { kind: "managed_task", status: "failed", expectedKind: "managed_task" },
    { kind: "managed_task", status: "delivered", expectedKind: "managed_task" },
    { kind: "external_session", status: "received", expectedKind: "external_session" },
    { kind: "external_session", status: 42, expectedKind: "external_session" },
  ])("keeps unsupported semantics unknown: $kind / $status", async ({ kind, status, expectedKind }) => {
    const [task] = await readRuns([{
      id: "run-1",
      result_notifications: [{ ...notification, kind, status, future_detail: { receipt_version: 2 } }],
    }]);
    expect(task?.result_notifications).toEqual([{
      ...notification,
      kind: expectedKind,
      status: "unknown",
      future_detail: { receipt_version: 2 },
    }]);
  });

  it.each([null, "not an array", {}, [null], [{ return_id: 7 }], [notification, {}]])(
    "keeps malformed notification metadata unknown without deleting results: %j",
    async (value) => {
      const tasks = await readRuns([
        { id: "run-1", status: "failed", result: { output: "Partial result" }, error: "Executor failed", result_notifications: value },
        { id: "run-2", result_notifications: [notification] },
      ]);
      expect(tasks).toHaveLength(2);
      expect(tasks[0]).toMatchObject({ id: "run-1", status: "failed", result: { output: "Partial result" }, error: "Executor failed" });
      expect(tasks[0]?.result_notifications).toBeUndefined();
      expect(tasks[1]?.result_notifications).toEqual([notification]);
    },
  );

  it("keeps notification refusal and optional references separate from task errors", async () => {
    const [task] = await readRuns([{
      id: "run-1",
      status: "completed",
      result: { output: "Preserved final answer" },
      error: null,
      result_notifications: [
        { ...notification, status: "refused", error: "Original session unavailable" },
        { ...notification, status: "unknown", handling_task_id: null, delivery_id: 42, error: {} },
      ],
    }]);
    expect(task?.result).toEqual({ output: "Preserved final answer" });
    expect(task?.error).toBeNull();
    expect(task?.result_notifications?.[0]?.error).toBe("Original session unavailable");
    expect(task?.result_notifications?.[1]).toMatchObject({ status: "unknown", return_id: "return-1" });
    expect(task?.result_notifications?.[1]?.handling_task_id).toBeUndefined();
    expect(task?.result_notifications?.[1]?.delivery_id).toBeUndefined();
    expect(task?.result_notifications?.[1]?.error).toBeUndefined();
  });
});

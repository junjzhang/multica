// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import type { ResultTakeoverRequest } from "../types/result-takeover";
import { ApiClient } from "./client";
import { AgentTaskSchema, ResultNotificationSchema } from "./schemas";

const request: ResultTakeoverRequest = {
  client_request_id: "request-1", result_task_id: "executor-C", from_return_id: "return-A",
  expected_current_return_id: "return-A", target: { kind: "managed_task", source_task_id: "source-B", selection_version: "proof-B" },
};
const response = {
  id: "action-1", client_request_id: request.client_request_id, result_task_id: request.result_task_id,
  from_return_id: request.from_return_id, target_return_id: "return-B", current_return_id: "return-C", created_at: "2026-11-03T00:00:00Z",
};

afterEach(() => vi.unstubAllGlobals());

it("keeps accepted return knowledge separate from pending notifications and malformed metadata", () => {
  expect(AgentTaskSchema.parse({ id: "old" }).accepted_returns).toBeUndefined();
  expect(AgentTaskSchema.parse({ id: "empty", accepted_returns: [] }).accepted_returns).toEqual([]);
  for (const accepted_returns of [null, {}, [{}], [{ return_id: "return", recipient_agent_id: null }]]) {
    expect(AgentTaskSchema.parse({ id: "invalid", accepted_returns }).accepted_returns).toBeUndefined();
  }
  const accepted = { return_id: "return", recipient_agent_id: "dispatcher", kind: "managed_task", source_task_id: "source" };
  expect(AgentTaskSchema.parse({ id: "partial", accepted_returns: [accepted, {}] }).accepted_returns).toBeUndefined();
  const task = AgentTaskSchema.parse({ id: "running", result_notifications: [], accepted_returns: [accepted] });
  expect(task.accepted_returns).toEqual([accepted]);
  expect(task.result_notifications).toEqual([]);
});

function clientReturning(body: unknown) {
  const fetch = vi.fn().mockResolvedValue(new Response(JSON.stringify(body), { status: 200 }));
  vi.stubGlobal("fetch", fetch);
  return { api: new ApiClient("https://example.test"), fetch };
}

describe("saved-result takeover", () => {
  it("keeps the stable action target distinct from a later current route", async () => {
    const { api, fetch } = clientReturning(response);
    await expect(api.takeoverIssueResult("issue-1", request)).resolves.toEqual(response);
    expect(fetch).toHaveBeenCalledWith("https://example.test/api/issues/issue-1/result-takeovers", expect.objectContaining({
      method: "POST", body: JSON.stringify(request),
    }));
  });

  it.each([null, {}, { ...response, target_return_id: null }, { ...response, current_return_id: undefined },
    { ...response, client_request_id: "different-request" }, { ...response, result_task_id: "different-result" }])(
    "does not claim a confirmed action from malformed or unrelated response %j", async (body) => {
      await expect(clientReturning(body).api.takeoverIssueResult("issue-1", request)).rejects.toThrow(/confirm/i);
    },
  );

  it("preserves old unknown and new target notifications without rewriting the saved result", () => {
    const result = { output: "original saved output" };
    const task = AgentTaskSchema.parse({ id: "C", agent_id: "executor", status: "completed", result,
      result_notifications: [
        { return_id: "A", current_return_id: "B", recipient_agent_id: "dispatcher", kind: "managed_task", status: "unknown" },
        { return_id: "B", current_return_id: "B", takeover_id: "action", recipient_agent_id: "dispatcher", kind: "managed_task", status: "queued" },
      ],
    });
    expect(task.result).toEqual(result);
    expect(task.result_notifications?.map((note) => [note.return_id, note.current_return_id, note.status])).toEqual([
      ["A", "B", "unknown"], ["B", "B", "queued"],
    ]);
  });

  it("does not invent takeover metadata for an older backend", () => {
    expect(ResultNotificationSchema.parse({ return_id: "A", recipient_agent_id: "dispatcher", kind: "managed_task", status: "unknown" }).current_return_id).toBeUndefined();
  });
});

describe("takeover selection snapshot", () => {
  const snapshot = { current_return_id: "A", returns: [{ return_id: "A", kind: "managed_task", target: null }], targets: [] };
  it("preserves an explicit empty target list and inaccessible historical target", async () => {
    await expect(clientReturning(snapshot).api.getResultTakeoverOptions("issue", "C", "A")).resolves.toEqual(snapshot);
  });
  it.each([{}, { ...snapshot, returns: [] }, { ...snapshot, targets: undefined }, { ...snapshot, current_return_id: null },
    { ...snapshot, returns: [{ return_id: "A", kind: "managed_task" }] },
    { ...snapshot, targets: [{ kind: "managed_task", source_task_id: "B" }] },
    { ...snapshot, targets: [{ kind: "managed_task", source_task_id: "B", provider: "pi", session_ref: "safe-ref", display_name: "Safe run", available: true }] }])(
    "rejects unreadable selection metadata rather than showing an empty registry: %j", async (body) => {
      await expect(clientReturning(body).api.getResultTakeoverOptions("issue", "C", "A")).rejects.toThrow(/unavailable/i);
    },
  );
  it("does not enable a target when its availability proof is missing", async () => {
    const { api } = clientReturning({ ...snapshot, targets: [{ kind: "managed_task", source_task_id: "B", selection_version: "proof-B", provider: "codex", session_ref: "session-B", display_name: "B" }] });
    expect((await api.getResultTakeoverOptions("issue", "C", "A")).targets[0]?.available).toBe(false);
  });
});

it("renders only server-authorized session references and strips raw address fields", async () => {
  const { api } = clientReturning({ current_return_id: "A", returns: [{ return_id: "A", kind: "managed_task", target: {
    provider: "pi", session_ref: "session-safe", display_name: "Authorized issue", session_id: "/private/home/transcript.jsonl", work_dir: "/private/work",
  } }], targets: [] });
  const parsed = await api.getResultTakeoverOptions("issue", "C", "A");
  expect(parsed.returns[0]?.target).toEqual({ provider: "pi", session_ref: "session-safe", display_name: "Authorized issue" });
  expect(JSON.stringify(parsed)).not.toContain("/private/");
});

// @vitest-environment node
import { afterEach, expect, it, vi } from "vitest";
import { ApiClient } from "./client";

afterEach(() => vi.unstubAllGlobals());

const request = {
  client_request_id: "request", expected_issue_revision: 1,
  previous_agent_id: "old", agent_id: "new", reason: "handoff",
};

it("sends explicit leader and executor intents to different endpoints", async () => {
  const fetchMock = vi.fn()
    .mockResolvedValueOnce(new Response(JSON.stringify({ root_issue_id: "root", revision: 2, issue_revision: 2 })))
    .mockResolvedValueOnce(new Response(JSON.stringify({ issue_id: "child", issue_revision: 2 })));
  vi.stubGlobal("fetch", fetchMock);
  const api = new ApiClient("https://example.test");
  await api.handoffIssueLeadership("child", { ...request, expected_revision: 1 });
  await api.handoffIssueExecutor("child", request);
  expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
    "https://example.test/api/issues/child/task-leader/handoff",
    "https://example.test/api/issues/child/executor/handoff",
  ]);
});

it.each([null, {}, { issue_id: "child" }, { issue_id: "", issue_revision: 2 }])(
  "rejects an unconfirmed executor handoff: %j", async (body) => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify(body))));
    await expect(new ApiClient("https://example.test").handoffIssueExecutor("child", request)).rejects.toThrow(/confirm/i);
  },
);

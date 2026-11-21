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
  const leadership = { ...request, expected_revision: 1, suppress_run: true };
  const executor = { ...request, suppress_run: true };
  await api.handoffIssueLeadership("child", leadership);
  await api.handoffIssueExecutor("child", executor);
  expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
    "https://example.test/api/issues/child/task-leader/handoff",
    "https://example.test/api/issues/child/executor/handoff",
  ]);
  expect(fetchMock.mock.calls.map(([, init]) => JSON.parse(init.body))).toEqual([leadership, executor]);
});

it.each([
  { value: true, expected: true },
  { value: false, expected: false },
  { value: undefined, expected: false },
  { value: "true", expected: false },
])("parses the handoff suppression echo: $value", async ({ value, expected }) => {
  const fetchMock = vi.fn()
    .mockResolvedValueOnce(new Response(JSON.stringify({ root_issue_id: "root", suppress_run: value })))
    .mockResolvedValueOnce(new Response(JSON.stringify({ issue_id: "child", issue_revision: 2, suppress_run: value })));
  vi.stubGlobal("fetch", fetchMock);
  const api = new ApiClient("https://example.test");
  await expect(api.handoffIssueLeadership("child", { ...request, expected_revision: 1 })).resolves.toMatchObject({ suppress_run: expected });
  await expect(api.handoffIssueExecutor("child", request)).resolves.toMatchObject({ suppress_run: expected });
});

it.each([null, {}, { issue_id: "child" }, { issue_id: "", issue_revision: 2 }])(
  "rejects an unconfirmed executor handoff: %j", async (body) => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify(body))));
    await expect(new ApiClient("https://example.test").handoffIssueExecutor("child", request)).rejects.toThrow(/confirm/i);
  },
);

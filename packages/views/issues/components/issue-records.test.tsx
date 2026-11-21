// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { IssueRecord } from "@multica/core/api/issue-record-schema";
import { renderWithI18n } from "../../test/i18n";

const apiMock = vi.hoisted(() => ({ listIssueRecords: vi.fn() }));
vi.mock("@multica/core/api", async (original) => ({
  ...(await original<typeof import("@multica/core/api")>()),
  api: apiMock,
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getActorName: (_type: string, id: string) => `name:${id}` }),
}));
import { IssueRecords } from "./issue-records";

function record(overrides: Partial<IssueRecord>): IssueRecord {
  return {
    id: `r${overrides.seq ?? 1}`,
    issue_id: "i1",
    seq: 1,
    kind: "run",
    summary: "summary",
    rationale: "",
    evidence: [],
    verdict: "pending",
    fields: {},
    supersedes_seq: null,
    author_type: "agent",
    author_id: "a1",
    created_at: "2026-11-21T00:00:00Z",
    ...overrides,
  };
}

function renderRecords() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithI18n(
    <QueryClientProvider client={qc}>
      <IssueRecords issueId="i1" />
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("IssueRecords", () => {
  it("renders nothing for an empty ledger", async () => {
    apiMock.listIssueRecords.mockResolvedValue([]);
    const { container } = renderRecords();
    await waitFor(() => expect(apiMock.listIssueRecords).toHaveBeenCalled());
    expect(container.querySelector("[data-issue-records]")).toBeNull();
  });

  it("shows records in order, links URLs and marks superseded rows", async () => {
    apiMock.listIssueRecords.mockResolvedValue([
      record({ seq: 1, summary: "FA3 repeat", evidence: ["https://example.test/run/1", "ci-run/abc"], fields: { protocol: "v1" } }),
      record({ seq: 2, summary: "FA3 concluded", verdict: "pass", supersedes_seq: 1 }),
    ]);
    renderRecords();
    const section = await screen.findByRole("region", { name: "Records" });
    const first = section.querySelector('[data-record-seq="1"]') as HTMLElement;
    const second = section.querySelector('[data-record-seq="2"]') as HTMLElement;
    expect(within(first).getByText("FA3 repeat").className).toContain("line-through");
    expect(within(first).getByText(/superseded by #2/)).toBeTruthy();
    expect(within(first).getByRole("link", { name: "https://example.test/run/1" }).getAttribute("href")).toBe("https://example.test/run/1");
    expect(within(first).getByText("ci-run/abc").tagName).toBe("LI");
    expect(within(first).getByText("protocol=v1")).toBeTruthy();
    expect(within(second).getByText(/corrects #1/)).toBeTruthy();
    expect(within(second).getByText("FA3 concluded").className).not.toContain("line-through");
  });

  it("does not turn non-http evidence into links", async () => {
    apiMock.listIssueRecords.mockResolvedValue([record({ evidence: ["javascript:alert(1)"] })]);
    renderRecords();
    const section = await screen.findByRole("region", { name: "Records" });
    expect(within(section).queryByRole("link")).toBeNull();
  });
});

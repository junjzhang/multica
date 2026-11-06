// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ApiError } from "@multica/core/api";
import type { ContractView } from "@multica/core/api/issue-contract-schema";
import { renderWithI18n } from "../../test/i18n";
import { issueContractOptions } from "@multica/core/issues/contract";

const apiMock = vi.hoisted(() => ({
  getIssueContract: vi.fn(),
  updateIssueContract: vi.fn(),
  listChildIssues: vi.fn(),
}));
vi.mock("@multica/core/api", async (original) => ({
  ...(await original<typeof import("@multica/core/api")>()),
  api: apiMock,
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getActorName: (_type: string, id: string) => id }),
}));
vi.mock("../../common/actor-avatar", () => ({ ActorAvatar: () => <span /> }));
import { IssueContract } from "./issue-contract";

function view(role: "submit" | "review", version = 1): ContractView {
  const executor = { type: "member", id: "executor" };
  const acceptor = { type: "member", id: "acceptor" };
  const merge = { allowed: false, repository_url: null, target_branch: null };
  return {
    contract: {
      sequence: version,
      versions: [
        {
          number: version,
          goal: "Goal",
          scope: "Scope",
          authorization_note: "Existing scope",
          criteria: [`Criterion ${version}`],
          executor,
          acceptor,
          author: acceptor,
          created_at: "2026-11-06T00:00:00Z",
          basis: { merge_authorization: merge },
        },
      ],
      deliveries:
        role === "review"
          ? [
              {
                id: `delivery-${version}`,
                version,
                criteria: [0],
                evidence: "Submitted evidence",
                child_ids: [],
                author: executor,
                created_at: "2026-11-06T00:00:00Z",
              },
            ]
          : [],
      decisions: [],
    },
    stale: false,
    accepted: [],
    pending_actor: role === "submit" ? executor : acceptor,
    executor,
    acceptor,
    merge_authorization: merge,
    can_edit: false,
    can_submit: role === "submit",
    can_review: role === "review",
  };
}

function renderContract() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  renderWithI18n(
    <QueryClientProvider client={client}>
      <IssueContract issueId="issue-1" />
    </QueryClientProvider>,
  );
  return client;
}

beforeEach(() => {
  vi.clearAllMocks();
  apiMock.listChildIssues.mockResolvedValue({ issues: [] });
});
afterEach(cleanup);

describe("contract draft recovery", () => {
  it("keeps the revision dialog inside the same lock as a review draft", async () => {
    let current = { ...view("review"), can_edit: true };
    apiMock.getIssueContract.mockImplementation(async () => current);
    apiMock.updateIssueContract.mockResolvedValue({
      ...current,
      can_review: false,
    });
    const client = renderContract();
    const user = userEvent.setup();
    await user.click(
      await screen.findByRole("checkbox", { name: "Criterion 1" }),
    );
    current = { ...view("review", 2), can_edit: true };
    current.contract!.sequence = 4;
    act(() =>
      client.setQueryData(
        issueContractOptions("ws-1", "issue-1").queryKey,
        current,
      ),
    );
    await screen.findByRole("checkbox", { name: "Criterion 2" });
    expect(
      screen.getByRole("button", { name: "Revise contract" }),
    ).toBeDisabled();
    expect(
      screen.getByRole("checkbox", { name: "Criterion 2" }),
    ).not.toBeChecked();
    await user.click(screen.getByRole("button", { name: "Revise contract" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Accept selected" }));
    expect(apiMock.updateIssueContract).not.toHaveBeenCalled();
    await user.click(
      screen.getByRole("button", { name: "Discard draft and reload" }),
    );
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: "Revise contract" }),
      ).toBeEnabled(),
    );
    await user.click(screen.getByRole("button", { name: "Revise contract" }));
    await user.keyboard("{Escape}");
    expect(
      screen.getByRole("checkbox", { name: "Criterion 2" }),
    ).not.toBeChecked();
    await user.click(screen.getByRole("checkbox", { name: "Criterion 2" }));
    await user.click(screen.getByRole("button", { name: "Accept selected" }));
    await waitFor(() =>
      expect(apiMock.updateIssueContract).toHaveBeenCalledTimes(1),
    );
    expect(apiMock.updateIssueContract.mock.calls[0]![1]).toMatchObject({
      action: "accept",
      version: 2,
      expected_sequence: 4,
      delivery_id: "delivery-2",
      criteria: [0],
    });
    client.clear();
  });
  for (const role of ["submit", "review"] as const)
    it(`restarts a ${role} draft explicitly after a conflict`, async () => {
      let current = view(role);
      apiMock.getIssueContract.mockImplementation(async () => current);
      apiMock.updateIssueContract
        .mockImplementationOnce(async () => {
          current = view(role, 2);
          throw new ApiError("Contract changed", 409, "Conflict");
        })
        .mockImplementation(async () => ({
          ...current,
          can_submit: false,
          can_review: false,
        }));
      const client = renderContract();
      const user = userEvent.setup();
      await user.click(
        await screen.findByRole("checkbox", { name: "Criterion 1" }),
      );
      if (role === "submit")
        await user.type(
          screen.getByLabelText("Delivery evidence", { exact: true }),
          "Old evidence",
        );
      await user.click(
        screen.getByRole("button", {
          name: role === "submit" ? "Submit delivery" : "Accept selected",
        }),
      );
      const restart = await screen.findByRole("button", {
        name: "Discard draft and reload",
      });
      expect(
        await screen.findByRole("checkbox", { name: "Criterion 2" }),
      ).not.toBeChecked();
      expect(
        screen.getByRole("button", {
          name: role === "submit" ? "Submit delivery" : "Accept selected",
        }),
      ).toBeDisabled();
      expect(
        screen.queryByRole("button", { name: "Retry request" }),
      ).not.toBeInTheDocument();
      await user.click(restart);
      await waitFor(() =>
        expect(
          screen.queryByRole("button", { name: "Discard draft and reload" }),
        ).not.toBeInTheDocument(),
      );
      await user.click(screen.getByRole("checkbox", { name: "Criterion 2" }));
      if (role === "submit")
        await user.type(
          screen.getByLabelText("Delivery evidence", { exact: true }),
          "New evidence",
        );
      await user.click(
        screen.getByRole("button", {
          name: role === "submit" ? "Submit delivery" : "Accept selected",
        }),
      );
      await waitFor(() =>
        expect(apiMock.updateIssueContract).toHaveBeenCalledTimes(2),
      );
      const first = apiMock.updateIssueContract.mock.calls[0]![1];
      const second = apiMock.updateIssueContract.mock.calls[1]![1];
      expect(second).toMatchObject({
        expected_sequence: 2,
        version: 2,
        criteria: [0],
      });
      expect(second.client_request_id).not.toBe(first.client_request_id);
      if (role === "review") expect(second.delivery_id).toBe("delivery-2");
      client.clear();
    });

  it("retries the same request UUID when the result is unknown", async () => {
    const current = view("submit");
    apiMock.getIssueContract.mockResolvedValue(current);
    apiMock.updateIssueContract
      .mockRejectedValueOnce(new TypeError("Failed to fetch"))
      .mockResolvedValue({ ...current, can_submit: false });
    const client = renderContract();
    const user = userEvent.setup();
    await user.click(
      await screen.findByRole("checkbox", { name: "Criterion 1" }),
    );
    await user.type(
      screen.getByLabelText("Delivery evidence", { exact: true }),
      "Evidence",
    );
    await user.click(screen.getByRole("button", { name: "Submit delivery" }));
    await user.click(
      await screen.findByRole("button", { name: "Retry request" }),
    );
    await waitFor(() =>
      expect(apiMock.updateIssueContract).toHaveBeenCalledTimes(2),
    );
    expect(apiMock.updateIssueContract.mock.calls[1]![1]).toEqual(
      apiMock.updateIssueContract.mock.calls[0]![1],
    );
    client.clear();
  });
});

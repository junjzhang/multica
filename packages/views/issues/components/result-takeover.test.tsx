// @vitest-environment jsdom
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { AgentTask, ResultNotification, ResultTakeoverOptions, ResultTakeoverRequest } from "@multica/core/types";
import { ApiError } from "@multica/core/api";
import { resultTakeoverKeys } from "@multica/core/issues";
import { issueKeys } from "@multica/core/issues/queries";
import { renderWithI18n } from "../../test/i18n";

const state = vi.hoisted(() => ({ canManage: true }));
const calls = vi.hoisted(() => ({ listAgents: vi.fn(), getResultTakeoverOptions: vi.fn(), takeoverIssueResult: vi.fn() }));
vi.mock("@multica/core/api", async (original) => ({ ...await original<typeof import("@multica/core/api")>(), api: calls }));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "workspace" }));
vi.mock("@multica/core/permissions", () => ({
  useAgentPermissions: () => ({ canEdit: { allowed: state.canManage }, isLoading: false }),
}));
vi.mock("@multica/core/workspace/hooks", () => ({ useActorName: () => ({ getActorName: () => "Dispatcher" }) }));
vi.mock("../../common/actor-avatar", () => ({ ActorAvatar: () => null }));
import { ResultNotifications } from "./result-notifications";

const note: ResultNotification = { return_id: "return-A", current_return_id: "return-A", recipient_agent_id: "dispatcher", kind: "managed_task", status: "unknown" };
const task = { id: "executor-C", agent_id: "executor", status: "completed", result: { output: "unchanged saved output" }, result_notifications: [note] } as AgentTask;
const options: ResultTakeoverOptions = {
  current_return_id: "return-A", returns: [{ return_id: "return-A", kind: "managed_task", target: { provider: "codex", session_ref: "session-A", display_name: "Original A" } }],
  targets: [
    { kind: "managed_task", source_task_id: "source-B", selection_version: "proof-B", provider: "codex", session_ref: "session-B", display_name: "Session B", available: true },
    { kind: "external_session", external_session_id: "external-C", provider: "paseo", session_ref: "session-C", display_name: "Session C", available: true },
    { kind: "managed_task", source_task_id: "ended", selection_version: "proof-ended", provider: "codex", session_ref: "ended-session", display_name: "Unavailable source", available: false },
  ],
};
function accepted(request: ResultTakeoverRequest) {
  return { id: "takeover-1", client_request_id: request.client_request_id, result_task_id: request.result_task_id, from_return_id: request.from_return_id, target_return_id: "return-B", current_return_id: "return-B", created_at: "now" };
}
function renderNotes(notes: ResultNotification[] = [note]) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  client.setQueryData(issueKeys.tasks("issue"), [task]);
  const view = renderWithI18n(<QueryClientProvider client={client}><ResultNotifications notifications={notes} task={task} issueId="issue" /></QueryClientProvider>);
  return { ...view, client };
}
async function openDialog() {
  fireEvent.click((await screen.findAllByRole("button", { name: "Choose a return session" }))[0]!);
  return screen.getByRole("dialog", { name: "Send saved result" });
}
beforeEach(() => {
  state.canManage = true;
  calls.listAgents.mockResolvedValue([{ id: "dispatcher", name: "Dispatcher", archived_at: null }]);
  calls.getResultTakeoverOptions.mockResolvedValue(options);
  calls.takeoverIssueResult.mockImplementation(async (_issue, request) => accepted(request));
});
afterEach(() => { cleanup(); vi.resetAllMocks(); });

describe("saved result return selection", () => {
  it("keeps viewers and older backends read-only", async () => {
    state.canManage = false;
    const view = renderNotes();
    expect(screen.getByText("Receipt unknown")).toBeInTheDocument();
    await waitFor(() => expect(calls.listAgents).toHaveBeenCalled());
    expect(calls.getResultTakeoverOptions).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: "Choose a return session" })).not.toBeInTheDocument();
    view.unmount();
    state.canManage = true;
    renderNotes([{ ...note, current_return_id: undefined }]);
    expect(calls.getResultTakeoverOptions).not.toHaveBeenCalled();
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
  });

  it.each([403, 404])("does not offer unavailable operations after HTTP %s", async (status) => {
    calls.getResultTakeoverOptions.mockRejectedValue(new ApiError("not available", status, "Unavailable"));
    renderNotes();
    await waitFor(() => expect(calls.getResultTakeoverOptions).toHaveBeenCalled());
    expect(screen.queryByRole("button", { name: "Choose a return session" })).not.toBeInTheDocument();
    expect(screen.queryByText("No eligible return sessions are available.")).not.toBeInTheDocument();
  });

  it("keeps a failed read unknown and requires an explicit available choice", async () => {
    calls.getResultTakeoverOptions.mockRejectedValueOnce(new TypeError("offline"));
    renderNotes();
    expect(await screen.findByText("Return session information is unavailable. Refresh before choosing.")).toBeInTheDocument();
    expect(screen.queryByText("No eligible return sessions are available.")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Reload and choose again" }));
    const dialog = await openDialog();
    const submit = within(dialog).getByRole("button", { name: "Send saved result and update return session" });
    expect(submit).toBeDisabled();
    expect(within(dialog).getByRole("radio", { name: /Unavailable source/ })).toBeDisabled();
    expect(within(dialog).getAllByRole("radio").every((radio) => !(radio as HTMLInputElement).checked)).toBe(true);
  });

  it("retries uncertain input with the exact original payload and key despite a refreshed current route", async () => {
    calls.takeoverIssueResult.mockRejectedValueOnce(new TypeError("response lost"));
    const { client } = renderNotes();
    let dialog = await openDialog();
    fireEvent.click(within(dialog).getByRole("radio", { name: /Session B/ }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Send saved result and update return session" }));
    expect(await screen.findByText("The outcome is unknown. Retry uses the original request; it does not create a second notification.")).toBeInTheDocument();
    act(() => client.setQueryData(resultTakeoverKeys.options("workspace", "issue", "executor-C", "return-A"), { ...options, current_return_id: "new-route", targets: options.targets.map((choice) => choice.kind === "managed_task" ? { ...choice, selection_version: "new-pin-proof" } : choice) }));
    fireEvent.click(within(dialog).getAllByRole("button", { name: "Close" })[0]!);
    dialog = await openDialog();
    fireEvent.click(within(dialog).getByRole("button", { name: "Retry the same request" }));
    expect(await screen.findByText("Return-session request recorded. Check the notification status below.")).toBeInTheDocument();
    expect(calls.takeoverIssueResult).toHaveBeenCalledTimes(2);
    const first = calls.takeoverIssueResult.mock.calls[0]![1];
    expect(calls.takeoverIssueResult.mock.calls[1]![1]).toEqual(first);
    expect(first).toMatchObject({ expected_current_return_id: "return-A", result_task_id: "executor-C", target: { kind: "managed_task", source_task_id: "source-B", selection_version: "proof-B" } });
    expect(first.client_request_id).toMatch(/^[\da-f-]{36}$/);
    expect(client.getQueryData(issueKeys.tasks("issue"))).toEqual([task]);
    expect(screen.getByText("Receipt unknown")).toBeInTheDocument();
    expect(screen.queryByText("Received by session")).not.toBeInTheDocument();
  });

  it.each(["route", "session", "proof"])("requires another choice when the observed %s changes before submission", async (changed) => {
    const { client } = renderNotes();
    const dialog = await openDialog();
    fireEvent.click(within(dialog).getByRole("radio", { name: /Session B/ }));
    const refreshed = changed === "route" ? { ...options, current_return_id: "current-C" }
      : { ...options, targets: options.targets.map((choice) => choice.display_name === "Session B"
        ? changed === "session" ? { ...choice, session_ref: "repinned-B" } : { ...choice, selection_version: "proof-repinned" }
        : choice) };
    act(() => client.setQueryData(resultTakeoverKeys.options("workspace", "issue", "executor-C", "return-A"), refreshed));
    await screen.findByText("The selection changed. Choose a current available session again.");
    expect(within(dialog).getByRole("button", { name: "Send saved result and update return session" })).toBeDisabled();
    expect(calls.takeoverIssueResult).not.toHaveBeenCalled();
  });

  it("requires a new explicit selection after a conflict and uses a new request key", async () => {
    calls.takeoverIssueResult.mockRejectedValueOnce(new ApiError("route changed", 409, "Conflict"));
    renderNotes();
    const dialog = await openDialog();
    fireEvent.click(within(dialog).getByRole("radio", { name: /Session B/ }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Send saved result and update return session" }));
    expect(await screen.findByText("The return route or target changed. Retry the same request to check its outcome, or reload and choose again.")).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Retry the same request" })).toBeEnabled();
    calls.getResultTakeoverOptions.mockResolvedValue({ ...options, current_return_id: "route-C" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Reload and choose again" }));
    await waitFor(() => expect(within(dialog).getByRole("radio", { name: /Session C/ })).toBeEnabled());
    fireEvent.click(within(dialog).getByRole("radio", { name: /Session C/ }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Send saved result and update return session" }));
    await waitFor(() => expect(calls.takeoverIssueResult).toHaveBeenCalledTimes(2));
    const [first, second] = calls.takeoverIssueResult.mock.calls.map((args) => args[1]);
    expect(second.client_request_id).not.toBe(first.client_request_id);
    expect(second).toMatchObject({ expected_current_return_id: "route-C", target: { kind: "external_session", external_session_id: "external-C" } });
  });

  it("makes changing target after an unknown request a separate explicit operation", async () => {
    calls.takeoverIssueResult.mockRejectedValueOnce(new TypeError("connection lost"));
    renderNotes();
    const dialog = await openDialog();
    fireEvent.click(within(dialog).getByRole("radio", { name: /Session B/ }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Send saved result and update return session" }));
    await screen.findByText("The outcome is unknown. Retry uses the original request; it does not create a second notification.");
    fireEvent.click(within(dialog).getByRole("button", { name: "Choose a different session" }));
    expect(screen.getByText("The previous request is still unconfirmed. Choosing another session starts a separate request.")).toBeInTheDocument();
    await waitFor(() => expect(within(dialog).getByRole("radio", { name: /Session C/ })).toBeEnabled());
    fireEvent.click(within(dialog).getByRole("radio", { name: /Session C/ }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Send saved result and update return session" }));
    await waitFor(() => expect(calls.takeoverIssueResult).toHaveBeenCalledTimes(2));
    const [first, second] = calls.takeoverIssueResult.mock.calls.map((args) => args[1]);
    expect(second.client_request_id).not.toBe(first.client_request_id);
    expect(first.target).toEqual({ kind: "managed_task", source_task_id: "source-B", selection_version: "proof-B" });
    expect(second.target).toEqual({ kind: "external_session", external_session_id: "external-C" });
  });

  it("keeps the original request available when target discovery later becomes unavailable", async () => {
    calls.takeoverIssueResult.mockRejectedValueOnce(new TypeError("response lost"));
    renderNotes();
    const dialog = await openDialog();
    fireEvent.click(within(dialog).getByRole("radio", { name: /Session B/ }));
    calls.getResultTakeoverOptions.mockRejectedValue(new ApiError("source disappeared", 404, "Not Found"));
    fireEvent.click(within(dialog).getByRole("button", { name: "Send saved result and update return session" }));
    await screen.findByText("The outcome is unknown. Retry uses the original request; it does not create a second notification.");
    fireEvent.click(within(dialog).getByRole("button", { name: "Retry the same request" }));
    await screen.findByText("Return-session request recorded. Check the notification status below.");
    expect(calls.takeoverIssueResult.mock.calls[1]![1]).toEqual(calls.takeoverIssueResult.mock.calls[0]![1]);
  });

  it("preserves existing B uncertainty and distinguishes a stable action target from actual current", async () => {
    calls.takeoverIssueResult.mockImplementation(async (_issue, request) => ({ ...accepted(request), current_return_id: "return-C" }));
    calls.getResultTakeoverOptions.mockResolvedValue({ ...options, current_return_id: "return-B", returns: [
      ...options.returns, { return_id: "return-B", kind: "managed_task", target: { provider: "codex", session_ref: "session-B", display_name: "Session B" } },
    ] });
    renderNotes([{ ...note, current_return_id: "return-B" }, { ...note, return_id: "return-B", current_return_id: "return-B", status: "unknown" }]);
    const dialog = await openDialog();
    fireEvent.click(within(dialog).getByRole("radio", { name: /Session B/ }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Send saved result and update return session" }));
    expect(await screen.findByText("This result already has a notification for that session. Its existing status is retained.")).toBeInTheDocument();
    expect(screen.getByText("Another return session is now current. This request still refers to the session you selected.")).toBeInTheDocument();
    expect(screen.getAllByText("Receipt unknown")).toHaveLength(2);
    expect(screen.queryByText("Received by session")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /rerun|re-run/i })).not.toBeInTheDocument();
  });
});

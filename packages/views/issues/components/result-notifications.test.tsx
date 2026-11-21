// @vitest-environment jsdom

import { cleanup, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { ResultNotification } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";

vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({
    getActorName: (_type: string, id: string) => id === "leader" ? "Leader" : id === "reviewer" ? "Reviewer" : "Unknown Agent",
  }),
}));

vi.mock("../../common/actor-avatar", () => ({ ActorAvatar: () => null }));

import { ResultNotifications } from "./result-notifications";

afterEach(cleanup);

function note(overrides: Partial<ResultNotification> = {}): ResultNotification {
  return { return_id: "return-1", recipient_agent_id: "leader", kind: "managed_task", status: "queued", ...overrides };
}

describe("ResultNotifications", () => {
  it("distinguishes unavailable metadata from a known empty list without claiming receipt", () => {
    const view = renderWithI18n(<ResultNotifications notifications={undefined} />);
    expect(screen.getByRole("region", { name: "Result notifications" })).toBeInTheDocument();
    expect(screen.getByText("Notification status unavailable")).toBeInTheDocument();
    expect(screen.queryByText("No result notifications")).not.toBeInTheDocument();
    expect(screen.queryByText("Received by session")).not.toBeInTheDocument();
    view.unmount();

    renderWithI18n(<ResultNotifications notifications={[]} />);
    expect(screen.getByText("No result notifications")).toBeInTheDocument();
    expect(screen.queryByText("Notification status unavailable")).not.toBeInTheDocument();
    expect(screen.queryByText("Received by session")).not.toBeInTheDocument();
  });

  it("keeps waiting and processing distinct from the session receipt", () => {
    renderWithI18n(<ResultNotifications notifications={[
      note(),
      note({ return_id: "return-2", recipient_agent_id: "reviewer", status: "running" }),
      note({ return_id: "return-3", kind: "external_session", status: "delivered", delivery_id: "internal-delivery" }),
    ]} />);
    const items = screen.getAllByRole("listitem");
    expect(within(items[0]!).getByText("Leader")).toBeInTheDocument();
    expect(within(items[0]!).getByText("Waiting")).toBeInTheDocument();
    expect(within(items[1]!).getByText("Reviewer")).toBeInTheDocument();
    expect(within(items[1]!).getByText("Processing")).toBeInTheDocument();
    expect(within(items[1]!).queryByText("Received by session")).not.toBeInTheDocument();
    expect(within(items[2]!).getByText("Received by session")).toBeInTheDocument();
    expect(screen.getByText("Receiving a notification does not mean the result is approved.")).toBeInTheDocument();
    expect(screen.queryByText("internal-delivery")).not.toBeInTheDocument();
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
  });

  it("shows uncertainty and refusal with separate, redacted diagnostics", () => {
    renderWithI18n(<ResultNotifications notifications={[
      note({ status: "unknown", error: "Receipt lookup timed out" }),
      note({ return_id: "return-2", recipient_agent_id: "unavailable-agent", status: "refused",
        handling_task_id: "internal-handler", error: "Request refused: Bearer fake-secret-value" }),
    ]} />);
    const items = screen.getAllByRole("listitem");
    expect(within(items[0]!).getByText("Receipt unknown")).toBeInTheDocument();
    expect(within(items[0]!).getByText("Receipt lookup timed out")).toBeInTheDocument();
    expect(within(items[1]!).getByText("Unknown Agent")).toBeInTheDocument();
    expect(within(items[1]!).getByText("Cannot deliver")).toBeInTheDocument();
    expect(within(items[1]!).getByText("Technical details")).toBeInTheDocument();
    expect(within(items[1]!).getByText("Request refused: Bearer [REDACTED]")).toBeInTheDocument();
    expect(screen.queryByText(/fake-secret-value|internal-handler|unavailable-agent/)).not.toBeInTheDocument();
    expect(screen.queryByText("Received by session")).not.toBeInTheDocument();
  });

  it("shows a suppressed progress-only return as neutral, not as a delivery failure", () => {
    renderWithI18n(<ResultNotifications notifications={[note({ kind: "external_session", status: "suppressed" })]} />);
    const item = screen.getByRole("listitem");
    const label = within(item).getByText("Not sent: no action needed");
    expect(label).toHaveClass("text-muted-foreground");
    expect(within(item).queryByText("Cannot deliver")).not.toBeInTheDocument();
    expect(screen.queryByText("Received by session")).not.toBeInTheDocument();
  });

  it("does not turn unsupported semantics into a positive receipt", () => {
    renderWithI18n(<ResultNotifications notifications={[
      note({ kind: "unknown", status: "received" }),
      note({ return_id: "return-2", status: "future_state" as ResultNotification["status"] }),
    ]} />);
    expect(screen.getAllByText("Receipt unknown")).toHaveLength(2);
    expect(screen.queryByText("Received by session")).not.toBeInTheDocument();
    expect(screen.queryByText(/result is approved/)).not.toBeInTheDocument();
  });

  it("localizes managed receipt without calling it business acceptance", () => {
    renderWithI18n(<ResultNotifications notifications={[note({ status: "received" })]} />, { locale: "zh-Hans" });
    expect(screen.getByRole("region", { name: "结果通知" })).toBeInTheDocument();
    expect(screen.getByText("会话已接收")).toBeInTheDocument();
    expect(screen.getByText("收到通知不代表结果已验收。")).toBeInTheDocument();
  });
});

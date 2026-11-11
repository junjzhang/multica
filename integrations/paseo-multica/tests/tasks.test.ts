import { describe, expect, it } from "vitest";
import {
  TASK_GROUPS,
  sessionTasksViewSchema,
  sessionTasksWireSchema,
  taskCounts,
  taskGroupTitle,
  taskStatusLabel,
  taskSummaryLabel,
  toTasksView,
  type SessionTaskItem,
} from "../shared/tasks";
import { AGENT_A, SESSION_A, WORKSPACE_ID, makeTasksWire } from "./fixtures";

const CHECKED_AT = "2026-11-11T12:00:00.000Z";

function item(overrides: Partial<SessionTaskItem> = {}): SessionTaskItem {
  return {
    issueId: "0199a0b1-4444-7444-8444-444444444444",
    title: "Task",
    status: "in_progress",
    statusCategory: "started",
    role: "executor",
    ...overrides,
  };
}

describe("session task views", () => {
  it("maps the wire projection into the rendered view and drops unsafe links", () => {
    const wire = makeTasksWire({
      owned: [
        {
          issue_id: "0199a0b1-4444-7444-8444-444444444444",
          identifier: "WORK-1",
          title: "Owned task",
          status: "in_progress",
          status_name: "Doing",
          status_category: "started",
          url: "https://app.multica.example/acme/issues/WORK-1",
          role: "executor",
          updated_at: "2026-11-11T10:00:00Z",
        },
        {
          issue_id: "0199a0b1-7777-7777-8777-777777777777",
          title: "Unsafe link",
          status: "todo",
          status_category: "unstarted",
          url: "javascript:alert(1)",
          role: "leader",
        },
      ],
    });

    const view = toTasksView(wire, CHECKED_AT);
    expect(view.state).toBe("ready");
    expect(view.agentId).toBe(AGENT_A);
    expect(view.owned[0]).toMatchObject({
      identifier: "WORK-1",
      statusName: "Doing",
      url: "https://app.multica.example/acme/issues/WORK-1",
    });
    expect(view.owned[1]?.url).toBeUndefined();
    expect(view.checkedAt).toBe(CHECKED_AT);
  });

  it("parses the wire projection and refuses a partial one", () => {
    expect(sessionTasksWireSchema.safeParse(makeTasksWire()).success).toBe(true);
    expect(
      sessionTasksWireSchema.safeParse({ agent_id: AGENT_A, owned: [], accepting: [], returning: [] }).success,
    ).toBe(false);
    expect(
      sessionTasksWireSchema.safeParse({
        ...makeTasksWire(),
        owned: [{ issue_id: "x", title: "t", status: "todo", status_category: "unstarted", role: "nope" }],
      }).success,
    ).toBe(false);
  });

  it("keeps every view state parseable by the RPC contract", () => {
    const ready = toTasksView(makeTasksWire(), CHECKED_AT);
    const disconnected = {
      state: "disconnected" as const,
      agentId: AGENT_A,
      reason: "Connect this session.",
      checkedAt: CHECKED_AT,
    };
    const unavailable = {
      state: "unavailable" as const,
      agentId: AGENT_A,
      reason: "Older server.",
      checkedAt: CHECKED_AT,
    };
    for (const view of [ready, disconnected, unavailable]) {
      expect(sessionTasksViewSchema.safeParse(view).success).toBe(true);
    }
    expect(sessionTasksViewSchema.safeParse({ ...disconnected, reason: "" }).success).toBe(false);
  });

  it("labels the composer pill for work, idle, disconnected and unavailable sessions", () => {
    const ready = toTasksView(makeTasksWire(), CHECKED_AT);
    expect(taskSummaryLabel(ready)).toEqual({
      label: "3 tasks",
      title: "Multica tasks: 1 owned, 1 to accept, 1 to return",
    });
    expect(taskSummaryLabel(taskSummaryZero())).toEqual({
      label: "Multica",
      title: "No Multica tasks for this session",
    });
    expect(
      taskSummaryLabel({
        state: "disconnected",
        agentId: AGENT_A,
        reason: "No binding.",
        checkedAt: CHECKED_AT,
      }),
    ).toEqual({ label: "Multica", title: "Multica not connected. No binding." });
    expect(
      taskSummaryLabel({ state: "unavailable", agentId: AGENT_A, reason: "Older server.", checkedAt: CHECKED_AT }),
    ).toEqual({ label: "Multica", title: "Multica tasks unavailable. Older server." });
  });

  it("renders one concise count per obligation and a stable status label", () => {
    const ready = toTasksView(makeTasksWire(), CHECKED_AT);
    expect(taskCounts(ready)).toEqual({ owned: 1, accepting: 1, returning: 1 });
    expect(TASK_GROUPS.map(taskGroupTitle)).toEqual([
      "Your tasks",
      "Waiting for your acceptance",
      "Returns pending",
    ]);
    expect(taskStatusLabel(item({ status: "in_review" }))).toBe("in review");
    expect(taskStatusLabel(item({ status: "custom_key", statusName: "Custom" }))).toBe("Custom");
  });
});

function taskSummaryZero() {
  return toTasksView(
    makeTasksWire({ owned: [], accepting: [], returning: [] }),
    CHECKED_AT,
  );
}

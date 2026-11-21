import { describe, expect, it } from "vitest";
import {
  boardDataSchema,
  buildBoard,
  classify,
  currentStage,
  groupByProject,
  isFresh,
  pillLabel,
  STALE_AFTER_DAYS,
  type Actor,
  type BoardIssue,
  type BoardPullRequest,
  type ReadyBoardData,
  type Viewer,
} from "../shared/board";

const NOW = Date.parse("2026-11-17T12:00:00Z");
const DAY = 24 * 60 * 60 * 1000;
const ago = (days: number) => new Date(NOW - days * DAY).toISOString();
const SESSION: Actor = { type: "agent", id: "leader", session_id: "session-1" };
const OTHER_SESSION: Actor = { type: "agent", id: "leader", session_id: "session-2" };
const EXECUTOR: Actor = { type: "agent", id: "executor" };
const USER: Actor = { type: "member", id: "user-1" };
const viewer: Viewer = { agentId: "leader", sessionId: "session-1", userId: "user-1", forgeUsernames: ["jayjayzhang"] };

let sequence = 0;
function issue(overrides: Partial<BoardIssue> = {}): BoardIssue {
  sequence += 1;
  return {
    id: `issue-${sequence}`,
    identifier: `WORK-${sequence}`,
    title: `Task ${sequence}`,
    status: "in_progress",
    statusCategory: "started",
    lastActivityAt: ago(1),
    activeRun: false,
    lastRunFailed: false,
    pullRequests: [],
    ...overrides,
  };
}
function contract(pendingActor: Actor | undefined, accepted = false) {
  return { ...(pendingActor ? { pendingActor } : {}), acceptor: SESSION, executor: EXECUTOR, accepted };
}
function pr(overrides: Partial<BoardPullRequest> = {}): BoardPullRequest {
  return { id: "pr-1", number: 813, repo: "slime", title: "MR", state: "open", url: "https://git.example.com/o/slime/merge_requests/813", ...overrides };
}

describe("classification rules", () => {
  it("puts the session's own pending turn in needs-attention as an acceptance", () => {
    expect(classify(issue({ contract: contract(SESSION) }), viewer, NOW)).toMatchObject({ section: "attention", todos: ["acceptance"], marker: "leader" });
  });

  it("treats a pending turn of this agent in another session as someone else's", () => {
    expect(classify(issue({ contract: contract(OTHER_SESSION) }), viewer, NOW).section).toBe("parked");
  });

  it("labels the session's turn as executor work when it is not the acceptor", () => {
    const own = { ...contract(SESSION), executor: SESSION, acceptor: USER };
    expect(classify(issue({ contract: own }), viewer, NOW)).toMatchObject({ section: "attention", todos: ["session"] });
  });

  it("puts the user's own pending turn in needs-attention", () => {
    expect(classify(issue({ contract: contract(USER) }), viewer, NOW)).toMatchObject({ section: "attention", todos: ["user"], marker: "you" });
  });

  it("asks for MR approval only after acceptance, while open and not yet approved by the user", () => {
    const waiting = pr({ myReview: "approving", review: "approving", ci: "passed", mergeable: "mergeable" });
    expect(classify(issue({ contract: contract(undefined, true), pullRequests: [waiting] }), viewer, NOW)).toMatchObject({ section: "attention", todos: ["approval"], marker: "you" });
    expect(classify(issue({ contract: contract(EXECUTOR, false), pullRequests: [waiting] }), viewer, NOW).todos).not.toContain("approval");
    expect(classify(issue({ contract: contract(undefined, true), pullRequests: [{ ...waiting, myReview: "approved" }] }), viewer, NOW).todos).toEqual([]);
    expect(classify(issue({ contract: contract(undefined, true), pullRequests: [{ ...waiting, state: "merged" }] }), viewer, NOW).todos).toEqual([]);
    expect(classify(issue({ contract: contract(undefined, true), pullRequests: [{ ...waiting, myReview: undefined }] }), viewer, NOW).todos).toEqual([]);
  });

  it.each([
    ["blocked status", { status: "blocked" }],
    ["failed CI on an open MR", { pullRequests: [pr({ ci: "failed" })] }],
    ["a failed latest run", { lastRunFailed: true }],
  ] as [string, Partial<BoardIssue>][])("flags %s as an anomaly without an active run", (_name, overrides) => {
    expect(classify(issue({ contract: contract(EXECUTOR), ...overrides }), viewer, NOW)).toMatchObject({ section: "attention", todos: ["anomaly"], marker: "anomaly" });
    expect(classify(issue({ contract: contract(EXECUTOR), ...overrides, activeRun: true }), viewer, NOW)).toMatchObject({ section: "active", marker: "running" });
  });

  it("ignores failed CI on a merged MR", () => {
    expect(classify(issue({ contract: contract(EXECUTOR), pullRequests: [pr({ ci: "failed", state: "merged" })] }), viewer, NOW).section).toBe("active");
  });

  it("keeps an active run in progress regardless of activity age or status", () => {
    expect(classify(issue({ status: "backlog", lastActivityAt: ago(30), activeRun: true }), viewer, NOW)).toMatchObject({ section: "active", marker: "running" });
  });

  it("keeps the executor's turn in progress only within the stale window", () => {
    expect(classify(issue({ contract: contract(EXECUTOR), lastActivityAt: ago(STALE_AFTER_DAYS) }), viewer, NOW).section).toBe("active");
    expect(classify(issue({ contract: contract(EXECUTOR), lastActivityAt: new Date(NOW - STALE_AFTER_DAYS * DAY - 1).toISOString() }), viewer, NOW)).toMatchObject({ section: "parked", parkedReason: "stale" });
    expect(classify(issue({ contract: contract(EXECUTOR), lastActivityAt: undefined }), viewer, NOW)).toMatchObject({ section: "parked", parkedReason: "stale" });
  });

  it("parks backlog even when it is the executor's fresh turn", () => {
    expect(classify(issue({ status: "backlog", statusCategory: "backlog", contract: contract(EXECUTOR) }), viewer, NOW)).toMatchObject({ section: "parked", parkedReason: "backlog" });
  });

  it("parks a turn owned by anyone else without calling it stale while it is fresh", () => {
    expect(classify(issue({ contract: contract({ type: "agent", id: "someone" }) }), viewer, NOW)).toMatchObject({ section: "parked", parkedReason: "other" });
    expect(classify(issue({ contract: contract(undefined, true) }), viewer, NOW)).toMatchObject({ section: "parked", parkedReason: "other" });
    expect(classify(issue({ contract: contract({ type: "agent", id: "someone" }), lastActivityAt: ago(8) }), viewer, NOW).parkedReason).toBe("stale");
  });

  it("falls back for legacy tasks without a contract", () => {
    const coordinated = { status: "in_review", coordinator: SESSION };
    expect(classify(issue(coordinated), viewer, NOW)).toMatchObject({ section: "attention", todos: ["acceptance"], marker: "leader" });
    expect(classify(issue({ ...coordinated, coordinator: OTHER_SESSION }), viewer, NOW)).toMatchObject({ section: "active", todos: [] });
    expect(classify(issue({ ...coordinated, coordinator: { type: "agent", id: "leader" } }), viewer, NOW).todos).toEqual([]);
    expect(classify(issue({ status: "in_review" }), viewer, NOW).section).toBe("active");
    expect(classify(issue({ status: "in_review", lastActivityAt: ago(8) }), viewer, NOW)).toMatchObject({ section: "parked", parkedReason: "stale" });
    expect(classify(issue({ status: "todo" }), viewer, NOW).section).toBe("active");
    expect(classify(issue({ status: "todo", lastActivityAt: ago(8) }), viewer, NOW).section).toBe("parked");
  });

  it("keeps a done task with an open MR in needs-attention until the MR closes", () => {
    const waiting = pr({ myReview: "approving" });
    const done = { status: "done", statusCategory: "completed", contract: contract(undefined, true), pullRequests: [waiting] };
    expect(classify(issue(done), viewer, NOW)).toMatchObject({ section: "attention", todos: ["approval"], marker: "you" });
    expect(classify(issue({ ...done, pullRequests: [{ ...waiting, myReview: "approved" }] }), viewer, NOW)).toMatchObject({ section: "attention", todos: ["merge"] });
    expect(classify(issue({ ...done, pullRequests: [{ ...waiting, state: "merged" }] }), viewer, NOW).section).toBe("done");
    expect(classify(issue({ ...done, status: "cancelled", statusCategory: "cancelled" }), viewer, NOW).section).toBe("done");
    const root = issue({ status: "done", statusCategory: "completed" });
    const child = issue({ ...done, parentId: root.id });
    const board = buildBoard(data([root, child], [root.id]), NOW);
    expect(board.sections.attention.map((tree) => tree.issue.id)).toEqual([root.id]);
    expect(board.todos.map((todo) => [todo.kind, todo.issue.id])).toEqual([["approval", child.id]]);
  });

  it("files done and cancelled tasks under completed", () => {
    expect(classify(issue({ status: "done", statusCategory: "completed" }), viewer, NOW)).toMatchObject({ section: "done", marker: "done" });
    expect(classify(issue({ status: "cancelled", statusCategory: "cancelled", contract: contract(SESSION) }), viewer, NOW).section).toBe("done");
  });

  it("measures freshness inclusively at the boundary", () => {
    expect(isFresh(ago(7), NOW)).toBe(true);
    expect(isFresh(new Date(NOW - 7 * DAY - 1).toISOString(), NOW)).toBe(false);
    expect(isFresh("not a date", NOW)).toBe(false);
  });
});

function data(issues: BoardIssue[], rootIds: string[], extra: Partial<ReadyBoardData> = {}): ReadyBoardData {
  return { state: "ready", scope: "session", viewer, issues, rootIds, projects: [], truncated: false, checkedAt: new Date(NOW).toISOString(), ...extra };
}

describe("task trees", () => {
  it("places a tree in the section of its most urgent descendant and aggregates the marker", () => {
    const root = issue({ status: "backlog", statusCategory: "backlog" });
    const delivered = issue({ parentId: root.id, contract: contract(SESSION) });
    const blocked = issue({ parentId: delivered.id, status: "blocked", contract: contract(EXECUTOR) });
    const board = buildBoard(data([root, delivered, blocked], [root.id]), NOW);
    expect(board.sections.attention.map((tree) => tree.issue.id)).toEqual([root.id]);
    const tree = board.sections.attention[0]!;
    expect(tree.marker).toBe("anomaly");
    expect(tree.classification.marker).toBe("idle");
    expect(tree.stages[0]!.nodes[0]!.marker).toBe("anomaly");
    expect(board.todos.map((todo) => [todo.kind, todo.issue.id])).toEqual([["acceptance", delivered.id], ["anomaly", blocked.id]]);
  });

  it("orders aggregate markers ▲ > ● > ◐ > ⋯", () => {
    const roots = [
      issue({ activeRun: true }),
      issue({ contract: contract(SESSION) }),
      issue({ status: "blocked" }),
      issue({ contract: contract(USER) }),
    ];
    const board = buildBoard(data(roots, roots.map((root) => root.id)), NOW);
    expect(board.sections.attention.map((tree) => tree.marker)).toEqual(["anomaly", "you", "leader"]);
    expect(board.sections.active.map((tree) => tree.marker)).toEqual(["running"]);
  });

  it("does not show an open parent as complete because its children are", () => {
    const root = issue({ status: "todo", lastActivityAt: ago(20) });
    const child = issue({ parentId: root.id, status: "done", statusCategory: "completed" });
    expect(buildBoard(data([root, child], [root.id]), NOW).sections.parked[0]!.marker).toBe("idle");
  });

  it("groups children by stage and reports the first incomplete batch", () => {
    const root = issue({ contract: contract(EXECUTOR) });
    const done = { status: "done", statusCategory: "completed" };
    const children = [
      issue({ parentId: root.id, stage: 1, ...done }),
      issue({ parentId: root.id, stage: 1, ...done }),
      issue({ parentId: root.id, stage: 2, status: "todo", statusCategory: "unstarted", lastActivityAt: ago(20) }),
      issue({ parentId: root.id, stage: 2, status: "todo", statusCategory: "unstarted", lastActivityAt: ago(20) }),
    ];
    const tree = buildBoard(data([root, ...children], [root.id]), NOW).sections.active[0]!;
    expect(tree.stages.map((group) => [group.stage, group.done, group.total])).toEqual([[1, 2, 2], [2, 0, 2]]);
    expect(currentStage(tree)).toMatchObject({ stage: 2, done: 0, total: 2 });
    expect([tree.done, tree.total]).toEqual([2, 4]);
  });

  it("shows a root nested in another root only inside that tree", () => {
    const root = issue({ contract: contract(EXECUTOR) });
    const child = issue({ parentId: root.id, contract: contract(SESSION) });
    const board = buildBoard(data([root, child], [child.id, root.id]), NOW);
    expect(Object.values(board.sections).flat().map((tree) => tree.issue.id)).toEqual([root.id]);
    expect(board.todos).toHaveLength(1);
  });

  it("files a tree under completed only when every task in it is done, and folds it by default", () => {
    const root = issue({ status: "done", statusCategory: "completed" });
    const open = issue({ parentId: root.id, status: "todo", lastActivityAt: ago(9) });
    expect(buildBoard(data([root, open], [root.id]), NOW).sections.parked).toHaveLength(1);
    const closed = { ...open, status: "done", statusCategory: "completed" };
    expect(buildBoard(data([root, closed], [root.id]), NOW).sections.done).toHaveLength(1);
  });

  it("counts parked trees by backlog and staleness", () => {
    const roots = [
      issue({ status: "backlog" }),
      issue({ status: "todo", lastActivityAt: ago(10) }),
      issue({ status: "todo", lastActivityAt: ago(12) }),
      issue({ contract: contract({ type: "agent", id: "someone" }) }),
    ];
    expect(buildBoard(data(roots, roots.map((root) => root.id)), NOW).parkedReasons).toEqual({ backlog: 1, stale: 2, other: 1 });
  });

  it("survives a parent cycle", () => {
    const a = issue({ id: "a" });
    const b = issue({ id: "b", parentId: "a" });
    a.parentId = "b";
    const board = buildBoard(data([a, b], ["a"]), NOW);
    expect(Object.values(board.sections).flat()).toHaveLength(1);
  });

  it("groups the all view by project with unassigned work last", () => {
    const roots = [issue({ projectId: "p2" }), issue({}), issue({ projectId: "p1" })];
    const board = buildBoard(data(roots, roots.map((root) => root.id)), NOW);
    const groups = groupByProject(board.sections.active, [{ id: "p1", name: "Multica" }, { id: "p2", name: "Paseo" }]);
    expect(groups.map((group) => group.name)).toEqual(["Multica", "Paseo", "无项目"]);
  });
});

describe("left-over tasks under a closed parent", () => {
  const closedParent = () => ({ identifier: "WORK-1", title: "Parent", closed: true });
  const openParent = { identifier: "WORK-1", title: "Parent", closed: false };

  it.each([
    ["blocked", { status: "blocked" }],
    ["a failed run", { lastRunFailed: true }],
    ["in review awaiting this session", { status: "in_review", contract: contract(SESSION) }],
    ["recently active", { contract: contract(EXECUTOR), lastActivityAt: ago(1) }],
  ] as [string, Partial<BoardIssue>][])("files a %s root under legacy with no to-do", (_name, overrides) => {
    const scopes = ["session", "all"] as const;
    for (const scope of scopes) {
      const root = issue({ parentId: "gone", parent: closedParent(), ...overrides });
      const child = issue({ parentId: root.id, contract: contract(USER) });
      const board = buildBoard(data([root, child], [root.id], { scope }), NOW);
      expect(board.sections.legacy.map((tree) => tree.issue.id)).toEqual([root.id]);
      expect([...board.sections.attention, ...board.sections.active, ...board.sections.parked, ...board.sections.done]).toEqual([]);
      expect(board.todos).toEqual([]);
      expect(pillLabel(data([root, child], [root.id], { scope }), NOW).label).toBe(`${root.identifier} · 无待办`);
    }
  });

  it("keeps the same task where it was while its parent is open or absent", () => {
    for (const parent of [openParent, undefined]) {
      const blocked = issue({ status: "blocked", ...(parent ? { parentId: "p", parent } : {}) });
      const active = issue({ contract: contract(EXECUTOR), ...(parent ? { parentId: "p", parent } : {}) });
      const board = buildBoard(data([blocked, active], [blocked.id, active.id]), NOW);
      expect(board.sections.legacy).toEqual([]);
      expect(board.sections.attention.map((tree) => tree.issue.id)).toEqual([blocked.id]);
      expect(board.sections.active.map((tree) => tree.issue.id)).toEqual([active.id]);
      expect(board.todos.map((todo) => [todo.kind, todo.issue.id])).toEqual([["anomaly", blocked.id]]);
    }
  });

  it("does not treat a closed root or a child under its open board parent as left over", () => {
    const closed = issue({ status: "done", statusCategory: "completed", parentId: "gone", parent: closedParent() });
    const root = issue({ contract: contract(EXECUTOR) });
    const child = issue({ parentId: root.id, parent: openParent, status: "blocked" });
    const board = buildBoard(data([closed, root, child], [closed.id, root.id]), NOW);
    expect(board.sections.legacy).toEqual([]);
    expect(board.sections.done.map((tree) => tree.issue.id)).toEqual([closed.id]);
    expect(board.todos.map((todo) => todo.issue.id)).toEqual([child.id]);
  });

  it("still counts real to-dos next to a left-over tree", () => {
    const legacy = issue({ status: "blocked", parentId: "gone", parent: closedParent() });
    const real = issue({ contract: contract(USER) });
    expect(pillLabel(data([legacy, real], [legacy.id, real.id]), NOW).label).toBe(`${real.identifier} · 待办 1`);
  });
});

describe("composer pill", () => {
  it("names the most urgent tree and counts to-dos on one line", () => {
    const root = issue({ identifier: "WORK-494", status: "backlog" });
    const children = [issue({ parentId: root.id, contract: contract(SESSION) }), issue({ parentId: root.id, status: "blocked" }), issue({ parentId: root.id, contract: contract(USER) })];
    expect(pillLabel(data([root, ...children], [root.id]), NOW)).toMatchObject({ label: "WORK-494 · 待办 3", progress: { done: 0, total: 3 } });
  });

  it("shows explicit no-to-do, empty, disconnected and unavailable states", () => {
    const quiet = issue({ identifier: "WORK-7", contract: contract(EXECUTOR) });
    expect(pillLabel(data([quiet], [quiet.id]), NOW)).toEqual({ label: "WORK-7 · 无待办", title: `WORK-7 ${quiet.title}` });
    expect(pillLabel(data([], []), NOW).label).toBe("Multica · 无任务");
    expect(pillLabel({ state: "disconnected", reason: "no binding", checkedAt: "x" }, NOW)).toEqual({ label: "Multica · 未连接", title: "no binding" });
    expect(pillLabel({ state: "unavailable", reason: "offline", checkedAt: "x" }, NOW).label).toBe("Multica · 不可用");
  });
});

it("keeps every board state parseable by the RPC contract", () => {
  const root = issue({ contract: contract(SESSION), pullRequests: [pr({ ci: "passed" })] });
  expect(boardDataSchema.parse(data([root], [root.id]))).toEqual(data([root], [root.id]));
  expect(boardDataSchema.safeParse({ state: "ready" }).success).toBe(false);
});

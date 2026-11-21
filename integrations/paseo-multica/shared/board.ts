import { z } from "zod";

/**
 * Task board for one Paseo session: a to-do queue plus the task trees it is
 * part of. The server only collects issue facts; every grouping below is a
 * pure function of those facts and the clock, so it never writes anything back.
 */
export const STALE_AFTER_DAYS = 7;
const DAY_MS = 24 * 60 * 60 * 1000;

export const BOARD_SCOPES = ["session", "all"] as const;
export type BoardScope = (typeof BOARD_SCOPES)[number];

const actorSchema = z.object({ type: z.string(), id: z.string(), session_id: z.string().optional() });
export type Actor = z.infer<typeof actorSchema>;

const pullRequestSchema = z.object({
  id: z.string(),
  number: z.number(),
  repo: z.string(),
  title: z.string(),
  state: z.string(),
  url: z.string(),
  ci: z.enum(["passed", "failed", "pending"]).optional(),
  review: z.string().optional(),
  /** The viewer's own reviewer state; absent when the viewer is not a reviewer. */
  myReview: z.string().optional(),
  mergeable: z.string().optional(),
});
export type BoardPullRequest = z.infer<typeof pullRequestSchema>;

/** A Paseo session as the host's `openAgent` takes it: the host's server id and the session's agent id. */
const paseoSessionSchema = z.object({ serverId: z.string(), agentId: z.string() });
export type PaseoSession = z.infer<typeof paseoSessionSchema>;

const issueSchema = z.object({
  id: z.string(),
  identifier: z.string(),
  title: z.string(),
  status: z.string(),
  statusName: z.string().optional(),
  statusCategory: z.string(),
  url: z.string().optional(),
  parentId: z.string().optional(),
  stage: z.number().optional(),
  projectId: z.string().optional(),
  lastActivityAt: z.string().optional(),
  contract: z
    .object({
      pendingActor: actorSchema.optional(),
      acceptor: actorSchema.optional(),
      executor: actorSchema.optional(),
      /** Every criterion of the current version is accepted. */
      accepted: z.boolean(),
      /** Display name of the pending actor when it is neither the viewer nor this session. */
      pendingName: z.string().optional(),
    })
    .optional(),
  /** The issue coordinator; `session_id` is set only from its active session binding on this issue. */
  coordinator: actorSchema.optional(),
  activeRun: z.boolean(),
  lastRunFailed: z.boolean(),
  /** The latest run's failure, when that run failed: its redacted error, when it ended, and the run a rerun repeats. */
  lastFailure: z.object({ summary: z.string(), at: z.string().optional(), taskId: z.string().optional() }).optional(),
  /** When the task last moved to blocked; set only while it is blocked. */
  blockedSince: z.string().optional(),
  /** The parent task as a link target, whether or not the parent is on the board; `closed` when it is done or cancelled. */
  parent: z.object({ identifier: z.string(), title: z.string(), url: z.string().optional(), closed: z.boolean() }).optional(),
  pullRequests: z.array(pullRequestSchema),
  /** The external Paseo session executing this task; absent when execution is managed or the session cannot be opened. */
  executionSession: paseoSessionSchema.optional(),
});
export type BoardIssue = z.infer<typeof issueSchema>;

const viewerSchema = z.object({
  agentId: z.string(),
  sessionId: z.string().optional(),
  userId: z.string().optional(),
  forgeUsernames: z.array(z.string()),
});
export type Viewer = z.infer<typeof viewerSchema>;

const archivedDeliverySchema = z.object({
  id: z.string(),
  issueIdentifier: z.string(),
  issueTitle: z.string(),
  session: paseoSessionSchema,
});

const readyBoardSchema = z.object({
  state: z.literal("ready"),
  scope: z.enum(BOARD_SCOPES),
  viewer: viewerSchema,
  issues: z.array(issueSchema),
  rootIds: z.array(z.string()),
  projects: z.array(z.object({ id: z.string(), name: z.string() })),
  archivedDeliveries: z.array(archivedDeliverySchema).optional(),
  truncated: z.boolean(),
  checkedAt: z.string(),
});
export type ReadyBoardData = z.infer<typeof readyBoardSchema>;

export const boardDataSchema = z.discriminatedUnion("state", [
  readyBoardSchema,
  z.object({ state: z.literal("disconnected"), reason: z.string(), checkedAt: z.string() }),
  z.object({ state: z.literal("unavailable"), reason: z.string(), checkedAt: z.string() }),
]);
export type BoardData = z.infer<typeof boardDataSchema>;

/** `legacy` holds open roots whose parent is closed: left over, not work in flight. */
export type Section = "attention" | "active" | "parked" | "done" | "legacy";
export type TodoKind = "approval" | "merge" | "user" | "acceptance" | "session" | "anomaly";
/** To-dos only the user can clear; every other kind waits on this session. */
const USER_TODOS = new Set<TodoKind>(["approval", "user"]);
export function isUserTodo(kind: TodoKind): boolean {
  return USER_TODOS.has(kind);
}
/** Row markers; the order is the subtree aggregation priority, highest last. */
export const MARKERS = ["idle", "done", "running", "leader", "you", "anomaly"] as const;
export type Marker = (typeof MARKERS)[number];
export const MARKER_GLYPH: Record<Marker, string> = { idle: "○", done: "✓", running: "⋯", leader: "◐", you: "●", anomaly: "▲" };

export interface Classification {
  section: Section;
  todos: TodoKind[];
  marker: Marker;
  parkedReason?: ParkedReason;
}

export type ParkedReason = "backlog" | "stale" | "other";

const OPEN_PR_STATES = new Set(["open", "draft"]);
const TERMINAL_CATEGORIES = new Set(["completed", "cancelled", "canceled"]);

export function isTerminal(issue: Pick<BoardIssue, "status" | "statusCategory">): boolean {
  return TERMINAL_CATEGORIES.has(issue.statusCategory) || issue.status === "done" || issue.status === "cancelled";
}

function isCancelled(issue: Pick<BoardIssue, "status" | "statusCategory">): boolean {
  return issue.statusCategory === "cancelled" || issue.statusCategory === "canceled" || issue.status === "cancelled";
}

function sameActor(a: Actor | undefined, b: Actor | undefined): boolean {
  return !!a && !!b && a.type === b.type && a.id === b.id && (!a.session_id || !b.session_id || a.session_id === b.session_id);
}

export function isSession(actor: Actor | undefined, viewer: Viewer): boolean {
  if (!actor || actor.type !== "agent" || actor.id !== viewer.agentId) return false;
  return !actor.session_id || !viewer.sessionId || actor.session_id === viewer.sessionId;
}

/** Unlike a contract actor, a coordinator counts as this session only through an explicit session binding. */
export function isCoordinatingSession(coordinator: Actor | undefined, viewer: Viewer): boolean {
  return !!coordinator && !!viewer.sessionId && coordinator.type === "agent" && coordinator.id === viewer.agentId && coordinator.session_id === viewer.sessionId;
}

export function isUser(actor: Actor | undefined, viewer: Viewer): boolean {
  return !!actor && !!viewer.userId && (actor.type === "member" || actor.type === "user") && actor.id === viewer.userId;
}

export function isOpenPullRequest(pr: BoardPullRequest): boolean {
  return OPEN_PR_STATES.has(pr.state);
}

/** MRs whose task contract is accepted and that still wait for the viewer's approval. */
export function awaitingApproval(issue: BoardIssue): BoardPullRequest[] {
  if (!issue.contract?.accepted) return [];
  return issue.pullRequests.filter((pr) => isOpenPullRequest(pr) && pr.myReview !== undefined && pr.myReview !== "approved");
}

export function isFresh(lastActivityAt: string | undefined, now: number): boolean {
  if (!lastActivityAt) return false;
  const at = Date.parse(lastActivityAt);
  return Number.isFinite(at) && now - at <= STALE_AFTER_DAYS * DAY_MS;
}

export function classify(issue: BoardIssue, viewer: Viewer, now: number): Classification {
  const approval = awaitingApproval(issue).length > 0;
  // Acceptance and closing are usually one step, so a done task can still wait for MR approval.
  if (isTerminal(issue)) {
    // A closed task whose MR is still open is a loose end: approve it, or merge or close it.
    if (isCancelled(issue) || !issue.pullRequests.some(isOpenPullRequest)) return { section: "done", todos: [], marker: "done" };
    return { section: "attention", todos: [approval ? "approval" : "merge"], marker: "you" };
  }
  const contract = issue.contract;
  const todos: TodoKind[] = approval ? ["approval"] : [];
  if (contract) {
    if (isUser(contract.pendingActor, viewer)) todos.push("user");
    if (isSession(contract.pendingActor, viewer)) todos.push(sameActor(contract.pendingActor, contract.acceptor) ? "acceptance" : "session");
  } else if (issue.status === "in_review" && isCoordinatingSession(issue.coordinator, viewer)) {
    todos.push("acceptance");
  }
  const failedCi = issue.pullRequests.some((pr) => isOpenPullRequest(pr) && pr.ci === "failed");
  if (!issue.activeRun && (issue.status === "blocked" || failedCi || issue.lastRunFailed)) todos.push("anomaly");
  if (todos.length > 0) {
    const marker: Marker = todos.includes("anomaly") ? "anomaly" : todos.some((kind) => kind === "approval" || kind === "user") ? "you" : "leader";
    return { section: "attention", todos, marker };
  }
  const fresh = isFresh(issue.lastActivityAt, now);
  const executorTurn = contract ? sameActor(contract.pendingActor, contract.executor) : true;
  if (issue.activeRun || (executorTurn && issue.status !== "backlog" && fresh)) {
    return { section: "active", todos, marker: "running" };
  }
  return { section: "parked", todos, marker: "idle", parkedReason: issue.status === "backlog" ? "backlog" : fresh ? "other" : "stale" };
}

export interface StageGroup {
  stage?: number;
  nodes: TreeNode[];
  done: number;
  total: number;
}

export interface TreeNode {
  issue: BoardIssue;
  classification: Classification;
  /** This row's marker, raised to the most urgent (⋯ or above) marker in its subtree. */
  marker: Marker;
  section: Section;
  stages: StageGroup[];
  done: number;
  total: number;
}

export interface Todo {
  kind: TodoKind;
  issue: BoardIssue;
  pullRequests: BoardPullRequest[];
}

export interface Board {
  todos: Todo[];
  sections: Record<Section, TreeNode[]>;
  parkedReasons: Record<ParkedReason, number>;
}

const SECTION_ORDER: Section[] = ["attention", "active", "parked", "done"];
const TODO_ORDER: TodoKind[] = ["approval", "user", "acceptance", "session", "merge", "anomaly"];

function rank(marker: Marker): number {
  return MARKERS.indexOf(marker);
}

function activityOf(node: TreeNode): number {
  return Date.parse(node.issue.lastActivityAt ?? "") || 0;
}

function compareNodes(a: TreeNode, b: TreeNode): number {
  return rank(b.marker) - rank(a.marker) || activityOf(b) - activityOf(a);
}

export function buildBoard(data: Pick<ReadyBoardData, "issues" | "rootIds" | "viewer">, now: number): Board {
  const byId = new Map(data.issues.map((issue) => [issue.id, issue]));
  const children = new Map<string, BoardIssue[]>();
  for (const issue of data.issues) {
    if (!issue.parentId || !byId.has(issue.parentId)) continue;
    children.set(issue.parentId, [...(children.get(issue.parentId) ?? []), issue]);
  }

  const placed = new Set<string>();
  function node(issue: BoardIssue): TreeNode {
    placed.add(issue.id);
    const kids = (children.get(issue.id) ?? []).filter((child) => !placed.has(child.id)).map(node);
    const classification = classify(issue, data.viewer, now);
    const stages = groupStages(kids);
    const marker = kids.reduce<Marker>((best, kid) => (rank(kid.marker) >= rank("running") && rank(kid.marker) > rank(best) ? kid.marker : best), classification.marker);
    const sections = new Set([classification.section, ...kids.map((kid) => kid.section)]);
    const section = SECTION_ORDER.find((candidate) => sections.has(candidate) && (candidate !== "done" || sections.size === 1)) ?? "parked";
    const done = kids.filter((kid) => isTerminal(kid.issue)).length;
    return { issue, classification, marker, section, stages, done, total: kids.length };
  }

  // A root that sits inside another root's subtree is shown only there.
  // An open root under a closed parent is left over: its whole subtree is legacy and raises no to-do.
  const rootSet = new Set(data.rootIds.filter((id) => byId.has(id)));
  const nested = (id: string): boolean => {
    const seen = new Set<string>([id]);
    for (let parent = byId.get(id)?.parentId; parent && !seen.has(parent); parent = byId.get(parent)?.parentId) {
      if (rootSet.has(parent)) return true;
      seen.add(parent);
    }
    return false;
  };
  const roots = [...rootSet].filter((id) => !nested(id)).map((id) => {
    const tree = node(byId.get(id)!);
    return isLeftOver(tree.issue) ? { ...tree, section: "legacy" as const } : tree;
  });

  const sections: Record<Section, TreeNode[]> = { attention: [], active: [], parked: [], done: [], legacy: [] };
  for (const root of roots) sections[root.section].push(root);
  for (const list of Object.values(sections)) list.sort(compareNodes);

  const todos: Todo[] = [];
  const parkedReasons: Record<ParkedReason, number> = { backlog: 0, stale: 0, other: 0 };
  const visit = (tree: TreeNode) => {
    const { issue, classification } = tree;
    for (const kind of classification.todos) {
      todos.push({ kind, issue, pullRequests: kind === "approval" ? awaitingApproval(issue) : issue.pullRequests.filter(isOpenPullRequest) });
    }
    for (const stage of tree.stages) stage.nodes.forEach(visit);
  };
  roots.filter((root) => root.section !== "legacy").forEach(visit);
  for (const root of sections.parked) parkedReasons[parkedReasonOf(root) ?? "other"] += 1;
  todos.sort((a, b) => TODO_ORDER.indexOf(a.kind) - TODO_ORDER.indexOf(b.kind) || (Date.parse(b.issue.lastActivityAt ?? "") || 0) - (Date.parse(a.issue.lastActivityAt ?? "") || 0));
  return { todos, sections, parkedReasons };
}

function isLeftOver(issue: BoardIssue): boolean {
  return !!issue.parent?.closed && !isTerminal(issue);
}

function parkedReasonOf(tree: TreeNode): ParkedReason | undefined {
  if (tree.classification.parkedReason) return tree.classification.parkedReason;
  for (const group of tree.stages) {
    for (const child of group.nodes) {
      const reason = parkedReasonOf(child);
      if (reason) return reason;
    }
  }
  return undefined;
}

function groupStages(nodes: TreeNode[]): StageGroup[] {
  const groups = new Map<number | undefined, TreeNode[]>();
  for (const child of nodes) groups.set(child.issue.stage, [...(groups.get(child.issue.stage) ?? []), child]);
  return [...groups.entries()]
    .sort(([a], [b]) => (a ?? -Infinity) - (b ?? -Infinity))
    .map(([stage, list]) => ({
      ...(stage === undefined ? {} : { stage }),
      nodes: list.sort(compareNodes),
      done: list.filter((child) => isTerminal(child.issue)).length,
      total: list.length,
    }));
}

/** The first staged batch that is not complete; undefined when nothing is staged. */
export function currentStage(tree: Pick<TreeNode, "stages">): StageGroup | undefined {
  const staged = tree.stages.filter((group) => group.stage !== undefined);
  return staged.find((group) => group.done < group.total) ?? staged.at(-1);
}

export function groupByProject(nodes: TreeNode[], projects: ReadyBoardData["projects"]): { id?: string; name: string; nodes: TreeNode[] }[] {
  const names = new Map(projects.map((project) => [project.id, project.name]));
  const groups = new Map<string | undefined, TreeNode[]>();
  for (const tree of nodes) groups.set(tree.issue.projectId, [...(groups.get(tree.issue.projectId) ?? []), tree]);
  return [...groups.entries()]
    .map(([id, list]) => ({ ...(id ? { id } : {}), name: (id && names.get(id)) || "无项目", nodes: list }))
    .sort((a, b) => (a.id ? 0 : 1) - (b.id ? 0 : 1) || a.name.localeCompare(b.name));
}

/** The composer pill: one line, e.g. "WORK-494 · 待办 3". */
export interface PillSummary {
  label: string;
  title: string;
  /** Child completion of the named tree; absent when it has no children. */
  progress?: { done: number; total: number };
}

export function pillLabel(data: BoardData, now: number): PillSummary {
  if (data.state === "disconnected") return { label: "Multica · 未连接", title: data.reason };
  if (data.state === "unavailable") return { label: "Multica · 不可用", title: data.reason };
  const board = buildBoard(data, now);
  const focus = [...board.sections.attention, ...board.sections.active, ...board.sections.parked, ...board.sections.legacy][0];
  const archivedCount = data.archivedDeliveries?.length ?? 0;
  if (!focus) {
    return archivedCount ? { label: `Multica · 待办 ${archivedCount}`, title: "投递失败：会话已归档" } : { label: "Multica · 无任务", title: "本会话没有 Multica 任务" };
  }
  const count = new Set(board.todos.map((todo) => `${todo.kind}:${todo.issue.id}`)).size + archivedCount;
  const label = `${focus.issue.identifier} · ${count > 0 ? `待办 ${count}` : "无待办"}`;
  const title = `${focus.issue.identifier} ${focus.issue.title}`;
  return focus.total > 0 ? { label, title, progress: { done: focus.done, total: focus.total } } : { label, title };
}

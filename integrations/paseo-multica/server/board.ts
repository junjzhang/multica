import { z } from "zod";
import { isCoordinatingSession, isSession, isTerminal, type Actor, type BoardIssue, type BoardPullRequest, type BoardScope, type ReadyBoardData, type Viewer } from "../shared/board";
import type { SessionTasksWire } from "../shared/tasks";
import type { ApiCredential, MulticaApi } from "./api";

/** Upper bound on issues per board; a larger tree is cut and marked truncated. */
export const MAX_BOARD_ISSUES = 300;
const CONCURRENCY = 8;
const CHILDREN_BATCH = 200;
// The same unfinished states the server's session-task projection uses.
const ACTIVE_RUN_STATES = new Set(["queued", "dispatched", "running", "waiting_local_directory", "deferred"]);
const OPEN_PR_STATES = new Set(["open", "draft"]);

const actor = z.object({ type: z.string(), id: z.string(), session_id: z.string().optional() });
const issueWire = z.object({
  id: z.string(),
  identifier: z.string(),
  title: z.string(),
  status: z.string(),
  status_name: z.string().nullish(),
  status_category: z.string().nullish(),
  parent_issue_id: z.string().nullish(),
  project_id: z.string().nullish(),
  stage: z.number().nullish(),
  last_activity_at: z.string().nullish(),
  coordinator: actor.nullish(),
  external_session_bindings: z.array(z.object({ agent_id: z.string(), session: z.object({ id: z.string() }).nullish() })).nullish(),
  contract: z
    .object({
      pending_actor: actor.nullish(),
      acceptor: actor.nullish(),
      executor: actor.nullish(),
      accepted: z.array(z.number()).default([]),
      stale: z.boolean().default(false),
      contract: z.object({ versions: z.array(z.object({ criteria: z.array(z.string()) })) }).nullish(),
    })
    .nullish(),
});
type IssueWire = z.infer<typeof issueWire>;
const issueList = z.object({ issues: z.array(issueWire) });
const runList = z.array(z.object({ status: z.string() }));
const reviewer = z.object({ username: z.string(), review_state: z.string() });
const pullRequestWire = z.object({
  id: z.string(),
  number: z.number(),
  title: z.string(),
  state: z.string(),
  repo_name: z.string(),
  html_url: z.string(),
  checks_conclusion: z.string().nullish(),
  mergeable: z.string().nullish(),
  review: z.object({ state: z.string().nullish(), reviewers: z.array(reviewer).nullish() }).nullish(),
});
const pullRequestList = z.object({ pull_requests: z.array(pullRequestWire) });
const pullRequestSummaries = z.object({ pull_requests: z.array(z.object({ issue_id: z.string(), state: z.string() })) });
const projectList = z.object({ projects: z.array(z.object({ id: z.string(), title: z.string() })) });

export interface BoardRequest {
  scope: BoardScope;
  credential: ApiCredential;
  appUrl: string;
  agentId: string;
  sessionTasks: SessionTasksWire;
  forgeUsernames: string[];
  checkedAt: string;
}

export async function loadBoard(api: MulticaApi, request: BoardRequest): Promise<ReadyBoardData> {
  const { credential } = request;
  const read = async <T>(path: string, schema: z.ZodType<T>): Promise<T> => {
    const parsed = schema.safeParse(await api.get(credential, path));
    if (!parsed.success) throw new Error(`Multica returned an unexpected response for ${path.split("?")[0]}.`);
    return parsed.data;
  };
  const [slug, owner] = await Promise.all([
    read(`/api/workspaces/${encodeURIComponent(credential.workspaceId)}`, z.object({ slug: z.string() })).then((workspace) => workspace.slug),
    read(`/api/agents/${encodeURIComponent(request.agentId)}`, z.object({ owner_id: z.string().nullish() })),
  ]);

  const issues = new Map<string, IssueWire>();
  const detailed = new Set<string>();
  let truncated = false;
  const add = (issue: IssueWire): boolean => {
    if (issues.has(issue.id)) return false;
    if (issues.size >= MAX_BOARD_ISSUES) { truncated = true; return false; }
    issues.set(issue.id, issue);
    return true;
  };

  let rootIds: string[];
  if (request.scope === "session") {
    const wire = request.sessionTasks;
    rootIds = [...new Set([...wire.owned, ...wire.accepting, ...(wire.delegated ?? wire.returning)].map((item) => item.issue_id))];
    for (const issue of await mapLimit(rootIds, (id) => read(`/api/issues/${encodeURIComponent(id)}`, issueWire))) {
      if (add(issue)) detailed.add(issue.id);
    }
  } else {
    const open = (await read("/api/issues?open_only=true", issueList)).issues;
    const openIds = new Set(open.map((issue) => issue.id));
    rootIds = open.filter((issue) => !issue.parent_issue_id || !openIds.has(issue.parent_issue_id)).map((issue) => issue.id);
    open.forEach(add);
  }

  // Breadth-first so a truncated board keeps the levels nearest the roots.
  const expanded = new Set<string>();
  for (let level = rootIds.filter((id) => issues.has(id)); level.length > 0;) {
    level.forEach((id) => expanded.add(id));
    const next = new Set<string>();
    for (let start = 0; start < level.length; start += CHILDREN_BATCH) {
      const ids = level.slice(start, start + CHILDREN_BATCH).map(encodeURIComponent).join(",");
      for (const child of (await read(`/api/issues/children?parent_ids=${ids}`, issueList)).issues) {
        add(child);
        if (issues.has(child.id) && !expanded.has(child.id)) next.add(child.id);
      }
    }
    level = [...next];
  }

  const summaries = (await read("/api/issues/pull-requests", pullRequestSummaries)).pull_requests;
  const linked = new Set(summaries.map((pr) => pr.issue_id));
  const linkedOpen = new Set(summaries.filter((pr) => OPEN_PR_STATES.has(pr.state)).map((pr) => pr.issue_id));
  const viewer: Viewer = {
    agentId: request.agentId,
    ...(request.sessionTasks.session_id ? { sessionId: request.sessionTasks.session_id } : {}),
    ...(owner.owner_id ? { userId: owner.owner_id } : {}),
    forgeUsernames: request.forgeUsernames,
  };

  // Neither root source lists closed tasks, but a closed task can still have an MR waiting for
  // approval. Add those as roots: all of them for 全部, this session's own for 本会话.
  const closedWithOpenMr = [...linkedOpen].filter((id) => !issues.has(id));
  for (const issue of await mapLimit(closedWithOpenMr, (id) => read(`/api/issues/${encodeURIComponent(id)}`, issueWire))) {
    const mine = isSession(issue.contract?.acceptor ?? undefined, viewer) || isSession(issue.contract?.executor ?? undefined, viewer) || isCoordinatingSession(coordinatorOf(issue), viewer);
    if (!isTerminal({ status: issue.status, statusCategory: issue.status_category ?? "" }) || (request.scope === "session" && !mine)) continue;
    if (add(issue)) {
      detailed.add(issue.id);
      rootIds.push(issue.id);
    }
  }

  const board = await mapLimit([...issues.values()], async (base): Promise<BoardIssue> => {
    const open = !isTerminal({ status: base.status, statusCategory: base.status_category ?? "" });
    const path = `/api/issues/${encodeURIComponent(base.id)}`;
    const [full, runs, prs] = await Promise.all([
      (open || linkedOpen.has(base.id)) && !detailed.has(base.id) ? read(path, issueWire) : base,
      open ? read(`${path}/task-runs`, runList) : [],
      linked.has(base.id) ? read(`${path}/pull-requests`, pullRequestList).then((list) => list.pull_requests) : [],
    ]);
    return toBoardIssue(full, runs, prs, viewer, `${request.appUrl.replace(/\/+$/, "")}/${slug}/issues/`);
  });

  const projects = request.scope === "all" ? (await read("/api/projects?limit=200", projectList)).projects.map((project) => ({ id: project.id, name: project.title })) : [];
  return { state: "ready", scope: request.scope, viewer, issues: board, rootIds, projects, truncated, checkedAt: request.checkedAt };
}

function toBoardIssue(issue: IssueWire, runs: z.infer<typeof runList>, prs: z.infer<typeof pullRequestWire>[], viewer: Viewer, issueBase: string): BoardIssue {
  const contract = issue.contract;
  const criteria = contract?.contract?.versions.at(-1)?.criteria.length ?? 0;
  return {
    id: issue.id,
    identifier: issue.identifier,
    title: issue.title,
    status: issue.status,
    ...(issue.status_name ? { statusName: issue.status_name } : {}),
    statusCategory: issue.status_category ?? "",
    ...(httpURL(issueBase) ? { url: `${issueBase}${encodeURIComponent(issue.identifier)}` } : {}),
    ...(issue.parent_issue_id ? { parentId: issue.parent_issue_id } : {}),
    ...(typeof issue.stage === "number" ? { stage: issue.stage } : {}),
    ...(issue.project_id ? { projectId: issue.project_id } : {}),
    ...(issue.last_activity_at ? { lastActivityAt: issue.last_activity_at } : {}),
    ...(coordinatorOf(issue) ? { coordinator: coordinatorOf(issue) } : {}),
    ...(contract?.contract?.versions.length
      ? {
          contract: {
            ...(contract.pending_actor ? { pendingActor: contract.pending_actor } : {}),
            ...(contract.acceptor ? { acceptor: contract.acceptor } : {}),
            ...(contract.executor ? { executor: contract.executor } : {}),
            accepted: !contract.stale && criteria > 0 && contract.accepted.length >= criteria,
          },
        }
      : {}),
    activeRun: runs.some((run) => ACTIVE_RUN_STATES.has(run.status)),
    lastRunFailed: runs[0]?.status === "failed",
    pullRequests: prs.filter((pr) => httpURL(pr.html_url)).map((pr) => toPullRequest(pr, viewer)),
  };
}

function coordinatorOf(issue: IssueWire): Actor | undefined {
  const coordinator = issue.coordinator;
  if (!coordinator) return undefined;
  const session = issue.external_session_bindings?.find((binding) => binding.agent_id === coordinator.id)?.session?.id;
  return { type: coordinator.type, id: coordinator.id, ...(coordinator.type === "agent" && session ? { session_id: session } : {}) };
}

function toPullRequest(pr: z.infer<typeof pullRequestWire>, viewer: Viewer): BoardPullRequest {
  const ci = pr.checks_conclusion;
  const mine = pr.review?.reviewers?.find((entry) => viewer.forgeUsernames.includes(entry.username));
  return {
    id: pr.id,
    number: pr.number,
    repo: pr.repo_name,
    title: pr.title,
    state: pr.state,
    url: pr.html_url,
    ...(ci === "passed" || ci === "failed" || ci === "pending" ? { ci } : {}),
    ...(pr.review?.state ? { review: pr.review.state } : {}),
    ...(mine ? { myReview: mine.review_state } : {}),
    ...(pr.mergeable ? { mergeable: pr.mergeable } : {}),
  };
}

function httpURL(value: string): boolean {
  try { return /^https?:$/.test(new URL(value).protocol); } catch { return false; }
}

async function mapLimit<T, R>(items: T[], task: (item: T) => Promise<R>): Promise<R[]> {
  const results: R[] = new Array(items.length);
  let next = 0;
  const worker = async () => {
    while (next < items.length) {
      const index = next++;
      results[index] = await task(items[index]!);
    }
  };
  await Promise.all(Array.from({ length: Math.min(CONCURRENCY, items.length) }, worker));
  return results;
}

import type { PluginAgentPanelProps } from "@getpaseo/plugin/client";
import { useRpc } from "@getpaseo/plugin/client";
import { Icon, Loading } from "@getpaseo/plugin/client/ui";
import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { ActivityIndicator, Linking, Pressable, Text, View } from "react-native";
import {
  awaitingApproval,
  buildBoard,
  currentStage,
  groupByProject,
  isFresh,
  isOpenPullRequest,
  isSession,
  isUser,
  isUserTodo,
  type BoardData,
  type BoardIssue,
  type BoardPullRequest,
  type BoardScope,
  type PaseoSession,
  type StageGroup,
  type Todo,
  type TodoKind,
  type TreeNode,
  type Viewer,
} from "../shared/board";
import { sessionBoard, taskAction, taskExpansion } from "../shared/contracts";
import { quickActions, type QuickAction, type TaskAction, type TaskActionResult } from "../shared/task-actions";
import { STATUS_LABEL, STATUS_SVG, statusKey, type StatusKey } from "./status-icon";

type Colors = PluginAgentPanelProps["theme"]["colors"];
type OpenTask = (url: string, identifier?: string) => void;
type OpenAgent = NonNullable<PluginAgentPanelProps["navigation"]>["openAgent"];
type Props = Pick<PluginAgentPanelProps, "agentId" | "theme" | "host" | "navigation"> & { openTask: OpenTask };

// Paseo's theme ramp: base 14 / sm 12, spacing 4·6·8·12·16·24.
const FONT = { base: 14, sm: 12 } as const;
const SPACE = { xs: 4, sm: 6, md: 8, lg: 12, xl: 16, xxl: 24 } as const;
const RADIUS = 6;
const GUTTER = 16;
const INDENT = 16;
// Every row is at least this tall, so a rail can place its node or tick at the row's middle.
const ROW_H = 32;
const NODE = 8;

const SCOPES: { value: BoardScope; label: string }[] = [{ value: "session", label: "本会话" }, { value: "all", label: "全部" }];

/** The muted phrase at a row's end, with an optional tone for the words the user must act on. */
type Phrase = { text: string; tone?: "action" | "danger" };

/** `feat(scope): title` keeps only `title`; the prefix repeats what the tree already says. */
export function displayTitle(title: string): string {
  return title.replace(/^[a-z]+(\([^)]*\))?!?:\s*/, "") || title;
}

function prRef(pr: BoardPullRequest): string {
  return `${pr.repo}${/github\.com/i.test(pr.url) ? "#" : "!"}${pr.number}`;
}

function prNumber(pr: BoardPullRequest): string {
  return `${/github\.com/i.test(pr.url) ? "#" : "!"}${pr.number}`;
}

function elapsed(value: string | undefined, now: number): string {
  const ms = now - Date.parse(value ?? "");
  if (!Number.isFinite(ms)) return "";
  const minutes = Math.max(0, Math.floor(ms / 60000));
  return minutes < 60 ? `${minutes}m` : minutes < 1440 ? `${Math.floor(minutes / 60)}h` : `${Math.floor(minutes / 1440)}d`;
}

/** A failure is red only while fresh; an old one is history, not an emergency. */
function anomalyPhrase(issue: BoardIssue, now: number): Phrase {
  const since = elapsed(issue.lastActivityAt, now);
  const failed = failedCi(issue);
  const text = failed.length > 0 ? stateOf(failed, "CI 失败") : issue.lastRunFailed ? "运行失败" : "阻塞";
  const urgent = (failed.length > 0 || issue.lastRunFailed) && isFresh(issue.lastActivityAt, now);
  return { text: since ? `${text} · ${since}` : text, ...(urgent ? { tone: "danger" as const } : {}) };
}

function failedCi(issue: BoardIssue): BoardPullRequest[] {
  return issue.pullRequests.filter((pr) => isOpenPullRequest(pr) && pr.ci === "failed");
}

/** The first MR's number and state, then how many more follow, whatever their state: `!7 CI 失败 +1`. */
function stateOf(prs: BoardPullRequest[], state: string): string {
  return `${prNumber(prs[0]!)} ${state}${prs.length > 1 ? ` +${prs.length - 1}` : ""}`;
}

function isConflicting(pr: BoardPullRequest): boolean {
  return pr.mergeable?.toLowerCase() === "conflicting";
}

/** A closed task's open MRs, most urgent first: what blocks them, else that they are still unmerged. */
function mergePhrase(prs: BoardPullRequest[]): Phrase {
  const pr = prs[0];
  if (!pr) return { text: "MR 未合并" };
  if (pr.ci === "failed") return { text: stateOf(prs, "CI 失败"), tone: "danger" };
  if (isConflicting(pr)) return { text: stateOf(prs, "有冲突"), tone: "danger" };
  return { text: stateOf(prs, "未合并") };
}

/** The MRs a to-do is about, most urgent first; empty when the to-do is not about an MR. */
export function todoPullRequests(todo: Todo): BoardPullRequest[] {
  switch (todo.kind) {
    case "approval":
    case "merge":
      return byUrgency(todo.pullRequests.map((pr) => ({ pr, issue: todo.issue }))).map((entry) => entry.pr);
    case "anomaly":
      return failedCi(todo.issue);
    default:
      return [];
  }
}

export function todoPhrase(todo: Todo, now: number): Phrase {
  switch (todo.kind) {
    case "approval": {
      const prs = todoPullRequests(todo);
      return { text: prs.length > 0 ? stateOf(prs, "待你审批") : "待你审批", tone: "action" };
    }
    case "merge":
      return mergePhrase(todoPullRequests(todo));
    case "user":
      return { text: "待你处理", tone: "action" };
    case "acceptance":
      return { text: "待验收" };
    case "session":
      return { text: "待本会话" };
    case "anomaly":
      return anomalyPhrase(todo.issue, now);
  }
}

export function treePhrase(tree: TreeNode): Phrase | undefined {
  if (tree.total > 0) {
    const staged = tree.stages.filter((group) => group.stage !== undefined);
    const stage = currentStage(tree);
    if (stage && staged.length > 1) return { text: `第 ${staged.indexOf(stage) + 1}/${staged.length} 批` };
    // A single child says nothing a count can add; its own row is the progress.
    return tree.total > 1 ? { text: `${tree.done}/${tree.total}` } : undefined;
  }
  return undefined;
}

type Tone = Phrase["tone"];
const TONE_RANK: Record<NonNullable<Tone> | "none", number> = { none: 0, action: 1, danger: 2 };

/** An MR with the task it belongs to; a parent's chip lists MRs from its whole subtree. */
export type PullRequestEntry = { pr: BoardPullRequest; issue: BoardIssue };

/** A clickable MR mark: one MR opens directly, several expand a list under the row. */
export type PullRequestChip = { icon: string; text: string; tone?: Tone; label: string; entries: PullRequestEntry[] };

function isMerged(pr: BoardPullRequest): boolean {
  return pr.state === "merged";
}

function prIcon(prs: BoardPullRequest[]): string {
  return prs.every(isMerged) ? "GitMerge" : prs.every((pr) => pr.state === "draft") ? "GitPullRequestDraft" : "GitPullRequest";
}

function prTone(entry: PullRequestEntry): Tone {
  return prDetail(entry.pr, entry.issue)?.tone;
}

/** Most urgent first; ties keep their order. */
function byUrgency(entries: PullRequestEntry[]): PullRequestEntry[] {
  return [...entries].sort((a, b) => TONE_RANK[prTone(b) ?? "none"] - TONE_RANK[prTone(a) ?? "none"]);
}

/** Open MRs anywhere in a tree, the task's own first, then its subtree in board order. */
function openIn(tree: TreeNode): PullRequestEntry[] {
  const own = tree.issue.pullRequests.filter(isOpenPullRequest).map((pr) => ({ pr, issue: tree.issue }));
  return [...own, ...tree.stages.flatMap((group) => group.nodes.flatMap(openIn))];
}

/**
 * Each open MR of a tree once. An MR can link to several tasks, often a
 * subtask and the parent its description mentions; the task last in board
 * order keeps it, so a descendant always wins over its own ancestors.
 */
export function openMergeRequestsIn(tree: TreeNode): PullRequestEntry[] {
  return [...new Map(openIn(tree).map((entry) => [entry.pr.id, entry])).values()];
}

/** A tree row's chip: the open MRs of its whole subtree, colored by the most urgent one; a single MR also names its state. */
export function treeChip(tree: TreeNode): PullRequestChip | undefined {
  const entries = byUrgency(openMergeRequestsIn(tree));
  if (entries.length === 0) return undefined;
  const prs = entries.map((entry) => entry.pr);
  const tone = prTone(entries[0]!);
  const detail = prs.length === 1 ? prDetail(entries[0]!.pr, entries[0]!.issue) : undefined;
  const text = prs.length > 1 ? `${prs.length}` : detail ? `${prNumber(prs[0]!)} ${detail.text}` : prNumber(prs[0]!);
  const label = detail ? `${prRef(prs[0]!)} ${detail.text}` : prs.map(prRef).join(" ");
  return { icon: prIcon(prs), text, ...(tone ? { tone } : {}), label, entries };
}

/** A to-do row's chip: its MR state phrase, opening the MRs it names. */
export function todoChip(todo: Todo, now: number): PullRequestChip | undefined {
  const prs = todoPullRequests(todo);
  if (prs.length === 0) return undefined;
  const phrase = todoPhrase(todo, now);
  const entries = prs.map((pr) => ({ pr, issue: todo.issue }));
  return { icon: prIcon(prs), text: phrase.text, ...(phrase.tone ? { tone: phrase.tone } : {}), label: `${prs.map(prRef).join(" ")} ${phrase.text}`, entries };
}

/** A task whose subtree has no open MR keeps its merged MRs as a quiet, unclickable mark. */
export function mergedMark(issue: BoardIssue): { text: string; label: string } | undefined {
  const merged = issue.pullRequests.filter(isMerged);
  return merged.length > 0 ? { text: merged.length === 1 ? prNumber(merged[0]!) : `${merged.length}`, label: `已合并 ${merged.map(prRef).join(" ")}` } : undefined;
}

/** What a finished part of a tree delivered: the merged MRs anywhere in it. */
export function mergedIn(trees: TreeNode[]): BoardPullRequest[] {
  return trees.flatMap((tree) => [...tree.issue.pullRequests.filter(isMerged), ...mergedIn(tree.stages.flatMap((group) => group.nodes))]);
}

function prDetail(pr: BoardPullRequest, issue: BoardIssue): Phrase | undefined {
  if (isMerged(pr)) return { text: "已合并" };
  if (pr.ci === "failed") return { text: "CI 失败", tone: "danger" };
  if (awaitingApproval(issue).includes(pr)) return { text: "待你审批", tone: "action" };
  if (isConflicting(pr)) return { text: "有冲突", tone: "danger" };
  if (pr.ci === "pending") return { text: "CI 运行中" };
  if (pr.ci === "passed") return { text: "CI 通过" };
  return pr.state === "draft" ? { text: "草稿" } : undefined;
}

/** One row per issue; an issue with several to-dos shows its most urgent one, already first in board order. */
function firstPerIssue(todos: Todo[]): Todo[] {
  const seen = new Set<string>();
  return todos.filter((todo) => !seen.has(todo.issue.id) && !!seen.add(todo.issue.id));
}

/** The session to jump to from a task row: its executing Paseo session, unless that is the session showing the board. */
export function sessionTarget(issue: BoardIssue, here: PaseoSession): PaseoSession | undefined {
  const session = issue.executionSession;
  return session && !(session.serverId === here.serverId && session.agentId === here.agentId) ? session : undefined;
}

const NO_VIEWER: Viewer = { agentId: "", forgeUsernames: [] };

function nodesOf(tree: TreeNode): TreeNode[] {
  return [tree, ...tree.stages.flatMap((group) => group.nodes.flatMap(nodesOf))];
}

export function BoardSection(props: Props) {
  return <BoardContent key={props.agentId} {...props} />;
}

function BoardContent({ agentId, theme, host, navigation, openTask }: Props) {
  const getBoard = useRpc(sessionBoard);
  const expansion = useRpc(taskExpansion);
  const runAction = useRpc(taskAction);
  const [scope, setScope] = useState<BoardScope>("session");
  const [data, setData] = useState<BoardData | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [expandedIds, setExpandedIds] = useState<string[]>([]);
  const [expansionError, setExpansionError] = useState("");
  const [parkedOpen, setParkedOpen] = useState(false);
  const writes = useRef<Promise<unknown>>(Promise.resolve());
  const request = useRef(0);

  const load = useCallback(async (refresh: boolean) => {
    const revision = ++request.current;
    setLoading(true);
    setError("");
    try {
      const next = await getBoard({ agentId, scope, refresh });
      if (revision === request.current) setData(next);
    } catch (cause) {
      if (revision === request.current) setError(cause instanceof Error ? cause.message : String(cause));
    } finally {
      if (revision === request.current) setLoading(false);
    }
  }, [agentId, scope, getBoard]);
  useEffect(() => { void load(false); }, [load]);
  useEffect(() => {
    let active = true;
    void expansion({ agentId }).then((ids) => { if (active) setExpandedIds(ids); }, (cause: unknown) => { if (active) setExpansionError(String(cause)); });
    return () => { active = false; };
  }, [agentId, expansion]);
  const toggle = useCallback((issueId: string, expanded: boolean) => {
    setExpandedIds((ids) => expanded ? [...ids.filter((id) => id !== issueId), issueId] : ids.filter((id) => id !== issueId));
    writes.current = writes.current.catch(() => {}).then(() => expansion({ agentId, issueId, expanded })).then(() => setExpansionError(""), (cause: unknown) => setExpansionError(String(cause)));
  }, [agentId, expansion]);

  const colors = theme.colors;
  const ready = data?.state === "ready" ? data : null;
  const board = useMemo(() => (ready ? buildBoard(ready, Date.now()) : null), [ready]);
  const now = Date.now();
  const openAgent = navigation?.openAgent;
  const here = { serverId: host.id, agentId };
  const openSession = (issue: BoardIssue) => {
    const target = sessionTarget(issue, here);
    return target && openAgent ? () => openAgent(target) : undefined;
  };
  const act = useCallback(async (issue: BoardIssue, action: TaskAction) => {
    const result = await runAction({ agentId, issueId: issue.id, action });
    if (result.ok) await load(true);
    return result;
  }, [agentId, runAction, load]);
  const trees = useMemo(() => new Map(board ? Object.values(board.sections).flat().flatMap(nodesOf).map((tree) => [tree.issue.id, tree] as const) : []), [board]);
  const row: RowContext = { colors, openTask, openSession, expandedIds, toggle, viewer: ready?.viewer ?? NO_VIEWER, now, trees, act };
  // Still one row per issue: its most urgent to-do decides which block it appears in.
  const todos = board ? firstPerIssue(board.todos) : [];
  const yours = todos.filter((todo) => isUserTodo(todo.kind));
  const sessions = todos.filter((todo) => !isUserTodo(todo.kind));
  const archivedDeliveries = ready?.archivedDeliveries ?? [];
  const todoIds = new Set([...yours, ...sessions].map((todo) => todo.issue.id));
  // A childless tree that needs you is fully described by its to-do row.
  const active = board ? [...board.sections.attention, ...board.sections.active].filter((tree) => tree.total > 0 || !todoIds.has(tree.issue.id)) : [];
  const empty = board && todoIds.size === 0 && archivedDeliveries.length === 0 && active.length === 0 && board.sections.parked.length === 0;

  return <View style={{ gap: SPACE.lg }}>
    <View style={{ flexDirection: "row", alignItems: "center", justifyContent: "space-between", paddingBottom: SPACE.md, borderBottomWidth: 1, borderBottomColor: colors.border }}>
      <View accessibilityRole="tablist" style={{ flexDirection: "row", gap: SPACE.xs }}>
        {SCOPES.map((option) => <Segment key={option.value} label={option.label} selected={scope === option.value} colors={colors} onPress={() => setScope(option.value)} />)}
      </View>
      <IconButton name="RefreshCw" label="刷新" disabled={loading} colors={colors} onPress={() => void load(true)} />
    </View>
    {expansionError ? <Text style={{ color: colors.statusDanger, fontSize: FONT.sm }}>展开状态未保存：{expansionError}</Text> : null}
    {error ? <Text style={{ color: colors.statusDanger, fontSize: FONT.sm }}>{error}</Text>
      : data && data.state !== "ready" ? <Notice colors={colors} title={data.state === "disconnected" ? "未连接 Multica" : "Multica 暂不可用"} detail={data.reason} />
      : !board || !ready ? <Loading />
      : empty ? <Notice colors={colors} title="暂无任务" />
      : <View style={{ gap: SPACE.xl }}>
        {yours.length > 0 || archivedDeliveries.length > 0 ? <Section title="需要你" colors={colors}>
          {archivedDeliveries.map((delivery) => <ArchivedDeliveryRow key={delivery.id} delivery={delivery} colors={colors} openAgent={openAgent} />)}
          {yours.map((todo) => <TaskRow key={todo.issue.id} data={todoRow(todo, now)} depth={0} row={row} />)}
        </Section> : null}
        {sessions.length > 0 ? <Section title="待本会话处理" colors={colors}>
          {sessions.map((todo) => <TaskRow key={todo.issue.id} data={todoRow(todo, now)} depth={0} row={row} />)}
        </Section> : null}
        {active.length === 0 ? null
          : ready.scope === "all" ? <View style={{ gap: 2 }}>{groupByProject(active, ready.projects).map((group) => <Folder key={group.id ?? group.name} name={group.name} nodes={group.nodes} row={row} />)}</View>
          : <Section title="进行中" colors={colors}>{active.map((tree) => <TaskRow key={tree.issue.id} data={treeRow(tree)} depth={0} row={row} />)}</Section>}
        {board.sections.parked.length > 0 ? <Section title="搁置" count={board.sections.parked.length} colors={colors} open={parkedOpen} onToggle={() => setParkedOpen(!parkedOpen)}>
          {board.sections.parked.map((tree) => <TaskRow key={tree.issue.id} data={treeRow(tree)} depth={0} row={row} />)}
        </Section> : null}
        {ready.truncated ? <Text style={{ color: colors.foregroundMuted, fontSize: FONT.sm, paddingLeft: GUTTER + SPACE.sm }}>部分子任务未显示</Text> : null}
      </View>}
  </View>;
}

type ArchivedDelivery = NonNullable<Extract<BoardData, { state: "ready" }>["archivedDeliveries"]>[number];

/** A delivery that failed because its session is archived; the archived session still opens in Paseo. */
function ArchivedDeliveryRow({ delivery, colors, openAgent }: { delivery: ArchivedDelivery; colors: Colors; openAgent?: OpenAgent }) {
  const summary = `${delivery.issueIdentifier} ${delivery.issueTitle}`;
  return <Pressable
    accessibilityRole={openAgent ? "link" : "text"}
    accessibilityLabel={`${summary}，投递失败：会话已归档${openAgent ? "，在 Paseo 中打开" : ""}`}
    disabled={!openAgent} onPress={openAgent ? () => openAgent(delivery.session) : undefined}
    style={{ paddingVertical: SPACE.xs, paddingLeft: SPACE.sm }}
  >
    <View style={{ gap: SPACE.xs, alignItems: "flex-start" }}>
      <Text style={{ color: colors.foreground }} numberOfLines={1}>{summary}</Text>
      <Text style={{ color: colors.foregroundMuted }}>投递失败：会话已归档{openAgent ? " · 在 Paseo 中打开" : ""}</Text>
    </View>
  </Pressable>;
}

function Notice({ colors, title, detail }: { colors: Colors; title: string; detail?: string }) {
  return <View style={{ gap: SPACE.xs, paddingVertical: SPACE.xl }}>
    <Text style={{ color: colors.foregroundMuted, fontSize: FONT.base }}>{title}</Text>
    {detail ? <Text style={{ color: colors.foregroundMuted, fontSize: FONT.sm }}>{detail}</Text> : null}
  </View>;
}

function useHover() {
  const [hovered, setHovered] = useState(false);
  return { hovered, onHoverIn: () => setHovered(true), onHoverOut: () => setHovered(false) };
}

function Segment({ label, selected, colors, onPress }: { label: string; selected: boolean; colors: Colors; onPress(): void }) {
  const hover = useHover();
  return <Pressable
    accessibilityRole="tab" accessibilityLabel={label} accessibilityState={{ selected }} onPress={onPress}
    onHoverIn={hover.onHoverIn} onHoverOut={hover.onHoverOut}
    style={{ height: 28, justifyContent: "center", paddingHorizontal: SPACE.lg, borderRadius: 999, backgroundColor: selected || hover.hovered ? colors.surface2 : "transparent" }}
  >
    <Text style={{ fontSize: FONT.sm, fontWeight: "500", color: selected ? colors.foreground : colors.foregroundMuted }}>{label}</Text>
  </Pressable>;
}

function IconButton({ name, label, disabled, colors, onPress }: { name: string; label: string; disabled?: boolean; colors: Colors; onPress(): void }) {
  const hover = useHover();
  return <Pressable
    accessibilityRole="button" accessibilityLabel={label} disabled={disabled} onPress={onPress}
    onHoverIn={hover.onHoverIn} onHoverOut={hover.onHoverOut}
    style={{ width: 28, height: 28, alignItems: "center", justifyContent: "center", borderRadius: 999, backgroundColor: hover.hovered && !disabled ? colors.surface2 : "transparent", opacity: disabled ? 0.5 : 1 }}
  >
    <Icon name={name} size={16} color={hover.hovered ? colors.foreground : colors.foregroundMuted} />
  </Pressable>;
}

function Section({ title, count, colors, open = true, onToggle, children }: { title: string; count?: number; colors: Colors; open?: boolean; onToggle?(): void; children: ReactNode }) {
  const heading = <View style={{ flexDirection: "row", alignItems: "center", gap: SPACE.sm, paddingVertical: SPACE.xs, paddingLeft: GUTTER + SPACE.sm, paddingRight: SPACE.sm }}>
    <Text style={{ color: colors.foreground, fontSize: FONT.sm, fontWeight: "600" }}>{title}</Text>
    {count !== undefined ? <Text style={{ color: colors.foregroundMuted, fontSize: FONT.sm }}>{count}</Text> : null}
    {onToggle ? <View style={{ marginLeft: "auto", transform: [{ rotate: open ? "90deg" : "0deg" }] }}><Icon name="ChevronRight" size={14} color={colors.foregroundMuted} /></View> : null}
  </View>;
  return <View style={{ gap: 2 }}>
    {onToggle ? <Pressable accessibilityRole="button" accessibilityLabel={title} accessibilityState={{ expanded: open }} onPress={onToggle}>{heading}</Pressable> : heading}
    {open ? children : null}
  </View>;
}

type RowContext = {
  colors: Colors; openTask: OpenTask; expandedIds: string[]; toggle(id: string, expanded: boolean): void;
  viewer: Viewer; now: number;
  /** Every tree node on the board by issue id, so a to-do row expands to the same subtree as its tree row. */
  trees: Map<string, TreeNode>;
  act(issue: BoardIssue, action: TaskAction): Promise<TaskActionResult>;
  /** Opens the task's executing session; undefined when the row has no session to open. */
  openSession(issue: BoardIssue): (() => void) | undefined;
};

/**
 * One line: disclosure gutter, title, then the muted phrase. Hover reveals the
 * issue number in front of the phrase instead of spending a second line on it.
 */
function toneColor(tone: Tone, colors: Colors): string {
  return tone === "danger" ? colors.statusDanger : tone === "action" ? colors.statusWarning : colors.foregroundMuted;
}

/** Multica's status hues, mapped onto the host theme: started work is amber, review green, blocked red, the rest quiet. */
function statusColor(status: StatusKey, colors: Colors): string {
  return status === "in_progress" ? colors.statusWarning : status === "in_review" ? colors.statusSuccess : status === "blocked" ? colors.statusDanger : colors.foregroundMuted;
}

function Row({ issue, phrase, depth, row, running, chip, mark, onPress, disclosure }: {
  issue: BoardIssue; phrase?: Phrase; depth: number; row: RowContext; running?: boolean; chip?: PullRequestChip; mark?: { text: string; label: string }; onPress?(): void;
  disclosure?: { expanded: boolean; onToggle(): void };
}) {
  const { colors } = row;
  const hover = useHover();
  const [pressed, setPressed] = useState(false);
  const [listOpen, setListOpen] = useState(false);
  const tone = toneColor(phrase?.tone, colors);
  const title = displayTitle(issue.title);
  const status = statusKey(issue);
  const openSession = row.openSession(issue);
  return <View>
    <View style={{ flexDirection: "row", alignItems: "center", paddingLeft: INDENT * depth, borderRadius: RADIUS, backgroundColor: pressed ? colors.surface2 : hover.hovered ? colors.surface1 : "transparent" }}>
      <Pressable
        accessibilityRole="button" accessibilityLabel={`${disclosure?.expanded ? "收起" : "展开"} ${issue.identifier}`} accessibilityState={{ expanded: !!disclosure?.expanded }}
        disabled={!disclosure} onPress={disclosure?.onToggle} hitSlop={SPACE.xs}
        style={{ width: GUTTER, alignSelf: "stretch", alignItems: "center", justifyContent: "center" }}
      >
        {disclosure ? <View style={{ transform: [{ rotate: disclosure.expanded ? "90deg" : "0deg" }] }}><Icon name="ChevronRight" size={12} color={colors.foregroundMuted} /></View> : null}
      </Pressable>
      <Pressable
        accessibilityRole="link" accessibilityLabel={`${issue.identifier} ${STATUS_LABEL[status]} ${title}${mark ? ` ${mark.label}` : ""}${phrase ? ` ${phrase.text}` : ""}`}
        disabled={!onPress} onPress={onPress}
        onHoverIn={hover.onHoverIn} onHoverOut={hover.onHoverOut} onPressIn={() => setPressed(true)} onPressOut={() => setPressed(false)}
        style={{ flex: 1, flexDirection: "row", alignItems: "center", gap: SPACE.md, minHeight: ROW_H, paddingVertical: SPACE.sm, paddingLeft: SPACE.sm, paddingRight: SPACE.sm }}
      >
        <Icon icon={{ svg: STATUS_SVG[status] }} size={14} color={statusColor(status, colors)} />
        <Text numberOfLines={1} style={{ flex: 1, color: colors.foreground, fontSize: FONT.base }}>{title}</Text>
        {running ? <ActivityIndicator size={10} color={colors.foregroundMuted} /> : null}
        {hover.hovered ? <Text style={{ color: colors.foregroundMuted, fontSize: FONT.sm }}>{issue.identifier}</Text> : null}
        {mark ? <View style={{ flexDirection: "row", alignItems: "center", gap: 2 }}>
          <Icon name="GitMerge" size={13} color={colors.foregroundMuted} />
          <Text style={{ color: colors.foregroundMuted, fontSize: FONT.sm }}>{mark.text}</Text>
        </View> : null}
        {phrase ? <Text numberOfLines={1} style={{ color: tone, fontSize: FONT.sm, fontWeight: phrase.tone ? "500" : "400" }}>{phrase.text}</Text> : null}
      </Pressable>
      {chip ? <Chip chip={chip} colors={colors} open={listOpen} onToggle={() => setListOpen(!listOpen)} /> : null}
      {openSession ? <SessionButton identifier={issue.identifier} colors={colors} onPress={openSession} /> : null}
    </View>
    {chip && listOpen && chip.entries.length > 1 ? chip.entries.map((entry) => <PullRequestRow key={entry.pr.id} pr={entry.pr} issue={entry.issue} source={entry.issue.id === issue.id ? undefined : entry.issue} depth={depth + 1} row={row} />) : null}
  </View>;
}

/**
 * The MR target at a row's end, apart from the row that opens the task: an
 * outlined chip that fills on hover and ends in an outbound arrow for one MR,
 * or a chevron when it expands the list of several.
 */
function Chip({ chip, colors, open, onToggle }: { chip: PullRequestChip; colors: Colors; open: boolean; onToggle(): void }) {
  const hover = useHover();
  const single = chip.entries.length === 1 ? chip.entries[0]!.pr : undefined;
  const color = toneColor(chip.tone, colors);
  const label = single ? `打开 ${chip.label}` : `${chip.entries.length} 个 MR ${chip.label}`;
  return <Pressable
    accessibilityRole={single ? "link" : "button"} accessibilityLabel={label} accessibilityState={single ? undefined : { expanded: open }}
    onPress={single ? () => void Linking.openURL(single.url) : onToggle}
    onHoverIn={hover.onHoverIn} onHoverOut={hover.onHoverOut}
    style={{
      flexDirection: "row", alignItems: "center", gap: SPACE.xs, flexShrink: 0, height: 22, marginRight: SPACE.xs, paddingHorizontal: SPACE.sm,
      borderRadius: RADIUS, borderWidth: 1, borderColor: chip.tone || hover.hovered ? color : colors.border,
      backgroundColor: hover.hovered || open ? colors.surface2 : colors.surface1, cursor: "pointer",
    }}
  >
    <Icon name={chip.icon} size={12} color={color} />
    <Text numberOfLines={1} style={{ color, fontSize: FONT.sm, fontWeight: "500", textDecorationLine: hover.hovered ? "underline" : "none" }}>{chip.text}</Text>
    {single ? <Icon name="ArrowUpRight" size={12} color={color} />
      : <View style={{ transform: [{ rotate: open ? "90deg" : "0deg" }] }}><Icon name="ChevronRight" size={12} color={color} /></View>}
  </Pressable>;
}

/** A quiet icon target after the row's other targets: it jumps to the Paseo session executing the task. */
function SessionButton({ identifier, colors, onPress }: { identifier: string; colors: Colors; onPress(): void }) {
  const hover = useHover();
  return <Pressable
    accessibilityRole="button" accessibilityLabel={`打开 ${identifier} 的执行会话`} onPress={onPress}
    onHoverIn={hover.onHoverIn} onHoverOut={hover.onHoverOut} hitSlop={SPACE.xs}
    style={{
      flexDirection: "row", alignItems: "center", justifyContent: "center", gap: SPACE.xs, minWidth: 22, height: 22, flexShrink: 0, marginRight: SPACE.xs,
      paddingHorizontal: hover.hovered ? SPACE.sm : 0, borderRadius: RADIUS, backgroundColor: hover.hovered ? colors.surface2 : "transparent", cursor: "pointer",
    }}
  >
    <Icon name="MessageSquare" size={13} color={hover.hovered ? colors.foreground : colors.foregroundMuted} />
    {hover.hovered ? <Text numberOfLines={1} style={{ color: colors.foreground, fontSize: FONT.sm }}>会话</Text> : null}
  </Pressable>;
}

/** A project in the all view: a collapsible folder whose count turns amber when something inside needs you. */
function Folder({ name, nodes, row }: { name: string; nodes: TreeNode[]; row: RowContext }) {
  const { colors } = row;
  const [open, setOpen] = useState(true);
  const hover = useHover();
  const urgent = nodes.some((tree) => tree.section === "attention");
  return <View>
    <Pressable
      accessibilityRole="button" accessibilityLabel={name} accessibilityState={{ expanded: open }} onPress={() => setOpen(!open)}
      onHoverIn={hover.onHoverIn} onHoverOut={hover.onHoverOut}
      style={{ flexDirection: "row", alignItems: "center", borderRadius: RADIUS, backgroundColor: hover.hovered ? colors.surface1 : "transparent" }}
    >
      <View style={{ width: GUTTER, alignItems: "center", transform: [{ rotate: open ? "90deg" : "0deg" }] }}><Icon name="ChevronRight" size={12} color={colors.foregroundMuted} /></View>
      <View style={{ flex: 1, flexDirection: "row", alignItems: "center", gap: SPACE.sm, paddingVertical: SPACE.sm, paddingHorizontal: SPACE.sm }}>
        <Icon name={open ? "FolderOpen" : "Folder"} size={14} color={colors.foregroundMuted} />
        <Text numberOfLines={1} style={{ flex: 1, color: colors.foreground, fontSize: FONT.base, fontWeight: "500" }}>{name}</Text>
        <Text style={{ color: urgent ? colors.statusWarning : colors.foregroundMuted, fontSize: FONT.sm }}>{nodes.length}</Text>
      </View>
    </Pressable>
    {open ? nodes.map((tree) => <TaskRow key={tree.issue.id} data={treeRow(tree)} depth={1} row={row} />) : null}
  </View>;
}

/** What a row shows besides its task: the to-do or tree supplies the phrase and MR marks; everything else is the task's. */
type RowData = { issue: BoardIssue; phrase?: Phrase; chip?: PullRequestChip; mark?: { text: string; label: string } };

export function todoRow(todo: Todo, now: number): RowData {
  const chip = todoChip(todo, now);
  return { issue: todo.issue, ...(chip ? { chip } : { phrase: todoPhrase(todo, now) }) };
}

export function treeRow(tree: TreeNode): RowData {
  const chip = treeChip(tree);
  const phrase = treePhrase(tree);
  const mark = chip ? undefined : mergedMark(tree.issue);
  return { issue: tree.issue, ...(phrase ? { phrase } : {}), ...(chip ? { chip } : { ...(mark ? { mark } : {}) }) };
}

/**
 * Every task row, in the to-do blocks and in the trees alike. Its disclosure is
 * keyed by issue, so a task listed in both places opens and closes in both.
 */
function TaskRow({ data, depth, row }: { data: RowData; depth: number; row: RowContext }) {
  const { issue } = data;
  const tree = row.trees.get(issue.id);
  const details = taskDetails(issue, tree, row);
  const expandable = hasDetails(details);
  const expanded = expandable && row.expandedIds.includes(issue.id);
  return <View>
    <Row
      issue={issue} phrase={data.phrase} chip={data.chip} mark={data.mark} depth={depth} row={row} running={issue.activeRun}
      onPress={issue.url ? () => row.openTask(issue.url!, issue.identifier) : undefined}
      disclosure={expandable ? { expanded, onToggle: () => row.toggle(issue.id, !expanded) } : undefined}
    />
    {expanded ? <TaskDetails details={details} issue={issue} tree={tree} depth={depth} row={row} /> : null}
  </View>;
}

/** The parts of an expanded row, in display order; an empty part is left out. */
export type Details = { reasons: string[]; parent?: NonNullable<BoardIssue["parent"]>; merged: BoardPullRequest[]; children: boolean; actions: QuickAction[] };

function hasDetails(details: Details): boolean {
  return details.reasons.length > 0 || !!details.parent || details.merged.length > 0 || details.children || details.actions.length > 0;
}

/** Who holds the contract's turn, from the viewer's side. */
function turnOf(issue: BoardIssue, viewer: Viewer): string | undefined {
  const actor = issue.contract?.pendingActor;
  if (!actor) return undefined;
  if (isUser(actor, viewer)) return "你";
  if (isSession(actor, viewer)) return "本会话";
  return issue.contract?.pendingName ?? (actor.type === "agent" ? "其他 Agent" : actor.type === "member" ? "其他成员" : actor.type);
}

/** Why the task is where it is: its last failed run, whose turn it is, how long it has been blocked. */
export function reasonsOf(issue: BoardIssue, viewer: Viewer, now: number): string[] {
  const reasons: string[] = [];
  if (issue.lastFailure) {
    const ago = elapsed(issue.lastFailure.at, now);
    reasons.push(`运行失败${ago ? `（${ago} 前）` : ""}${issue.lastFailure.summary ? `：${issue.lastFailure.summary}` : ""}`);
  }
  const turn = turnOf(issue, viewer);
  if (turn) reasons.push(`轮到：${turn}`);
  const blocked = issue.status === "blocked" ? elapsed(issue.blockedSince, now) : "";
  if (blocked) reasons.push(`已阻塞 ${blocked}`);
  return reasons;
}

export function taskDetails(issue: BoardIssue, tree: TreeNode | undefined, row: Pick<RowContext, "viewer" | "now">): Details {
  return {
    reasons: reasonsOf(issue, row.viewer, row.now),
    ...(issue.parent ? { parent: issue.parent } : {}),
    // Open MRs live in the chip; the details keep only the task's merged history.
    merged: issue.pullRequests.filter(isMerged),
    children: !!tree && tree.stages.some((group) => group.nodes.some(isOpen)),
    actions: quickActions(issue),
  };
}

function TaskDetails({ details, issue, tree, depth, row }: { details: Details; issue: BoardIssue; tree?: TreeNode; depth: number; row: RowContext }) {
  const { colors } = row;
  const inset = INDENT * depth + GUTTER + SPACE.sm;
  return <View>
    {details.reasons.map((reason) => <Text key={reason} selectable style={{ paddingLeft: inset, paddingRight: SPACE.sm, paddingVertical: 2, color: colors.foregroundMuted, fontSize: FONT.sm }}>{reason}</Text>)}
    {details.parent ? <ParentLink parent={details.parent} inset={inset} row={row} /> : null}
    {details.merged.map((pr) => <PullRequestRow key={pr.id} pr={pr} issue={issue} depth={depth + 1} row={row} />)}
    {details.children && tree ? <StageGraph tree={tree} depth={depth} row={row} /> : null}
    {details.actions.length > 0 ? <Actions issue={issue} actions={details.actions} inset={inset} row={row} /> : null}
  </View>;
}

function ParentLink({ parent, inset, row }: { parent: NonNullable<BoardIssue["parent"]>; inset: number; row: RowContext }) {
  const { colors } = row;
  const hover = useHover();
  const url = parent.url;
  return <Pressable
    accessibilityRole="link" accessibilityLabel={`父任务 ${parent.identifier} ${displayTitle(parent.title)}`}
    disabled={!url} onPress={url ? () => row.openTask(url, parent.identifier) : undefined}
    onHoverIn={hover.onHoverIn} onHoverOut={hover.onHoverOut}
    style={{ flexDirection: "row", alignItems: "center", gap: SPACE.sm, paddingLeft: inset, paddingRight: SPACE.sm, paddingVertical: SPACE.xs, borderRadius: RADIUS, backgroundColor: hover.hovered ? colors.surface1 : "transparent" }}
  >
    <Text style={{ color: colors.foregroundMuted, fontSize: FONT.sm }}>父任务</Text>
    <Text numberOfLines={1} style={{ flex: 1, color: colors.foreground, fontSize: FONT.sm, textDecorationLine: hover.hovered ? "underline" : "none" }}>{parent.identifier} {displayTitle(parent.title)}</Text>
  </Pressable>;
}

/**
 * The task's quick actions. One runs at a time; cancelling asks once more.
 * A refusal shows the server's reason and leaves the board as it was; success
 * refreshes the board, which is the only place the new state comes from.
 */
function Actions({ issue, actions, inset, row }: { issue: BoardIssue; actions: QuickAction[]; inset: number; row: RowContext }) {
  const { colors } = row;
  const [pending, setPending] = useState<string | null>(null);
  const [confirming, setConfirming] = useState<QuickAction | null>(null);
  const [error, setError] = useState("");
  const run = async (action: QuickAction) => {
    setConfirming(null);
    setPending(action.label);
    setError("");
    try {
      const result = await row.act(issue, action.action);
      if (!result.ok) setError(`${action.label}失败：${result.reason}`);
    } catch (cause) {
      setError(`${action.label}失败：${cause instanceof Error ? cause.message : String(cause)}`);
    } finally {
      setPending(null);
    }
  };
  return <View style={{ gap: SPACE.xs, paddingLeft: inset, paddingRight: SPACE.sm, paddingVertical: SPACE.xs }}>
    {confirming ? <View style={{ flexDirection: "row", alignItems: "center", flexWrap: "wrap", gap: SPACE.sm }}>
      <Text style={{ color: colors.foreground, fontSize: FONT.sm }}>确认取消 {issue.identifier}？</Text>
      <ActionButton label="确认取消" accessibilityLabel={`确认取消 ${issue.identifier}`} tone="danger" colors={colors} onPress={() => void run(confirming)} />
      <ActionButton label="返回" accessibilityLabel={`不取消 ${issue.identifier}`} colors={colors} onPress={() => setConfirming(null)} />
    </View> : <View style={{ flexDirection: "row", alignItems: "center", flexWrap: "wrap", gap: SPACE.sm }}>
      {actions.map((action) => <ActionButton
        key={action.label} label={action.label} accessibilityLabel={`${action.label} ${issue.identifier}`} colors={colors}
        busy={pending === action.label} disabled={pending !== null}
        onPress={() => (action.confirm ? setConfirming(action) : void run(action))}
      />)}
    </View>}
    {error ? <Text selectable style={{ color: colors.statusDanger, fontSize: FONT.sm }}>{error}</Text> : null}
  </View>;
}

function ActionButton({ label, accessibilityLabel, tone, busy, disabled, colors, onPress }: { label: string; accessibilityLabel: string; tone?: Tone; busy?: boolean; disabled?: boolean; colors: Colors; onPress(): void }) {
  const hover = useHover();
  const color = tone ? toneColor(tone, colors) : colors.foreground;
  return <Pressable
    accessibilityRole="button" accessibilityLabel={accessibilityLabel} accessibilityState={{ disabled: !!disabled, busy: !!busy }} disabled={disabled} onPress={onPress}
    onHoverIn={hover.onHoverIn} onHoverOut={hover.onHoverOut}
    style={{
      flexDirection: "row", alignItems: "center", gap: SPACE.xs, height: 24, paddingHorizontal: SPACE.md, borderRadius: RADIUS, borderWidth: 1,
      borderColor: tone ? color : colors.border, backgroundColor: hover.hovered && !disabled ? colors.surface2 : colors.surface1, opacity: disabled && !busy ? 0.5 : 1, cursor: disabled ? "auto" : "pointer",
    }}
  >
    {busy ? <ActivityIndicator size={10} color={colors.foregroundMuted} /> : null}
    <Text style={{ color, fontSize: FONT.sm, fontWeight: "500" }}>{busy ? `${label}中…` : label}</Text>
  </Pressable>;
}

/** A finished subtree with no open MR is history; graphs keep it only in their counts. */
function isOpen(tree: TreeNode): boolean {
  return tree.section !== "done";
}

type RailState = "done" | "current" | "pending";

/**
 * One cell of the rail left of a graph row: a line through the row, broken
 * above or below at the ends, carrying a stage node or a branch tick at the
 * row's middle.
 */
function Rail({ colors, top, bottom, node, tick }: { colors: Colors; top: boolean; bottom: boolean; node?: RailState; tick?: boolean }) {
  const line = { position: "absolute" as const, left: GUTTER / 2, width: 1, backgroundColor: colors.foregroundMuted, opacity: 0.35 };
  const size = node === "current" ? NODE + 2 : NODE;
  return <View style={{ width: GUTTER, alignSelf: "stretch" }}>
    {top ? <View style={{ ...line, top: 0, height: ROW_H / 2 }} /> : null}
    {bottom ? <View style={{ ...line, top: ROW_H / 2, bottom: 0 }} /> : null}
    {tick ? <View style={{ position: "absolute", left: GUTTER / 2, top: ROW_H / 2, width: GUTTER / 2, height: 1, backgroundColor: colors.foregroundMuted, opacity: 0.35 }} /> : null}
    {node === "current" ? <View style={{ position: "absolute", left: GUTTER / 2 - size + 0.5, top: ROW_H / 2 - size, width: size * 2, height: size * 2, borderRadius: size, backgroundColor: colors.foreground, opacity: 0.12 }} /> : null}
    {node ? <View style={{
      position: "absolute", left: (GUTTER - size) / 2 + 0.5, top: ROW_H / 2 - size / 2, width: size, height: size, borderRadius: size / 2,
      backgroundColor: node === "pending" ? colors.surface0 : node === "current" ? colors.foreground : colors.foregroundMuted,
      borderWidth: node === "pending" ? 1.5 : 0, borderColor: colors.foregroundMuted,
    }} /> : null}
  </View>;
}

function ProgressBar({ done, total, colors, current }: { done: number; total: number; colors: Colors; current: boolean }) {
  return <View style={{ width: 36, height: 3, borderRadius: 2, backgroundColor: colors.surface2, overflow: "hidden" }}>
    <View style={{ width: `${Math.round((done / Math.max(total, 1)) * 100)}%`, height: 3, backgroundColor: current ? colors.foreground : colors.foregroundMuted }} />
  </View>;
}

/**
 * A parent's children as a pipeline: each batch is a node on the rail and its
 * children branch off it. Only the current batch opens by default; a parent
 * without batches is one open group of branches with no node.
 */
function StageGraph({ tree, depth, row }: { tree: TreeNode; depth: number; row: RowContext }) {
  const { colors } = row;
  const current = currentStage(tree);
  const staged = tree.stages.some((group) => group.stage !== undefined);
  const [openStages, setOpenStages] = useState<Set<StageGroup["stage"]>>(() => new Set([current?.stage]));
  const stateOf = (group: StageGroup): RailState => group === current ? "current" : group.done === group.total ? "done" : "pending";
  return <View style={{ marginLeft: INDENT * depth }}>
    {tree.stages.map((group, index) => {
      const last = index === tree.stages.length - 1;
      const nodes = group.nodes.filter(isOpen);
      const delivered = mergedIn(group.nodes.filter((child) => !isOpen(child)));
      if (!staged && nodes.length === 0 && delivered.length === 0) return null;
      const open = nodes.length > 0 && (!staged || openStages.has(group.stage));
      const state = stateOf(group);
      const toggle = () => setOpenStages((value) => {
        const next = new Set(value);
        if (next.has(group.stage)) next.delete(group.stage); else next.add(group.stage);
        return next;
      });
      return <View key={group.stage ?? "unstaged"}>
        {staged ? <View style={{ flexDirection: "row" }}>
          <Rail colors={colors} top bottom={!last || open} node={state} />
          <Pressable
            accessibilityRole="button" accessibilityLabel={group.stage === undefined ? "未分批" : `第 ${group.stage} 批`} accessibilityState={{ expanded: open }} disabled={nodes.length === 0} onPress={toggle}
            style={{ flex: 1, flexDirection: "row", alignItems: "center", gap: SPACE.md, minHeight: ROW_H, paddingHorizontal: SPACE.sm }}
          >
            <Text style={{ flex: 1, color: state === "current" ? colors.foreground : colors.foregroundMuted, fontSize: FONT.sm, fontWeight: state === "current" ? "600" : "400" }}>
              {group.stage === undefined ? "未分批" : `第 ${group.stage} 批`}
            </Text>
            {state === "done" ? <PullRequestLinks prs={delivered} colors={colors} />
              : group.total > 1 ? <>
                <ProgressBar done={group.done} total={group.total} colors={colors} current={state === "current"} />
                <Text style={{ minWidth: 28, textAlign: "right", color: colors.foregroundMuted, fontSize: FONT.sm }}>{group.done}/{group.total}</Text>
              </> : null}
          </Pressable>
        </View> : delivered.length > 0 ? <View style={{ flexDirection: "row" }}>
          <Rail colors={colors} top bottom={!last || open} tick />
          <View style={{ flex: 1, flexDirection: "row", alignItems: "center", gap: SPACE.md, minHeight: ROW_H, paddingHorizontal: SPACE.sm }}>
            <Text style={{ flex: 1, color: colors.foregroundMuted, fontSize: FONT.sm }}>已完成 {group.nodes.length - nodes.length} 项</Text>
            <PullRequestLinks prs={delivered} colors={colors} />
          </View>
        </View> : null}
        {open ? nodes.map((child, childIndex) => <View key={child.issue.id} style={{ flexDirection: "row" }}>
          <Rail colors={colors} top bottom={!(last && childIndex === nodes.length - 1)} tick />
          <View style={{ flex: 1 }}><TaskRow data={treeRow(child)} depth={0} row={row} /></View>
        </View>) : null}
      </View>;
    })}
  </View>;
}

/** Merged MR references at a row's end, each opening its MR; a long list ends in a count. */
function PullRequestLinks({ prs, colors }: { prs: BoardPullRequest[]; colors: Colors }) {
  const shown = prs.slice(0, 3);
  return <View style={{ flexDirection: "row", alignItems: "center", gap: SPACE.sm }}>
    {shown.length > 0 ? <Icon name="GitMerge" size={12} color={colors.foregroundMuted} /> : null}
    {shown.map((pr) => <Pressable key={pr.id} accessibilityRole="link" accessibilityLabel={`${prRef(pr)} 已合并`} onPress={() => void Linking.openURL(pr.url)}>
      <Text style={{ color: colors.foregroundMuted, fontSize: FONT.sm, textDecorationLine: "underline" }}>{prRef(pr)}</Text>
    </Pressable>)}
    {prs.length > shown.length ? <Text style={{ color: colors.foregroundMuted, fontSize: FONT.sm }}>+{prs.length - shown.length}</Text> : null}
  </View>;
}

/** An MR under its task: number and title, or the subtask it came from, then its one state that matters; opens the MR. */
function PullRequestRow({ pr, issue, source, depth, row }: { pr: BoardPullRequest; issue: BoardIssue; source?: BoardIssue; depth: number; row: RowContext }) {
  const { colors } = row;
  const hover = useHover();
  const detail = prDetail(pr, issue);
  const name = source ? `${prRef(pr)} · ${source.identifier} ${displayTitle(source.title)}` : `${prRef(pr)} ${displayTitle(pr.title)}`;
  return <Pressable
    accessibilityRole="link" accessibilityLabel={`${name}${detail ? ` ${detail.text}` : ""}`} onPress={() => void Linking.openURL(pr.url)}
    onHoverIn={hover.onHoverIn} onHoverOut={hover.onHoverOut}
    style={{ flexDirection: "row", alignItems: "center", gap: SPACE.sm, paddingLeft: INDENT * depth + GUTTER + SPACE.sm, paddingRight: SPACE.sm, paddingVertical: SPACE.sm, borderRadius: RADIUS, backgroundColor: hover.hovered ? colors.surface1 : "transparent" }}
  >
    <Icon name={isMerged(pr) ? "GitMerge" : pr.state === "draft" ? "GitPullRequestDraft" : "GitPullRequest"} size={13} color={colors.foregroundMuted} />
    <Text numberOfLines={1} style={{ flex: 1, color: colors.foregroundMuted, fontSize: FONT.sm }}>{name}</Text>
    {detail ? <Text numberOfLines={1} style={{ color: toneColor(detail.tone, colors), fontSize: FONT.sm, fontWeight: detail.tone ? "500" : "400" }}>{detail.text}</Text> : null}
  </Pressable>;
}

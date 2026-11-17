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
  isTerminal,
  type BoardData,
  type BoardIssue,
  type BoardPullRequest,
  type BoardScope,
  type Todo,
  type TodoKind,
  type TreeNode,
} from "../shared/board";
import { sessionBoard, taskExpansion } from "../shared/contracts";

type Colors = PluginAgentPanelProps["theme"]["colors"];
type OpenTask = (url: string, identifier?: string) => void;
type Props = Pick<PluginAgentPanelProps, "agentId" | "theme"> & { openTask: OpenTask };

// Paseo's theme ramp: base 14 / sm 12, spacing 4·6·8·12·16·24.
const FONT = { base: 14, sm: 12 } as const;
const SPACE = { xs: 4, sm: 6, md: 8, lg: 12, xl: 16, xxl: 24 } as const;
const RADIUS = 6;
const GUTTER = 16;
const INDENT = 16;

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
  const failedCi = issue.pullRequests.some((pr) => isOpenPullRequest(pr) && pr.ci === "failed");
  const text = failedCi ? "CI 失败" : issue.lastRunFailed ? "运行失败" : "阻塞";
  const urgent = (failedCi || issue.lastRunFailed) && isFresh(issue.lastActivityAt, now);
  return { text: since ? `${text} · ${since}` : text, ...(urgent ? { tone: "danger" as const } : {}) };
}

export function todoPhrase(todo: Todo, now: number): Phrase {
  switch (todo.kind) {
    case "approval": {
      const pr = todo.pullRequests[0];
      return { text: pr ? `${prNumber(pr)} 待你审批` : "待你审批", tone: "action" };
    }
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

const STATUS_PHRASE: Record<string, string> = { backlog: "未开始", todo: "未开始", blocked: "阻塞", in_review: "评审中", done: "已完成", cancelled: "已取消" };

export function treePhrase(tree: TreeNode): Phrase | undefined {
  if (tree.total > 0) {
    const staged = tree.stages.filter((group) => group.stage !== undefined);
    const stage = currentStage(tree);
    if (stage && staged.length > 1) return { text: `第 ${staged.indexOf(stage) + 1}/${staged.length} 批` };
    return { text: `${tree.done}/${tree.total}` };
  }
  const text = STATUS_PHRASE[tree.issue.status];
  return text ? { text } : undefined;
}

type Tone = Phrase["tone"];

/** A task's MRs as one icon: open ones with a count, otherwise a merged mark; red for failed CI, amber when it waits for you. */
export function prSignal(issue: BoardIssue): { icon: string; count: number; tone?: Tone; label: string } | undefined {
  const open = issue.pullRequests.filter(isOpenPullRequest);
  if (open.length === 0) {
    // A closed task's merged MRs are the expected end state, not news.
    const merged = isTerminal(issue) ? 0 : issue.pullRequests.filter((pr) => pr.state === "merged").length;
    return merged > 0 ? { icon: "GitMerge", count: 0, label: `已合并 ${merged} 个 MR` } : undefined;
  }
  const tone: Tone = open.some((pr) => pr.ci === "failed") ? "danger" : awaitingApproval(issue).length > 0 ? "action" : undefined;
  const icon = open.every((pr) => pr.state === "draft") ? "GitPullRequestDraft" : "GitPullRequest";
  return { icon, count: open.length, ...(tone ? { tone } : {}), label: open.map(prRef).join(" ") };
}

function prDetail(pr: BoardPullRequest, issue: BoardIssue): Phrase | undefined {
  if (pr.ci === "failed") return { text: "CI 失败", tone: "danger" };
  if (awaitingApproval(issue).includes(pr)) return { text: "待你审批", tone: "action" };
  if (pr.mergeable?.toLowerCase() === "conflicting") return { text: "有冲突", tone: "danger" };
  if (pr.ci === "pending") return { text: "CI 运行中" };
  if (pr.ci === "passed") return { text: "CI 通过" };
  return pr.state === "draft" ? { text: "草稿" } : undefined;
}

/** One row per issue; an issue with several to-dos shows its most urgent one, already first in board order. */
function firstPerIssue(todos: Todo[]): Todo[] {
  const seen = new Set<string>();
  return todos.filter((todo) => !seen.has(todo.issue.id) && !!seen.add(todo.issue.id));
}

export function BoardSection(props: Props) {
  return <BoardContent key={props.agentId} {...props} />;
}

function BoardContent({ agentId, theme, openTask }: Props) {
  const getBoard = useRpc(sessionBoard);
  const expansion = useRpc(taskExpansion);
  const [scope, setScope] = useState<BoardScope>("session");
  const [data, setData] = useState<BoardData | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [expandedIds, setExpandedIds] = useState<string[]>([]);
  const [expansionError, setExpansionError] = useState("");
  const [open, setOpen] = useState<Record<"parked" | "done", boolean>>({ parked: false, done: false });
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
  const row: RowContext = { colors, openTask, expandedIds, toggle };
  const todos = board ? firstPerIssue(board.todos) : [];
  const todoIds = new Set(todos.map((todo) => todo.issue.id));
  // A childless tree that needs you is fully described by its to-do row.
  const active = board ? [...board.sections.attention, ...board.sections.active].filter((tree) => tree.total > 0 || !todoIds.has(tree.issue.id)) : [];
  const empty = board && todos.length === 0 && active.length === 0 && board.sections.parked.length === 0 && board.sections.done.length === 0;

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
        {todos.length > 0 ? <Section title="需要你" colors={colors}>
          {todos.map((todo) => <TodoRow key={todo.issue.id} todo={todo} now={now} row={row} />)}
        </Section> : null}
        {active.length === 0 ? null
          : ready.scope === "all" ? <View style={{ gap: 2 }}>{groupByProject(active, ready.projects).map((group) => <Folder key={group.id ?? group.name} name={group.name} nodes={group.nodes} row={row} />)}</View>
          : <Section title="进行中" colors={colors}>{active.map((tree) => <TreeRow key={tree.issue.id} tree={tree} depth={0} row={row} />)}</Section>}
        {(["parked", "done"] as const).map((section) => board.sections[section].length > 0 ? <Section
          key={section} title={section === "parked" ? "搁置" : "已完成"} count={board.sections[section].length} colors={colors}
          open={open[section]} onToggle={() => setOpen((value) => ({ ...value, [section]: !value[section] }))}
        >
          {board.sections[section].map((tree) => <TreeRow key={tree.issue.id} tree={tree} depth={0} row={row} />)}
        </Section> : null)}
        {ready.truncated ? <Text style={{ color: colors.foregroundMuted, fontSize: FONT.sm, paddingLeft: GUTTER + SPACE.sm }}>部分子任务未显示</Text> : null}
      </View>}
  </View>;
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

type RowContext = { colors: Colors; openTask: OpenTask; expandedIds: string[]; toggle(id: string, expanded: boolean): void };

/**
 * One line: disclosure gutter, title, then the muted phrase. Hover reveals the
 * issue number in front of the phrase instead of spending a second line on it.
 */
function toneColor(tone: Tone, colors: Colors): string {
  return tone === "danger" ? colors.statusDanger : tone === "action" ? colors.statusWarning : colors.foregroundMuted;
}

function Row({ issue, phrase, depth, row, running, muted, signal, onPress, disclosure }: {
  issue: BoardIssue; phrase?: Phrase; depth: number; row: RowContext; running?: boolean; muted?: boolean; signal?: boolean; onPress?(): void;
  disclosure?: { expanded: boolean; onToggle(): void };
}) {
  const { colors } = row;
  const hover = useHover();
  const [pressed, setPressed] = useState(false);
  const tone = toneColor(phrase?.tone, colors);
  const title = displayTitle(issue.title);
  const prs = signal ? prSignal(issue) : undefined;
  return <View
    style={{ flexDirection: "row", alignItems: "center", paddingLeft: INDENT * depth, borderRadius: RADIUS, backgroundColor: pressed ? colors.surface2 : hover.hovered ? colors.surface1 : "transparent" }}
  >
    <Pressable
      accessibilityRole="button" accessibilityLabel={`${disclosure?.expanded ? "收起" : "展开"} ${issue.identifier}`} accessibilityState={{ expanded: !!disclosure?.expanded }}
      disabled={!disclosure} onPress={disclosure?.onToggle} hitSlop={SPACE.xs}
      style={{ width: GUTTER, alignSelf: "stretch", alignItems: "center", justifyContent: "center" }}
    >
      {disclosure ? <View style={{ transform: [{ rotate: disclosure.expanded ? "90deg" : "0deg" }] }}><Icon name="ChevronRight" size={12} color={colors.foregroundMuted} /></View> : null}
    </Pressable>
    <Pressable
      accessibilityRole="link" accessibilityLabel={`${issue.identifier} ${title}${prs ? ` ${prs.label}` : ""}${phrase ? ` ${phrase.text}` : ""}`}
      disabled={!onPress} onPress={onPress}
      onHoverIn={hover.onHoverIn} onHoverOut={hover.onHoverOut} onPressIn={() => setPressed(true)} onPressOut={() => setPressed(false)}
      style={{ flex: 1, flexDirection: "row", alignItems: "center", gap: SPACE.md, paddingVertical: SPACE.sm, paddingLeft: SPACE.sm, paddingRight: SPACE.sm }}
    >
      <Text numberOfLines={1} style={{ flex: 1, color: muted ? colors.foregroundMuted : colors.foreground, fontSize: FONT.base }}>{title}</Text>
      {running ? <ActivityIndicator size={10} color={colors.foregroundMuted} /> : null}
      {hover.hovered ? <Text style={{ color: colors.foregroundMuted, fontSize: FONT.sm }}>{issue.identifier}</Text> : null}
      {prs ? <View style={{ flexDirection: "row", alignItems: "center", gap: 2 }}>
        <Icon name={prs.icon} size={13} color={toneColor(prs.tone, colors)} />
        {prs.count > 1 ? <Text style={{ color: toneColor(prs.tone, colors), fontSize: FONT.sm }}>{prs.count}</Text> : null}
      </View> : null}
      {phrase ? <Text numberOfLines={1} style={{ color: tone, fontSize: FONT.sm, fontWeight: phrase.tone ? "500" : "400" }}>{phrase.text}</Text> : null}
    </Pressable>
  </View>;
}

function TodoRow({ todo, now, row }: { todo: Todo; now: number; row: RowContext }) {
  const pr = todo.pullRequests[0];
  const external = todo.kind === "approval" && pr ? pr.url : undefined;
  const target = external ?? todo.issue.url;
  return <Row
    issue={todo.issue} phrase={todoPhrase(todo, now)} depth={0} row={row} running={todo.issue.activeRun}
    onPress={target ? () => (external ? void Linking.openURL(external) : row.openTask(target, todo.issue.identifier)) : undefined}
  />;
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
    {open ? nodes.map((tree) => <TreeRow key={tree.issue.id} tree={tree} depth={1} row={row} />) : null}
  </View>;
}

function TreeRow({ tree, depth, row }: { tree: TreeNode; depth: number; row: RowContext }) {
  const { expandedIds, toggle, openTask } = row;
  const { issue } = tree;
  const openPrs = issue.pullRequests.filter(isOpenPullRequest);
  const expandable = tree.total > 0 || openPrs.length > 0;
  const expanded = expandable && expandedIds.includes(issue.id);
  const children = tree.stages.flatMap((group) => group.nodes);
  return <View>
    <Row
      issue={issue} phrase={treePhrase(tree)} depth={depth} row={row} running={issue.activeRun} muted={tree.section === "done"} signal
      onPress={issue.url ? () => openTask(issue.url!, issue.identifier) : undefined}
      disclosure={expandable ? { expanded, onToggle: () => toggle(issue.id, !expanded) } : undefined}
    />
    {expanded ? openPrs.map((pr) => <PullRequestRow key={pr.id} pr={pr} issue={issue} depth={depth + 1} row={row} />) : null}
    {expanded ? children.map((child) => <TreeRow key={child.issue.id} tree={child} depth={depth + 1} row={row} />) : null}
  </View>;
}

/** An open MR under its task: number and title, then its one state that matters; opens the MR. */
function PullRequestRow({ pr, issue, depth, row }: { pr: BoardPullRequest; issue: BoardIssue; depth: number; row: RowContext }) {
  const { colors } = row;
  const hover = useHover();
  const detail = prDetail(pr, issue);
  return <Pressable
    accessibilityRole="link" accessibilityLabel={`${prRef(pr)} ${displayTitle(pr.title)}${detail ? ` ${detail.text}` : ""}`} onPress={() => void Linking.openURL(pr.url)}
    onHoverIn={hover.onHoverIn} onHoverOut={hover.onHoverOut}
    style={{ flexDirection: "row", alignItems: "center", gap: SPACE.sm, paddingLeft: INDENT * depth + GUTTER + SPACE.sm, paddingRight: SPACE.sm, paddingVertical: SPACE.sm, borderRadius: RADIUS, backgroundColor: hover.hovered ? colors.surface1 : "transparent" }}
  >
    <Icon name={pr.state === "draft" ? "GitPullRequestDraft" : "GitPullRequest"} size={13} color={colors.foregroundMuted} />
    <Text numberOfLines={1} style={{ flex: 1, color: colors.foregroundMuted, fontSize: FONT.sm }}>{prRef(pr)} {displayTitle(pr.title)}</Text>
    {detail ? <Text numberOfLines={1} style={{ color: toneColor(detail.tone, colors), fontSize: FONT.sm, fontWeight: detail.tone ? "500" : "400" }}>{detail.text}</Text> : null}
  </Pressable>;
}

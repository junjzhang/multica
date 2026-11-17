import type { PluginAgentPanelProps } from "@getpaseo/plugin/client";
import { useRpc } from "@getpaseo/plugin/client";
import { Icon, Loading } from "@getpaseo/plugin/client/ui";
import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Linking, Pressable, Text, View } from "react-native";
import {
  buildBoard,
  currentStage,
  groupByProject,
  type BoardData,
  type BoardPullRequest,
  type BoardScope,
  type Marker,
  type ReadyBoardData,
  type Section,
  type StageGroup,
  type Todo,
  type TodoKind,
  type TreeNode,
} from "../shared/board";
import { sessionBoard, taskExpansion } from "../shared/contracts";

type Colors = PluginAgentPanelProps["theme"]["colors"];
type OpenTask = (url: string, identifier?: string) => void;
type Props = Pick<PluginAgentPanelProps, "agentId" | "theme"> & { openTask: OpenTask };

// Paseo's theme ramp: base 14 / sm 12, spacing 4·6·8·12·16·24, radius lg 8.
const FONT = { base: 14, sm: 12 } as const;
const SPACE = { xs: 4, sm: 6, md: 8, lg: 12, xl: 16, xxl: 24 } as const;
const RADIUS = 8;
const ICON = { sm: 14, md: 16 } as const;
const RAIL = 16;
const DOT = 6;
// Where a row's title ink starts at depth 0: disclosure slot, row padding, dot slot, gap.
const TITLE_RAIL = RAIL - 4 + 8 + RAIL + 8;

const SCOPES: { value: BoardScope; label: string }[] = [{ value: "session", label: "本会话" }, { value: "all", label: "全部" }];
const SECTION_LABEL: Record<Section, string> = { attention: "需要处理", active: "进行中", parked: "搁置", done: "已完成" };
const TODO_ICON: Record<TodoKind, string> = { approval: "GitPullRequest", user: "CircleDot", acceptance: "ClipboardCheck", session: "MessageSquare", anomaly: "TriangleAlert" };
const TODO_HINT: Record<TodoKind, string> = { approval: "等你审批", user: "等你处理", acceptance: "等验收", session: "待本会话处理", anomaly: "异常" };

function dotColor(marker: Marker, colors: Colors): string | null {
  if (marker === "anomaly") return colors.statusDanger;
  if (marker === "you" || marker === "leader") return colors.statusWarning;
  if (marker === "running") return colors.foregroundMuted;
  if (marker === "done") return colors.border;
  return null;
}

function todoColor(kind: TodoKind, colors: Colors): string {
  return kind === "anomaly" ? colors.statusDanger : colors.statusWarning;
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
  const row: RowContext = { colors, openTask, expandedIds, toggle };
  const empty = board && board.todos.length === 0 && (["attention", "active", "parked", "done"] as const).every((section) => board.sections[section].length === 0);

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
      : <View style={{ gap: SPACE.xxl }}>
        {board.todos.length > 0 ? <TodoCard todos={board.todos} colors={colors} openTask={openTask} /> : null}
        {(["attention", "active"] as const).map((section) => <TreeSection key={section} section={section} nodes={board.sections[section]} data={ready} row={row} />)}
        {(["parked", "done"] as const).map((section) => <TreeSection
          key={section} section={section} nodes={board.sections[section]} data={ready} row={row}
          open={open[section]} onToggle={() => setOpen((value) => ({ ...value, [section]: !value[section] }))}
        />)}
        {ready.truncated ? <Text style={{ color: colors.foregroundMuted, fontSize: FONT.sm }}>部分子任务未显示</Text> : null}
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
    <Icon name={name} size={ICON.md} color={hover.hovered ? colors.foreground : colors.foregroundMuted} />
  </Pressable>;
}

/** A pressable row with Paseo's hover and pressed fill; the fill never moves the content. */
function RowPress({ label, onPress, disabled, children, colors }: { label: string; onPress?(): void; disabled?: boolean; children: ReactNode; colors: Colors }) {
  const hover = useHover();
  const [pressed, setPressed] = useState(false);
  return <Pressable
    accessibilityRole="link" accessibilityLabel={label} disabled={disabled || !onPress} onPress={onPress}
    onHoverIn={hover.onHoverIn} onHoverOut={hover.onHoverOut} onPressIn={() => setPressed(true)} onPressOut={() => setPressed(false)}
    style={{ flex: 1, flexDirection: "row", alignItems: "center", gap: SPACE.md, paddingVertical: SPACE.md, paddingHorizontal: SPACE.md, borderRadius: RADIUS, backgroundColor: pressed ? colors.surface2 : hover.hovered ? colors.surface1 : "transparent" }}
  >
    {children}
  </Pressable>;
}

function TodoCard({ todos, colors, openTask }: { todos: Todo[]; colors: Colors; openTask: OpenTask }) {
  return <View style={{ borderWidth: 1, borderColor: colors.border, borderRadius: RADIUS + 4, paddingVertical: SPACE.xs, paddingHorizontal: SPACE.xs }}>
    {todos.map((todo, index) => {
      const pr = todo.pullRequests[0];
      const external = todo.kind === "approval" && pr ? pr.url : undefined;
      const target = external ?? todo.issue.url;
      const since = todo.kind === "anomaly" ? elapsed(todo.issue.lastActivityAt) : "";
      return <View key={`${todo.kind}:${todo.issue.id}`} style={index > 0 ? { borderTopWidth: 1, borderTopColor: colors.border } : undefined}>
        <RowPress
          label={`${TODO_HINT[todo.kind]} ${todo.issue.identifier}`} colors={colors} disabled={!target}
          onPress={() => target && (external ? void Linking.openURL(external) : openTask(target, todo.issue.identifier))}
        >
          <Icon name={TODO_ICON[todo.kind]} size={ICON.md} color={todoColor(todo.kind, colors)} />
          <View style={{ flex: 1, gap: 2 }}>
            <Text numberOfLines={1} style={{ color: colors.foreground, fontSize: FONT.base }}>{todo.issue.title}</Text>
            <Meta colors={colors} identifier={todo.issue.identifier} pullRequests={todo.pullRequests} trailing={since} />
          </View>
          <Icon name={external ? "ArrowUpRight" : "ChevronRight"} size={ICON.sm} color={colors.foregroundMuted} />
        </RowPress>
      </View>;
    })}
  </View>;
}

function Meta({ colors, identifier, progress, pullRequests, trailing }: { colors: Colors; identifier: string; progress?: string; pullRequests: BoardPullRequest[]; trailing?: string }) {
  const muted = { color: colors.foregroundMuted, fontSize: FONT.sm };
  return <View style={{ flexDirection: "row", flexWrap: "wrap", alignItems: "center", columnGap: SPACE.md, rowGap: 2 }}>
    <Text style={muted}>{identifier}</Text>
    {progress ? <Text style={muted}>{progress}</Text> : null}
    {pullRequests.map((pr) => <PullRequestMeta key={pr.id} pr={pr} colors={colors} />)}
    {trailing ? <Text style={muted}>{trailing}</Text> : null}
  </View>;
}

function PullRequestMeta({ pr, colors }: { pr: BoardPullRequest; colors: Colors }) {
  const muted = { color: colors.foregroundMuted, fontSize: FONT.sm };
  const closed = pr.state === "merged" || pr.state === "closed";
  const ci = closed ? null : pr.ci === "passed" ? { name: "CircleCheck", color: colors.statusSuccess, label: "CI 通过" }
    : pr.ci === "failed" ? { name: "CircleX", color: colors.statusDanger, label: "CI 失败" }
    : pr.ci === "pending" ? { name: "CircleDashed", color: colors.foregroundMuted, label: "CI 运行中" } : null;
  const conflict = !closed && pr.mergeable?.toLowerCase() === "conflicting";
  return <View accessibilityLabel={`${prRef(pr)}${ci ? ` ${ci.label}` : ""}${conflict ? " 有冲突" : ""}${pr.state === "merged" ? " 已合并" : pr.state === "closed" ? " 已关闭" : ""}`} style={{ flexDirection: "row", alignItems: "center", gap: SPACE.xs }}>
    {pr.state === "merged" ? <Icon name="GitMerge" size={12} color={colors.foregroundMuted} /> : null}
    <Text style={[muted, pr.state === "closed" ? { textDecorationLine: "line-through" } : null]}>{prRef(pr)}</Text>
    {ci ? <Icon name={ci.name} size={12} color={ci.color} /> : null}
    {conflict ? <Text style={{ color: colors.statusDanger, fontSize: FONT.sm }}>冲突</Text> : null}
  </View>;
}

type RowContext = { colors: Colors; openTask: OpenTask; expandedIds: string[]; toggle(id: string, expanded: boolean): void };

function TreeSection({ section, nodes, data, row, open = true, onToggle }: { section: Section; nodes: TreeNode[]; data: ReadyBoardData; row: RowContext; open?: boolean; onToggle?(): void }) {
  const { colors } = row;
  if (nodes.length === 0) return null;
  const groups = data.scope === "all" ? groupByProject(nodes, data.projects) : [{ name: "", nodes }];
  const heading = <View style={{ flexDirection: "row", alignItems: "center", gap: SPACE.sm, flex: 1, paddingRight: SPACE.md }}>
    <Text style={{ color: colors.foregroundMuted, fontSize: FONT.base, fontWeight: "500" }}>{SECTION_LABEL[section]}</Text>
    <Text style={{ color: colors.foregroundMuted, fontSize: FONT.base }}>{nodes.length}</Text>
    {onToggle ? <View style={{ marginLeft: "auto", transform: [{ rotate: open ? "90deg" : "0deg" }] }}><Icon name="ChevronRight" size={ICON.sm} color={colors.foregroundMuted} /></View> : null}
  </View>;
  return <View style={{ gap: SPACE.xs }}>
    {onToggle
      ? <Pressable accessibilityRole="button" accessibilityLabel={SECTION_LABEL[section]} accessibilityState={{ expanded: open }} onPress={onToggle} style={{ flexDirection: "row", paddingVertical: SPACE.xs }}>{heading}</Pressable>
      : <View style={{ flexDirection: "row", paddingVertical: SPACE.xs }}>{heading}</View>}
    {open ? groups.map((group) => <View key={group.id ?? group.name} style={{ gap: 0 }}>
      {group.name ? <GroupLabel colors={colors} text={group.name} depth={0} /> : null}
      {group.nodes.map((tree) => <TreeRow key={tree.issue.id} tree={tree} depth={0} row={row} />)}
    </View>) : null}
  </View>;
}

function GroupLabel({ colors, text, depth, trailing }: { colors: Colors; text: string; depth: number; trailing?: string }) {
  return <View style={{ flexDirection: "row", gap: SPACE.sm, paddingTop: SPACE.md, paddingBottom: SPACE.xs, paddingLeft: TITLE_RAIL + RAIL * depth }}>
    <Text style={{ color: colors.foregroundMuted, fontSize: FONT.sm, fontWeight: "500" }}>{text}</Text>
    {trailing ? <Text style={{ color: colors.foregroundMuted, fontSize: FONT.sm }}>{trailing}</Text> : null}
  </View>;
}

function TreeRow({ tree, depth, row }: { tree: TreeNode; depth: number; row: RowContext }) {
  const { colors, expandedIds, toggle, openTask } = row;
  const { issue } = tree;
  const expandable = tree.total > 0;
  const expanded = expandable && expandedIds.includes(issue.id);
  const dot = dotColor(expanded ? tree.classification.marker : tree.marker, colors);
  const stage = currentStage(tree);
  const progress = stage?.stage !== undefined ? `第 ${stage.stage} 批 ${stage.done}/${stage.total}` : expandable ? `${tree.done}/${tree.total}` : "";
  return <View>
    <View style={{ flexDirection: "row", alignItems: "center", paddingLeft: RAIL * depth }}>
      <Pressable
        accessibilityRole="button" accessibilityLabel={`${expanded ? "收起" : "展开"} ${issue.identifier}`} accessibilityState={{ expanded }}
        disabled={!expandable} onPress={() => toggle(issue.id, !expanded)}
        hitSlop={SPACE.sm} style={{ width: RAIL, alignItems: "center", justifyContent: "center", alignSelf: "stretch", marginRight: -SPACE.xs }}
      >
        {expandable ? <View style={{ transform: [{ rotate: expanded ? "90deg" : "0deg" }] }}><Icon name="ChevronRight" size={ICON.sm} color={colors.foregroundMuted} /></View> : null}
      </Pressable>
      <RowPress label={`${issue.identifier} ${issue.title}`} colors={colors} disabled={!issue.url} onPress={() => issue.url && openTask(issue.url, issue.identifier)}>
        <View style={{ width: RAIL, alignItems: "center" }}>
          {dot ? <View style={{ width: DOT, height: DOT, borderRadius: DOT / 2, backgroundColor: dot }} /> : null}
        </View>
        <View style={{ flex: 1, gap: 2 }}>
          <Text numberOfLines={1} style={{ color: tree.section === "done" ? colors.foregroundMuted : colors.foreground, fontSize: FONT.base }}>{issue.title}</Text>
          <Meta colors={colors} identifier={issue.identifier} progress={progress} pullRequests={issue.pullRequests} />
        </View>
      </RowPress>
    </View>
    {expanded ? tree.stages.map((group) => <StageRows key={group.stage ?? "unstaged"} group={group} depth={depth + 1} row={row} />) : null}
  </View>;
}

function StageRows({ group, depth, row }: { group: StageGroup; depth: number; row: RowContext }) {
  return <View>
    {group.stage !== undefined ? <GroupLabel colors={row.colors} text={`第 ${group.stage} 批`} trailing={`${group.done}/${group.total}`} depth={depth} /> : null}
    {group.nodes.map((child) => <TreeRow key={child.issue.id} tree={child} depth={depth} row={row} />)}
  </View>;
}

function prRef(pr: BoardPullRequest): string {
  return `${pr.repo}${/github\.com/i.test(pr.url) ? "#" : "!"}${pr.number}`;
}

function elapsed(value?: string): string {
  const ms = Date.now() - Date.parse(value ?? "");
  if (!Number.isFinite(ms)) return "";
  const minutes = Math.max(0, Math.floor(ms / 60000));
  return minutes < 60 ? `${minutes}m` : minutes < 1440 ? `${Math.floor(minutes / 60)}h` : `${Math.floor(minutes / 1440)}d`;
}

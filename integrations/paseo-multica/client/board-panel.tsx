import type { PluginAgentPanelProps } from "@getpaseo/plugin/client";
import { useRpc } from "@getpaseo/plugin/client";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Linking, Pressable, Text, View } from "react-native";
import {
  buildBoard,
  currentStage,
  groupByProject,
  MARKER_GLYPH,
  STALE_AFTER_DAYS,
  type BoardData,
  type BoardPullRequest,
  type BoardScope,
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

const SCOPE_LABEL: Record<BoardScope, string> = { session: "本会话", all: "全部" };
const SECTION_LABEL: Record<Section, string> = { attention: "需要处理", active: "进行中", parked: "搁置", done: "已完成" };
const TODO_LABEL: Record<TodoKind, string> = { approval: "等你审批", user: "等你处理", acceptance: "等验收", session: "待本会话处理", anomaly: "异常" };
const LEGEND = "● 要你动手  ◐ 等 Leader 验收  ▲ 异常  ✓ 完成  ○ 未开始  ⋯ 运行中";

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
  const row = { colors, openTask, expandedIds, toggle };
  return <View style={{ gap: 12 }}>
    <View style={{ flexDirection: "row", alignItems: "center", justifyContent: "space-between", gap: 8 }}>
      <Text style={{ color: colors.foreground, fontWeight: "600" }}>Multica</Text>
      <Pressable accessibilityRole="button" accessibilityLabel="切换范围" onPress={() => setScope(scope === "session" ? "all" : "session")}>
        <Text style={{ color: colors.accent }}>[{SCOPE_LABEL[scope]} ▾]</Text>
      </Pressable>
      <Pressable accessibilityRole="button" accessibilityLabel="刷新" disabled={loading} onPress={() => void load(true)}>
        <Text style={{ color: colors.accent }}>⟳ {loading ? "…" : data ? clock(data.checkedAt) : ""}</Text>
      </Pressable>
    </View>
    {expansionError ? <Text style={{ color: colors.statusDanger }}>展开状态未保存：{expansionError}</Text> : null}
    {error ? <Text style={{ color: colors.statusDanger }}>{error}</Text>
      : data && data.state !== "ready" ? <Text style={{ color: colors.foregroundMuted }}>{data.state === "disconnected" ? "未连接 Multica。" : "Multica 暂不可用。"}{data.reason}</Text>
      : !board || !ready ? <Text style={{ color: colors.foregroundMuted }}>正在读取任务…</Text>
      : <>
        <TodoQueue todos={board.todos} colors={colors} openTask={openTask} />
        <View style={{ height: 1, backgroundColor: colors.border }} />
        {(["attention", "active"] as const).map((section) => <TreeSection key={section} title={`${SECTION_LABEL[section]} ${board.sections[section].length}`} nodes={board.sections[section]} data={ready} row={row} />)}
        <TreeSection
          title={`${SECTION_LABEL.parked} ${board.sections.parked.length}`}
          note={board.sections.parked.length ? `（Backlog ${board.parkedReasons.backlog} · ${STALE_AFTER_DAYS} 天无动静 ${board.parkedReasons.stale}）` : ""}
          nodes={board.sections.parked} data={ready} row={row}
          open={open.parked} onToggle={() => setOpen((value) => ({ ...value, parked: !value.parked }))}
        />
        <TreeSection
          title={`${SECTION_LABEL.done} ${board.sections.done.length}`}
          nodes={board.sections.done} data={ready} row={row}
          open={open.done} onToggle={() => setOpen((value) => ({ ...value, done: !value.done }))}
        />
        {ready.truncated ? <Text style={{ color: colors.foregroundMuted }}>任务过多，部分子任务未显示。</Text> : null}
        <Text style={{ color: colors.foregroundMuted, fontSize: 12 }}>{LEGEND}</Text>
      </>}
  </View>;
}

function TodoQueue({ todos, colors, openTask }: { todos: Todo[]; colors: Colors; openTask: OpenTask }) {
  return <View style={{ gap: 6 }}>
    <Text style={{ color: colors.foreground, fontWeight: "600" }}>待办 {todos.length}</Text>
    {todos.length === 0 ? <Text style={{ color: colors.foregroundMuted }}>没有待办。</Text> : null}
    {todos.map((todo) => {
      const glyph = todo.kind === "anomaly" ? MARKER_GLYPH.anomaly : todo.kind === "approval" || todo.kind === "user" ? MARKER_GLYPH.you : MARKER_GLYPH.leader;
      const target = todo.kind === "approval" ? todo.pullRequests[0]?.url : todo.issue.url;
      const action = todo.kind === "acceptance" ? "去验收" : "打开";
      const since = todo.kind === "anomaly" ? elapsed(todo.issue.lastActivityAt) : "";
      return <View key={`${todo.kind}:${todo.issue.id}`} style={{ flexDirection: "row", alignItems: "center", justifyContent: "space-between", gap: 8 }}>
        <Text style={{ color: colors.foreground, flexShrink: 1 }}>
          {glyph} {TODO_LABEL[todo.kind]}  {todo.issue.identifier}{todo.pullRequests.map((pr) => `  ${prRef(pr)}  ${prState(pr)}`).join("")}{since ? ` · ${since}` : ""}
        </Text>
        {target ? <Pressable accessibilityRole="link" onPress={() => (todo.kind === "approval" ? void Linking.openURL(target) : openTask(target, todo.issue.identifier))}>
          <Text style={{ color: colors.accent }}>[{action}]</Text>
        </Pressable> : null}
      </View>;
    })}
  </View>;
}

type RowContext = { colors: Colors; openTask: OpenTask; expandedIds: string[]; toggle(id: string, expanded: boolean): void };

function TreeSection({ title, note, nodes, data, row, open = true, onToggle }: { title: string; note?: string; nodes: TreeNode[]; data: ReadyBoardData; row: RowContext; open?: boolean; onToggle?(): void }) {
  const { colors } = row;
  const groups = data.scope === "all" ? groupByProject(nodes, data.projects) : [{ name: "", nodes }];
  return <View style={{ gap: 4 }}>
    <View style={{ flexDirection: "row", justifyContent: "space-between", gap: 8 }}>
      <Text style={{ color: colors.foreground, fontWeight: "600" }}>{title}<Text style={{ color: colors.foregroundMuted, fontWeight: "400" }}>{note ?? ""}</Text></Text>
      {onToggle && nodes.length > 0 ? <Pressable accessibilityRole="button" accessibilityState={{ expanded: open }} onPress={onToggle}><Text style={{ color: colors.accent }}>[{open ? "收起" : "展开"}]</Text></Pressable> : null}
    </View>
    {open ? groups.map((group) => <View key={group.id ?? group.name} style={{ gap: 2 }}>
      {group.name ? <Text style={{ color: colors.foregroundMuted, fontSize: 12 }}>{group.name}</Text> : null}
      {group.nodes.map((tree) => <TreeRow key={tree.issue.id} tree={tree} depth={0} row={row} />)}
    </View>) : null}
  </View>;
}

function TreeRow({ tree, depth, row }: { tree: TreeNode; depth: number; row: RowContext }) {
  const { colors, expandedIds, toggle, openTask } = row;
  const { issue } = tree;
  const expandable = tree.total > 0;
  const expanded = expandable && expandedIds.includes(issue.id);
  const glyph = MARKER_GLYPH[expanded ? tree.classification.marker : tree.marker];
  const stage = currentStage(tree);
  const progress = stage?.stage !== undefined ? `第${stage.stage}批 ${stage.done}/${stage.total}` : expandable ? `${tree.done}/${tree.total}` : "";
  const meta = [progress, issue.statusName ?? issue.status.replace(/_/g, " "), ...issue.pullRequests.map((pr) => `${prRef(pr)} ${prState(pr)}`)].filter(Boolean).join("  ");
  return <View style={{ gap: 2 }}>
    <View style={{ flexDirection: "row", alignItems: "center", gap: 6, marginLeft: depth * 16 }}>
      <Pressable accessibilityRole="button" accessibilityLabel={`${expanded ? "收起" : "展开"} ${issue.identifier}`} accessibilityState={{ expanded }} disabled={!expandable} onPress={() => toggle(issue.id, !expanded)}>
        <Text style={{ color: colors.foregroundMuted, width: 12 }}>{expandable ? (expanded ? "▾" : "▸") : depth > 0 ? "├" : " "}</Text>
      </Pressable>
      <Pressable accessibilityRole="link" disabled={!issue.url} onPress={() => issue.url && openTask(issue.url, issue.identifier)} style={{ flexShrink: 1 }}>
        <Text style={{ color: colors.foreground }}>{glyph} {issue.identifier} {issue.title}<Text style={{ color: colors.foregroundMuted }}>  {meta}</Text></Text>
      </Pressable>
    </View>
    {expanded ? tree.stages.map((group) => <StageRows key={group.stage ?? "unstaged"} group={group} depth={depth + 1} row={row} />) : null}
  </View>;
}

function StageRows({ group, depth, row }: { group: StageGroup; depth: number; row: RowContext }) {
  const { colors } = row;
  const state = group.done === group.total ? "✓" : group.nodes.some((child) => child.section === "active" || child.section === "attention") ? "进行中" : "未开始";
  return <View style={{ gap: 2 }}>
    {group.stage !== undefined ? <Text style={{ color: colors.foregroundMuted, marginLeft: depth * 16 }}>├ 第 {group.stage} 批 ── {group.done}/{group.total} {state}</Text> : null}
    {group.nodes.map((child) => <TreeRow key={child.issue.id} tree={child} depth={depth + (group.stage !== undefined ? 1 : 0)} row={row} />)}
  </View>;
}

function prRef(pr: BoardPullRequest): string {
  return `${pr.repo} ${/github\.com/i.test(pr.url) ? "#" : "!"}${pr.number}`;
}

function prState(pr: BoardPullRequest): string {
  if (pr.state === "merged") return "已合并";
  if (pr.state === "closed") return "已关闭";
  const ci = pr.ci === "passed" ? "✓" : pr.ci === "failed" ? "✗" : pr.ci === "pending" ? "⋯" : "—";
  const review = pr.review === "approved" ? "✓" : pr.review === "change_required" || pr.review === "change_denied" || pr.review === "changes_requested" ? "✗" : pr.review ? "⋯" : "—";
  const mergeable = pr.mergeable?.toLowerCase();
  const merge = mergeable === "mergeable" ? "✓" : mergeable === "conflicting" ? "✗" : "—";
  return `CI ${ci} 审 ${review} 合 ${merge}`;
}

function clock(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.valueOf()) ? "" : `${String(date.getHours()).padStart(2, "0")}:${String(date.getMinutes()).padStart(2, "0")}`;
}

function elapsed(value?: string): string {
  const ms = Date.now() - Date.parse(value ?? "");
  if (!Number.isFinite(ms)) return "";
  const minutes = Math.max(0, Math.floor(ms / 60000));
  return minutes < 60 ? `${minutes}m` : minutes < 1440 ? `${Math.floor(minutes / 60)}h` : `${Math.floor(minutes / 1440)}d`;
}

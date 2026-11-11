import type { PluginAgentPanelProps } from "@getpaseo/plugin/client";
import { useRpc } from "@getpaseo/plugin/client";
import {
  ExternalLink,
  SettingsAction,
  SettingsCard,
  SettingsRow,
  SettingsSection,
} from "@getpaseo/plugin/client/ui";
import { useCallback, useEffect, useState } from "react";
import { tasksSummary } from "../shared/contracts";
import {
  TASK_GROUPS,
  taskCounts,
  taskGroupTitle,
  taskStatusLabel,
  type SessionTaskItem,
  type SessionTasksView,
} from "../shared/tasks";

/**
 * The three lists of this session's Multica obligations. The section is
 * read-only; it explains a disconnected or unavailable state instead of
 * presenting an empty list that would look like "no work".
 */
export function SessionTasksSection({ agentId }: Pick<PluginAgentPanelProps, "agentId">) {
  const getTasks = useRpc(tasksSummary);
  const [view, setView] = useState<SessionTasksView | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setView(await getTasks({ agentId }));
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause));
    } finally {
      setLoading(false);
    }
  }, [agentId, getTasks]);

  useEffect(() => {
    void load();
  }, [load]);

  return (
    <SettingsSection
      title="This session's tasks"
      info="Read-only Multica work for this session: tasks you own as executor or leader, deliveries waiting for your acceptance, and delegations you dispatched whose result has not come back."
    >
      {loading ? (
        <SettingsCard>
          <SettingsRow label="Status">Loading…</SettingsRow>
        </SettingsCard>
      ) : error ? (
        <SettingsCard>
          <SettingsRow label="Could not read this session's tasks" error={error} />
        </SettingsCard>
      ) : view?.state === "disconnected" ? (
        <SettingsCard>
          <SettingsRow label="Multica is not connected" hint={view.reason} />
        </SettingsCard>
      ) : view?.state === "unavailable" ? (
        <SettingsCard>
          <SettingsRow label="Tasks unavailable" error={view.reason} />
        </SettingsCard>
      ) : view?.state === "ready" ? (
        <>
          <SettingsCard>
            <SettingsRow label="Counts" hint={countsLabel(view)} />
            <SettingsRow label="Session" hint={view.sessionId ?? "—"} />
            {view.truncated ? (
              <SettingsRow label="Showing the first 50 items of each list; a list may therefore be incomplete." />
            ) : null}
          </SettingsCard>
          {TASK_GROUPS.map((group) => (
            <SettingsCard key={group}>
              <SettingsRow label={taskGroupTitle(group)} hint={String(view[group].length)} />
              {view[group].length === 0 ? (
                <SettingsRow label="None" />
              ) : (
                view[group].map((item) => <SessionTaskRow key={item.issueId + ":" + item.role} item={item} />)
              )}
            </SettingsCard>
          ))}
          <SettingsAction
            label="Task list"
            actionLabel={loading ? "Refreshing…" : "Refresh"}
            disabled={loading}
            onPress={() => void load()}
          />
        </>
      ) : null}
    </SettingsSection>
  );
}

function SessionTaskRow({ item }: { item: SessionTaskItem }) {
  const title = item.title.trim() === "" ? item.issueId : item.title;
  return (
    <SettingsRow label={item.identifier ?? title} hint={hintFor(item)}>
      {item.url ? <ExternalLink href={item.url}>{title}</ExternalLink> : title}
    </SettingsRow>
  );
}

function hintFor(item: SessionTaskItem): string {
  const status = taskStatusLabel(item);
  if (item.role === "return" && item.taskStatus) {
    return `${status} · return ${item.taskStatus}`;
  }
  return status;
}

function countsLabel(view: Extract<SessionTasksView, { state: "ready" }>): string {
  const counts = taskCounts(view);
  return `${counts.owned} owned · ${counts.accepting} to accept · ${counts.returning} to return`;
}

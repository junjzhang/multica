import type { PluginAgentPanelProps } from "@getpaseo/plugin/client";
import { useRpc } from "@getpaseo/plugin/client";
import { SettingsAction, SettingsCard, SettingsRow, SettingsSection } from "@getpaseo/plugin/client/ui";
import { useCallback, useEffect, useState } from "react";
import { statusSession, type StatusView } from "../shared/contracts";
import { SessionTasksSection } from "./session-tasks";

const STATE_LABEL: Record<StatusView["state"], string> = {
  connected: "Connected",
  invalid: "Needs attention",
  unverified: "Not verified",
  disconnected: "Not connected",
};

export function StatusPanel({ agentId }: PluginAgentPanelProps) {
  const getStatus = useRpc(statusSession);
  const [view, setView] = useState<StatusView | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setView(await getStatus({ agentId }));
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause));
    } finally {
      setLoading(false);
    }
  }, [agentId, getStatus]);

  useEffect(() => {
    void load();
  }, [load]);

  return (
    <>
    <SettingsSection
      title="Multica connection"
      info="This session's Multica identity is private to this Paseo host. Use /multica connect, /multica status, or /multica disconnect in the composer."
    >
      <SettingsCard>
        <SettingsRow label="State">{loading ? "Checking…" : view ? STATE_LABEL[view.state] : "Unavailable"}</SettingsRow>
        <SettingsRow label="External agent">{view?.agentId ?? "—"}</SettingsRow>
        <SettingsRow label="Workspace">{view?.workspaceId ?? "—"}</SettingsRow>
        <SettingsRow label="Return address">{view?.returnAddress ?? "—"}</SettingsRow>
        <SettingsRow label="Delivery daemon">{view?.daemonId ?? "—"}</SettingsRow>
        <SettingsRow label="Credential expires">{view?.expiresAt ?? "—"}</SettingsRow>
        <SettingsAction
          label="Connection details"
          actionLabel={loading ? "Refreshing…" : "Refresh"}
          disabled={loading}
          onPress={() => void load()}
        />
      </SettingsCard>
      {error ? (
        <SettingsRow label="Could not read status" error={error} />
      ) : view?.reason ? (
        <SettingsRow label="Action needed" hint={view.reason} error={view.reason} />
      ) : null}
    </SettingsSection>
    <SessionTasksSection agentId={agentId} />
    </>
  );
}

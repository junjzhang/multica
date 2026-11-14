import type { PluginSurfaceProps, SettingsState } from "@getpaseo/plugin/client";
import { useRpc, useSettings } from "@getpaseo/plugin/client";
import {
  SettingsAction,
  SettingsCard,
  SettingsInput,
  SettingsRow,
  SettingsSection,
} from "@getpaseo/plugin/client/ui";
import { useEffect, useState } from "react";
import { backgroundWorkSupport } from "../shared/contracts";
import { connectionSettings } from "../shared/settings";

type ReadySettings = Extract<SettingsState<typeof connectionSettings.schema>, { status: "ready" }>;

export function ConnectionSettingsScreen(_props: PluginSurfaceProps) {
  const settings = useSettings(connectionSettings);
  const ready: ReadySettings | null = settings.status === "ready" ? settings : null;
  const [draft, setDraft] = useState({ ownerProfile: "", daemonId: "", daemonProfile: "" });

  useEffect(() => {
    if (ready) setDraft({ ownerProfile: ready.values.ownerProfile, daemonId: ready.values.daemonId, daemonProfile: ready.values.daemonProfile });
  }, [ready?.revision]);

  if (settings.status === "loading") {
    return (
      <SettingsSection title="Multica connection">
        <SettingsRow label="Settings">Loading…</SettingsRow>
      </SettingsSection>
    );
  }
  if (settings.status !== "ready") {
    return (
      <SettingsSection title="Multica connection">
        <SettingsRow label="Settings" error={settings.error} />
        <SettingsAction label="Try again" actionLabel="Reload" onPress={() => void settings.reload()} />
        {settings.status === "invalid" ? (
          <SettingsAction label="Restore defaults" actionLabel="Reset" onPress={() => void settings.reset()} />
        ) : null}
      </SettingsSection>
    );
  }

  const save = () =>
    void settings.save(
      { ownerProfile: draft.ownerProfile.trim(), daemonId: draft.daemonId.trim(), daemonProfile: draft.daemonProfile.trim() },
      settings.revision,
    );

  return (
    <SettingsSection
      title="Multica connection"
      info="The owner profile supplies registration and token management. Its credential stays on this host and is never shown here or sent to the agent."
    >
      <BackgroundSupportNotice />
      <SettingsCard>
        <SettingsInput
          label="Owner profile"
          hint="A Multica CLI profile name on this host. Leave empty for the default profile."
          initialValue={settings.values.ownerProfile}
          placeholder="default"
          onChangeText={(ownerProfile) => setDraft((current) => ({ ...current, ownerProfile }))}
          disabled={settings.saving}
        />
        <SettingsInput
          label="Local wait daemon profile"
          hint='The local Multica profile that runs waits. Enter "default" to select the default profile. Cancelling a wait stops only its local monitor.'
          initialValue={settings.values.daemonProfile}
          placeholder="default"
          onChangeText={(daemonProfile) => setDraft((current) => ({ ...current, daemonProfile }))}
          disabled={settings.saving}
        />
        <SettingsInput
          label="Delivery daemon"
          hint="The Multica daemon id authorized to deliver results to this host."
          initialValue={settings.values.daemonId}
          placeholder="daemon id"
          onChangeText={(daemonId) => setDraft((current) => ({ ...current, daemonId }))}
          disabled={settings.saving}
        />
        <SettingsAction
          label="Connection settings"
          actionLabel={settings.saving ? "Saving…" : "Save"}
          disabled={settings.saving}
          onPress={save}
        />
      </SettingsCard>
      {settings.saveError ? <SettingsRow label="Could not save" error={settings.saveError} /> : null}
      <SettingsRow label="Next step" hint="Run /multica connect in an existing conversation on this host.">
        /multica connect
      </SettingsRow>
    </SettingsSection>
  );
}

function BackgroundSupportNotice() {
  const readSupport = useRpc(backgroundWorkSupport);
  const [support, setSupport] = useState<"loading" | "supported" | "unsupported" | "error">("loading");
  useEffect(() => {
    let active = true;
    void readSupport({}).then((result) => { if (active) setSupport(result.supported ? "supported" : "unsupported"); }, () => { if (active) setSupport("error"); });
    return () => { active = false; };
  }, [readSupport]);
  if (support === "error") return <SettingsRow label="Background waits" error="Could not check this host. Reconnect and reopen settings." />;
  if (support !== "unsupported") return null;
  return <SettingsRow label="Background waits" error="Update this Paseo host to enable external background sources. The Multica waits continue running." />;
}

import type { PluginTimelineItemProps } from "@getpaseo/plugin/client";
import {
  ExternalLink,
  Markdown,
  SettingsAction,
  SettingsCard,
  SettingsRow,
} from "@getpaseo/plugin/client/ui";
import { useState } from "react";
import type { DeliveryData } from "../shared/delivery";

/**
 * One Multica delivery rendered as a card. The card shows what the delivery is
 * for; the stable IDs, payload and the prompt itself stay behind the raw
 * details, so nothing the agent was asked to read is lost.
 */
export function DeliveryCard({ item }: PluginTimelineItemProps<DeliveryData>) {
  const [expanded, setExpanded] = useState(false);
  const delivery = item.data;
  const heading = delivery.issueIdentifier ?? delivery.issueId ?? "This session";

  return (
    <SettingsCard>
      <SettingsRow label="Multica delivery" hint={delivery.kindLabel}>
        {heading}
      </SettingsRow>
      {delivery.issueTitle ? <SettingsRow label="Task">{delivery.issueTitle}</SettingsRow> : null}
      {delivery.issueUrl ? (
        <SettingsRow label="Link">
          <ExternalLink href={delivery.issueUrl}>{delivery.issueUrl}</ExternalLink>
        </SettingsRow>
      ) : null}
      <SettingsAction
        label="Raw details"
        actionLabel={expanded ? "Hide" : "Show"}
        onPress={() => setExpanded((open) => !open)}
      />
      {expanded ? (
        <SettingsRow label="Delivered prompt">
          <Markdown>{delivery.raw}</Markdown>
        </SettingsRow>
      ) : null}
    </SettingsCard>
  );
}

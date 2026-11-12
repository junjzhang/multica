import type { PluginTimelineItemProps } from "@getpaseo/plugin/client";
import { useState } from "react";
import { Linking, Pressable, Text, View } from "react-native";
import type { DeliveryData } from "../shared/delivery";

export function DeliveryCard({ item, theme, openTask }: PluginTimelineItemProps<DeliveryData> & { openTask(url: string, identifier?: string): void }) {
  const [expanded, setExpanded] = useState(false);
  const delivery = item.data;
  const colors = theme.colors;
  const heading = delivery.issueIdentifier ?? delivery.issueId ?? "This session";

  return (
    <View style={{ backgroundColor: colors.surface1, borderColor: colors.border, borderWidth: 1, borderRadius: 8, padding: 12, gap: 8 }}>
      <Text style={{ color: colors.foreground, fontWeight: "600" }}>
        {heading} <Text style={{ color: colors.foregroundMuted }}>· {delivery.kindLabel}{delivery.outcome ? ` · ${delivery.outcome}` : ""}</Text>
      </Text>
      {delivery.mentionMode ? <Text style={{ color: colors.foregroundMuted }}>{delivery.mentionMode}</Text> : null}
      {delivery.issueTitle ? <Text style={{ color: colors.foreground }}>{delivery.issueTitle}</Text> : null}
      {delivery.issueUrl ? (
        <Text accessibilityRole="link" onPress={() => openTask(delivery.issueUrl!, delivery.issueIdentifier)} style={{ color: colors.accent }}>
          Open task
        </Text>
      ) : null}
      {delivery.issueUrl ? <Text accessibilityRole="link" onPress={() => void Linking.openURL(delivery.issueUrl!)} style={{ color: colors.accent }}>Open in browser</Text> : null}
      <Pressable accessibilityRole="button" accessibilityState={{ expanded }} onPress={() => setExpanded((open) => !open)}>
        <Text style={{ color: colors.foregroundMuted }}>{expanded ? "Hide original" : "Show original"}</Text>
      </Pressable>
      {expanded ? <Text selectable style={{ color: colors.foreground, backgroundColor: colors.surface2, padding: 8 }}>{delivery.raw}</Text> : null}
    </View>
  );
}

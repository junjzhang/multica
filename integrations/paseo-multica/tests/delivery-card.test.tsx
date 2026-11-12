import type { PluginTimelineItemProps } from "@getpaseo/plugin/client";
import React from "react";
import { act, create, type ReactTestRenderer } from "react-test-renderer";
import { expect, it, vi } from "vitest";
import type { DeliveryData } from "../shared/delivery";

vi.mock("react-native", () => ({
  View: "View", Text: "Text", Pressable: "Pressable", Linking: { openURL: vi.fn() },
}));

import { DeliveryCard } from "../client/delivery-card";

const data: DeliveryData = {
  kind: "delegation_result", kindLabel: "delegation result", outcome: "submitted",
  issueIdentifier: "WORK-266", issueTitle: "A task", issueUrl: "https://example.test/task", raw: "Original prompt",
};

for (const mode of ["light", "dark"]) {
  it(`uses the ${mode} theme and reveals the original on demand`, async () => {
    const colors = Object.fromEntries(["surface0", "surface1", "surface2", "border", "foreground", "foregroundMuted", "accent", "accentForeground", "statusSuccess", "statusWarning", "statusDanger"].map((key) => [key, `${mode}-${key}`]));
    const props = { item: { type: "plugin", kind: "multica-delivery", version: 1, data }, theme: { colors } } as unknown as PluginTimelineItemProps<DeliveryData>;
    let renderer: ReactTestRenderer;
    await act(async () => { renderer = create(<DeliveryCard {...props} openTask={vi.fn()} />); });
    const before = JSON.stringify(renderer!.toJSON());
    expect(before).toContain("WORK-266");
    expect(before).toContain("A task");
    expect(before).toContain("submitted");
    expect(before).toContain(`${mode}-foreground`);
    expect(before).toContain(`${mode}-accent`);
    expect(before).not.toContain("Original prompt");
    await act(async () => { renderer!.root.findByProps({ accessibilityRole: "button" }).props.onPress(); });
    expect(JSON.stringify(renderer!.toJSON())).toContain("Original prompt");
    await act(async () => { renderer!.unmount(); });
  });
}

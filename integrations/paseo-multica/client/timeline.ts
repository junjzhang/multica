import type { PluginTimelineTransformerContribution } from "@getpaseo/plugin/client";
import { DELIVERY_ITEM_KIND, DELIVERY_ITEM_VERSION, parseDelivery } from "../shared/delivery";

/**
 * Shows a Multica mailbox delivery as a card instead of a wall of UUIDs.
 *
 * The delivery reaches the agent as a normal user message, so this only
 * changes display: the original text is kept verbatim inside the card, and an
 * unrecognized or older-format prompt is left exactly as it arrived.
 */
export const deliveryTransformer: PluginTimelineTransformerContribution<"user_message"> = {
  id: "multica-delivery",
  query: { itemType: "user_message" },
  transform({ item, phase }) {
    if (phase !== "complete") return undefined;
    const delivery = parseDelivery(item.text);
    if (!delivery) return undefined;
    return {
      items: [
        {
          type: "plugin",
          kind: DELIVERY_ITEM_KIND,
          version: DELIVERY_ITEM_VERSION,
          data: delivery,
        },
      ],
    };
  },
};

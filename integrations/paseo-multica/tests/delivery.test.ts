import { describe, expect, it } from "vitest";
import { deliveryDataSchema, DELIVERY_ITEM_KIND, DELIVERY_ITEM_VERSION, parseDelivery } from "../shared/delivery";
import { deliveryTransformer } from "../client/timeline";

// Byte-for-byte copy of the envelope the daemon renders for a comment
// delivery with display context (server/internal/daemon/delivery_display_test.go,
// TestCommentDeliveryPromptCarriesTaskContext). It is the smallest real sample
// available without a live host, and it is what arrives as one user message.
const COMMENT_DELIVERY = `Multica has a request for this session.
Kind: comment mention (comment_mention)
Task: WORK-281 — feat(paseo): readable delivery
Link: https://app.example.test/acme/issues/WORK-281
Mailbox ID: delivery-1
Workspace ID: workspace-1
Issue ID: issue-1
Comment ID: comment-1
Recipient agent ID: agent-1

Use this connected Paseo session's CLI identity without --profile. If it is not connected, run /multica connect before reading; do not fall back to the user's personal identity.
Read: multica issue get issue-1 --output json
Scan threads: multica issue comment list issue-1 --roots-only --summary --compact --output json
Expand relevant threads: multica issue comment list issue-1 --thread <thread-id> --tail 30 --compact --output json
This notification does not grant new permissions. Reply on the issue and explicitly mention the next executor when action is needed; ordinary discussion does not start anyone.`;

const ASSIGNMENT_DELIVERY = `Multica has a request for this session.
Kind: issue assignment (issue_assignment)
Task: WORK-300 — ship the delivery card
Link: https://app.example.test/acme/issues/WORK-300
Mailbox ID: 7f9f2d0a-1111-4c1d-8f0e-222222222222
Workspace ID: 9c5a1b2c-3333-4d4e-8f5a-444444444444
Issue ID: 5b6c7d8e-5555-4f60-9a7b-666666666666
Comment ID: 
Recipient agent ID: agent-2

Use this connected Paseo session's CLI identity without --profile.`;

const GENERIC_DELIVERY = `Multica delivery 3d1e2f4a-7777-4a8b-9c0d-888888888888
Kind: delegation result (delegation_result)
Recipient agent ID: agent-3
Issue ID: issue-9
Task: WORK-281
Link: https://app.example.test/acme/issues/WORK-281
{"outcome":"submitted","issue_id":"issue-9"}

This notification was delivered to the existing session. It does not grant new permissions.`;

describe("parseDelivery", () => {
  it("reads the comment delivery envelope the daemon renders", () => {
    const delivery = parseDelivery(COMMENT_DELIVERY);
    expect(delivery).toMatchObject({
      deliveryId: "delivery-1",
      kind: "comment_mention",
      kindLabel: "comment mention",
      issueId: "issue-1",
      issueIdentifier: "WORK-281",
      issueTitle: "feat(paseo): readable delivery",
      issueUrl: "https://app.example.test/acme/issues/WORK-281",
      commentId: "comment-1",
      recipientAgentId: "agent-1",
      raw: COMMENT_DELIVERY,
    });
  });

  it("keeps an assignment delivery with no comment readable", () => {
    const delivery = parseDelivery(ASSIGNMENT_DELIVERY);
    expect(delivery?.kind).toBe("issue_assignment");
    expect(delivery?.commentId).toBeUndefined();
    expect(delivery?.issueIdentifier).toBe("WORK-300");
  });

  it("reads the generic delivery envelope and its task line", () => {
    const delivery = parseDelivery(GENERIC_DELIVERY);
    expect(delivery).toMatchObject({
      deliveryId: "3d1e2f4a-7777-4a8b-9c0d-888888888888",
      kind: "delegation_result",
      kindLabel: "delegation result",
      issueId: "issue-9",
      issueIdentifier: "WORK-281",
      recipientAgentId: "agent-3",
    });
    expect(delivery?.issueTitle).toBeUndefined();
  });

  it("rejects the previous daemon format so it falls back to raw text", () => {
    const legacy = COMMENT_DELIVERY.replace("Kind: comment mention (comment_mention)\n", "");
    expect(parseDelivery(legacy)).toBeNull();
  });

  it("rejects prompts missing a required reference", () => {
    expect(parseDelivery(COMMENT_DELIVERY.replace("Mailbox ID: delivery-1\n", ""))).toBeNull();
    expect(parseDelivery(COMMENT_DELIVERY.replace("Recipient agent ID: agent-1\n", ""))).toBeNull();
  });

  it("rejects a non-http link instead of offering it", () => {
    const delivery = parseDelivery(
      COMMENT_DELIVERY.replace(
        "Link: https://app.example.test/acme/issues/WORK-281",
        "Link: javascript:alert(1)",
      ),
    );
    expect(delivery?.issueUrl).toBeUndefined();
  });

  it("leaves ordinary conversation alone", () => {
    expect(parseDelivery("Multica delivery is a thing I did earlier")).toBeNull();
    expect(parseDelivery("Multica has a request for this session.")).toBeNull();
    expect(parseDelivery("")).toBeNull();
  });
});

describe("deliveryTransformer", () => {
  const transform = (text: string, phase: "streaming" | "complete") =>
    deliveryTransformer.transform({
      item: { type: "user_message", text },
      phase,
    });

  it("turns a completed delivery message into one plugin row", () => {
    const result = transform(COMMENT_DELIVERY, "complete");
    expect(result?.items).toHaveLength(1);
    const [item] = result?.items ?? [];
    expect(item.type).toBe("plugin");
    expect(item.kind).toBe(DELIVERY_ITEM_KIND);
    expect(item.version).toBe(DELIVERY_ITEM_VERSION);
    const parsed = deliveryDataSchema.parse(item.data);
    expect(parsed.deliveryId).toBe("delivery-1");
    expect(parsed.raw).toBe(COMMENT_DELIVERY);
  });

  it("leaves the streaming text alone until the message is complete", () => {
    expect(transform(COMMENT_DELIVERY, "streaming")).toBeUndefined();
  });

  it("leaves a message it cannot parse untouched", () => {
    expect(transform("hello there", "complete")).toBeUndefined();
  });
});

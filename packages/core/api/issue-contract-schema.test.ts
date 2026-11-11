// @vitest-environment node
import { describe, expect, it } from "vitest";
import { ContractViewSchema } from "./issue-contract-schema";
import { parseWithFallback } from "./schema";
import { IssueSchema, SquadSchema } from "./schemas";

describe("contract API boundary", () => {
  it("keeps additive issue summaries optional and isolates malformed contracts", () => {
    expect(IssueSchema.shape.contract.parse(undefined)).toBeUndefined();
    expect(IssueSchema.shape.contract.parse({ contract: {} })).toBeUndefined();
    expect(
      IssueSchema.shape.contract.parse({ contract: null })?.contract,
    ).toBeNull();
  });
  it("keeps old issues empty and defaults missing capabilities to denied", () => {
    expect(ContractViewSchema.parse({ contract: null })).toEqual({
      contract: null,
      stale: false,
      accepted: [],
      pending_actor: null,
      executor: null,
      executor_representative: null,
      acceptor: null,
      merge_authorization: {
        allowed: false,
        repository_url: null,
        target_branch: null,
      },
      can_edit: false,
      can_submit: false,
      can_review: false,
    });
  });
  it("does not invent an agreement from malformed state", () => {
    for (const raw of [
      {},
      { contract: {} },
      { contract: { sequence: 1, versions: [] } },
      { contract: null, accepted: [-1] },
      { contract: null, can_review: "true" },
      { contract: null, executor_representative: { type: "agent", id: 42 } },
    ]) {
      expect(
        parseWithFallback(raw, ContractViewSchema, null, {
          endpoint: "contract",
        }),
      ).toBeNull();
    }
  });
  it("preserves squad responsibility separately from the real representative", () => {
    const view = ContractViewSchema.parse({
      contract: null,
      executor: { type: "squad", id: "squad" },
      executor_representative: { type: "agent", id: "leader" },
      pending_actor: { type: "squad", id: "squad" },
      can_submit: true,
    });
    expect(view.executor).toEqual({ type: "squad", id: "squad" });
    expect(view.executor_representative).toEqual({ type: "agent", id: "leader" });
    expect(view.pending_actor).toEqual(view.executor);
  });
  it("keeps session authors and ignores malformed additive session fields", () => {
    const view = ContractViewSchema.parse({
      contract: null,
      executor_representative: { type: "agent", id: "template", session_id: "session-a" },
    });
    expect(view.executor_representative?.session_id).toBe("session-a");
    const malformed = ContractViewSchema.parse({
      contract: null,
      executor_representative: { type: "agent", id: "template", session_id: 42 },
    });
    expect(malformed.executor_representative?.session_id).toBeUndefined();
    expect(SquadSchema.shape.default_leader_unavailable_reason.parse(42)).toBeUndefined();
    expect(IssueSchema.shape.leadership_revision.parse(-1)).toBeUndefined();
    expect(IssueSchema.shape.leadership_revision.parse(undefined)).toBeUndefined();
  });
});

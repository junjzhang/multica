// @vitest-environment node
import { describe, expect, it } from "vitest";
import { ContractViewSchema } from "./issue-contract-schema";
import { parseWithFallback } from "./schema";
import { IssueSchema } from "./schemas";

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
    ]) {
      expect(
        parseWithFallback(raw, ContractViewSchema, null, {
          endpoint: "contract",
        }),
      ).toBeNull();
    }
  });
});

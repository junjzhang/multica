// @vitest-environment node
import { describe, expect, it } from "vitest";
import { describeError } from "../shared/redaction";

describe("diagnostic redaction", () => {
  it("normalizes fragmented credentials before redaction and preserves recovery text", () => {
    const recovery = "Revoke token 33333333-3333-4333-8333-333333333333\n\t下一步";
    const error = new Error(`${recovery}: mul_fix\u0000ture_owner_token mea_fix\u0008ture_external_token\u001b\u009b`);
    expect(describeError(error)).toBe(`${recovery}: [redacted] [redacted]`);
  });

  it("redacts prefixes adjacent to word characters after normalization", () => {
    expect(describeError(new Error("x\u0001mea_ABCDEFGHIJKL a\u009bmat_ABCDEFGHIJ\u0007\tz xmul_ABCDEFGHIJKL")))
      .toBe("x[redacted] a[redacted]\tz x[redacted]");
  });
});

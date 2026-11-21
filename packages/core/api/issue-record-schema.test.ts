// @vitest-environment node
import { describe, expect, it } from "vitest";
import { IssueRecordListSchema, supersededSeqs } from "./issue-record-schema";

const valid = {
  id: "r1",
  issue_id: "i1",
  seq: 1,
  kind: "run",
  summary: "repeat",
  rationale: "",
  evidence: ["ci-run/1"],
  verdict: "pending",
  fields: { protocol: "v1", gpus: 8 },
  supersedes_seq: null,
  author_type: "agent",
  author_id: "a1",
  created_at: "2026-11-21T00:00:00Z",
};

describe("issue record API boundary", () => {
  it("drops malformed rows without hiding valid ones", () => {
    const parsed = IssueRecordListSchema.parse({
      records: [valid, { seq: "two" }, { ...valid, id: "r3", seq: 3, kind: "future", verdict: "new" }],
    });
    expect(parsed.records.map((r) => r.seq)).toEqual([1, 3]);
    expect(parsed.records[1]?.kind).toBe("future");
  });

  it("defaults missing optional collections", () => {
    const { evidence: _e, fields: _f, rationale: _r, supersedes_seq: _s, ...bare } = valid;
    const [record] = IssueRecordListSchema.parse({ records: [bare] }).records;
    expect(record).toMatchObject({ evidence: [], fields: {}, rationale: "", supersedes_seq: null });
  });

  it("treats a non-array payload as an empty ledger", () => {
    expect(IssueRecordListSchema.parse({ records: "x" }).records).toEqual([]);
    expect(IssueRecordListSchema.parse({}).records).toEqual([]);
  });

  it("maps each superseded seq to its successor", () => {
    const records = IssueRecordListSchema.parse({
      records: [valid, { ...valid, id: "r2", seq: 2, supersedes_seq: 1, verdict: "pass" }],
    }).records;
    expect([...supersededSeqs(records)]).toEqual([[1, 2]]);
  });
});

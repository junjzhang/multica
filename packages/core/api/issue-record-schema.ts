import { z } from "zod";

export const ISSUE_RECORD_KINDS = ["decision", "run"] as const;
export type IssueRecordKind = (typeof ISSUE_RECORD_KINDS)[number];

const FieldValue = z.union([z.string(), z.number(), z.boolean()]);

// Unknown kinds and verdicts from a newer server degrade to readable strings
// rather than dropping the record.
export const IssueRecordSchema = z.object({
  id: z.string(),
  issue_id: z.string(),
  seq: z.number().int().positive(),
  kind: z.string(),
  summary: z.string(),
  rationale: z.string().catch(""),
  evidence: z.array(z.string()).catch([]),
  verdict: z.string(),
  fields: z.record(z.string(), FieldValue).catch({}),
  supersedes_seq: z.number().int().positive().nullable().catch(null),
  author_type: z.string(),
  author_id: z.string(),
  created_at: z.string(),
});
export type IssueRecord = z.infer<typeof IssueRecordSchema>;

// A malformed entry is dropped individually so one bad row cannot hide the
// rest of the ledger.
export const IssueRecordListSchema = z.object({
  records: z
    .array(z.unknown())
    .catch([])
    .transform((rows) =>
      rows.flatMap((row) => {
        const parsed = IssueRecordSchema.safeParse(row);
        return parsed.success ? [parsed.data] : [];
      }),
    ),
});

/** Seqs of records that a later record supersedes. */
export function supersededSeqs(records: readonly IssueRecord[]): Map<number, number> {
  const superseded = new Map<number, number>();
  for (const record of records) {
    if (record.supersedes_seq !== null) superseded.set(record.supersedes_seq, record.seq);
  }
  return superseded;
}

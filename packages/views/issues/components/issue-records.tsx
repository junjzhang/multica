"use client";

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { ChevronRight, ListChecks } from "@multica/ui/icons";
import { useWorkspaceId } from "@multica/core/hooks";
import { issueRecordsOptions } from "@multica/core/issues/records";
import {
  supersededSeqs,
  type IssueRecord,
} from "@multica/core/api/issue-record-schema";
import { useActorName } from "@multica/core/workspace/hooks";
import { Button } from "@multica/ui/components/ui/button";
import { useT } from "../../i18n";

const VERDICT_TONE: Record<string, string> = {
  pass: "text-success",
  keep: "text-success",
  fail: "text-destructive",
  revert: "text-destructive",
};

function isHttpUrl(value: string): boolean {
  try {
    const url = new URL(value);
    return url.protocol === "http:" || url.protocol === "https:";
  } catch {
    return false;
  }
}

export function IssueRecords({ issueId }: { issueId: string }) {
  const wsId = useWorkspaceId();
  const { t } = useT("issues");
  const query = useQuery(issueRecordsOptions(wsId, issueId));
  const [open, setOpen] = useState(true);
  if (query.isPending) return null;
  if (query.isError && !query.data)
    return (
      <div className="space-y-2 px-2 text-caption">
        <p>{t(($) => $.records.load_error)}</p>
        <Button size="xs" variant="outline" onClick={() => query.refetch()}>
          {t(($) => $.records.retry)}
        </Button>
      </div>
    );
  const records = query.data ?? [];
  if (records.length === 0) return null;
  const superseded = supersededSeqs(records);

  return (
    <section
      aria-label={t(($) => $.records.title)}
      className="my-3 min-w-0 space-y-2"
      data-issue-records
    >
      <button
        type="button"
        onClick={() => setOpen(!open)}
        aria-expanded={open}
        className="flex w-full items-center gap-1 px-2 text-caption font-medium text-muted-foreground"
      >
        <ListChecks className="size-3" />
        {t(($) => $.records.title)}
        <span className="tabular-nums">{records.length}</span>
        <ChevronRight
          className={`ml-auto size-3 transition-transform ${open ? "rotate-90" : ""}`}
        />
      </button>
      {open && (
        <ol className="min-w-0 space-y-2 px-2 text-caption">
          {records.map((record) => (
            <RecordRow
              key={record.id}
              record={record}
              supersededBy={superseded.get(record.seq)}
            />
          ))}
        </ol>
      )}
    </section>
  );
}

function RecordRow({
  record,
  supersededBy,
}: {
  record: IssueRecord;
  supersededBy: number | undefined;
}) {
  const { t } = useT("issues");
  const { getActorName } = useActorName();
  const kindLabel =
    record.kind === "run"
      ? t(($) => $.records.kind_run)
      : record.kind === "decision"
        ? t(($) => $.records.kind_decision)
        : record.kind;
  const fields = Object.entries(record.fields);
  return (
    <li
      className={`min-w-0 space-y-1 ${supersededBy ? "text-muted-foreground" : ""}`}
      data-record-seq={record.seq}
    >
      <div className="flex min-w-0 items-baseline gap-1.5">
        <span className="shrink-0 tabular-nums text-muted-foreground">
          #{record.seq}
        </span>
        <span className="shrink-0 text-muted-foreground">{kindLabel}</span>
        <span
          className={`shrink-0 font-medium ${supersededBy ? "" : (VERDICT_TONE[record.verdict] ?? "")}`}
        >
          {record.verdict}
        </span>
        <span
          className={`min-w-0 break-words ${supersededBy ? "line-through" : ""}`}
        >
          {record.summary}
        </span>
      </div>
      {record.rationale && (
        <p className="break-words text-muted-foreground">{record.rationale}</p>
      )}
      {fields.length > 0 && (
        <p className="break-words font-mono text-muted-foreground">
          {fields.map(([key, value]) => `${key}=${String(value)}`).join(" ")}
        </p>
      )}
      {record.evidence.length > 0 && (
        <ul className="min-w-0 space-y-0.5">
          {record.evidence.map((item) => (
            <li key={item} className="truncate font-mono" title={item}>
              {isHttpUrl(item) ? (
                <a
                  href={item}
                  target="_blank"
                  rel="noreferrer"
                  className="underline underline-offset-2"
                >
                  {item}
                </a>
              ) : (
                item
              )}
            </li>
          ))}
        </ul>
      )}
      <p className="text-muted-foreground">
        {getActorName(record.author_type, record.author_id)}
        {record.supersedes_seq !== null &&
          ` · ${t(($) => $.records.supersedes, { seq: record.supersedes_seq })}`}
        {supersededBy !== undefined &&
          ` · ${t(($) => $.records.superseded_by, { seq: supersededBy })}`}
      </p>
    </li>
  );
}

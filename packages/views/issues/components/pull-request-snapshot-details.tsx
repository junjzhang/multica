"use client";

import type { GitHubPullRequest } from "@multica/core/types";
import { useT } from "../../i18n";

type IssuesT = ReturnType<typeof useT<"issues">>["t"];

function reviewLabel(state: string, t: IssuesT): string {
  switch (state) {
    case "approved": return t(($) => $.detail.pull_request_review_approved);
    case "approving": return t(($) => $.detail.pull_request_review_pending);
    case "rejected":
    case "change_denied": return t(($) => $.detail.pull_request_review_rejected);
    case "empty": return t(($) => $.detail.pull_request_review_none);
    default: return t(($) => $.detail.pull_request_review_unknown);
  }
}

function checkLabel(state: string, t: IssuesT): string {
  switch (state) {
    case "success": return t(($) => $.detail.pull_request_check_passed);
    case "failure":
    case "failed":
    case "error":
    case "canceled":
    case "cancelled": return t(($) => $.detail.pull_request_check_failed);
    case "need_approve": return t(($) => $.detail.pull_request_check_approval);
    case "pending":
    case "running": return t(($) => $.detail.pull_request_check_pending);
    default: return t(($) => $.detail.pull_request_check_unknown);
  }
}

function safeCheckURL(raw: string): string | undefined {
  try {
    const url = new URL(raw);
    if (["http:", "https:"].includes(url.protocol) && !url.username && !url.password) return url.href;
  } catch { /* Invalid links remain plain text. */ }
  return undefined;
}

export function PullRequestSnapshotDetails({ pr }: { pr: GitHubPullRequest }) {
  const { t } = useT("issues");
  if (pr.snapshot_available === false || pr.state === "closed" || pr.state === "merged") return null;
  if (!pr.review && !pr.check_details?.length && pr.commit_gate?.blocked !== true) return null;
  return (
    <div className={`space-y-1 pb-1.5 pl-5.5 text-micro text-muted-foreground ${pr.snapshot_stale ? "opacity-60" : ""}`}>
      {pr.mergeable === "mergeable" ? <p>{t(($) => $.detail.pull_request_merge_clear)}</p> : null}
      {pr.commit_gate?.blocked === true ? <p>{t(($) => $.detail.pull_request_ci_gate_blocked)}</p> : null}
      {pr.review ? (
        <details data-testid="pull-request-review">
          <summary className="cursor-pointer">{t(($) => $.detail.pull_request_review_summary, { state: reviewLabel(pr.review.state, t) })}</summary>
          <ul className="space-y-1 pt-1">
            {pr.review.reviewers.map((reviewer, index) => (
              <li key={`${reviewer.username}:${index}`} className="flex min-w-0 justify-between gap-2">
                <span className="truncate">@{reviewer.username}</span>
                <span>{reviewLabel(reviewer.review_state, t)}</span>
              </li>
            ))}
          </ul>
        </details>
      ) : null}
      {pr.check_details?.length ? (
        <details data-testid="pull-request-check-details">
          <summary className="cursor-pointer">{t(($) => $.detail.pull_request_check_details, { count: pr.check_details.length })}</summary>
          <ul className="space-y-1 pt-1">
            {pr.check_details.map((check) => {
              const href = safeCheckURL(check.target_url);
              return (
                <li key={check.id} className="flex min-w-0 justify-between gap-2">
                {href ? (
                  <a className="truncate underline" href={href} target="_blank" rel="noreferrer noopener">{check.context}</a>
                ) : <span className="truncate">{check.context}</span>}
                <span className="shrink-0">{checkLabel(check.state, t)}</span>
              </li>
              );
            })}
          </ul>
        </details>
      ) : null}
    </div>
  );
}

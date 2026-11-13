import { z } from "zod";
export const pullRequestSchema = z.object({
  id: z.string(), number: z.number(), title: z.string(), state: z.string(), repo_owner: z.string(), repo_name: z.string(),
  html_url: z.url().refine((url) => /^https?:\/\//i.test(url)), source_identifier: z.string(), source_url: z.string().optional(),
  checks_conclusion: z.string().nullish(), checks_rollup: z.string().nullish(), checks_failed: z.number().optional(),
  snapshot_stale: z.boolean().optional(), snapshot_available: z.boolean().optional(), mergeable: z.string().nullish(), review: z.object({ state: z.string() }).nullish(),
});
export const pullRequestsSchema = z.object({ pull_requests: z.array(pullRequestSchema) });
export type PullRequest = z.infer<typeof pullRequestSchema>;
export function needsAttention(pr: PullRequest): boolean {
  return (pr.state === "open" || pr.state === "draft") && ((pr.checks_failed ?? 0) > 0 || ["failure", "failed", "error"].includes(pr.checks_conclusion ?? pr.checks_rollup ?? "") || pr.mergeable === "conflicting" || ["pending", "required", "approving", "changes_requested"].includes(pr.review?.state ?? ""));
}

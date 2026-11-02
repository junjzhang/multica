// @vitest-environment node
import { describe, expect, it } from "vitest";
import { GitHubPullRequestSchema } from "./schemas";

const legacy = {
  id: "pr", provider: "tgit", workspace_id: "ws", repo_owner: "org", repo_name: "repo", number: 42,
  title: "MR", state: "open", html_url: "https://git.test/org/repo/merge_requests/42",
  branch: null, author_login: null, author_avatar_url: null, merged_at: null, closed_at: null,
  pr_created_at: "2026-10-26T06:00:00Z", pr_updated_at: "2026-10-26T07:00:00Z",
};

describe("TGit snapshot response compatibility", () => {
  it("keeps cards from older backends that have no snapshot fields", () => {
    const parsed = GitHubPullRequestSchema.parse(legacy);
    expect(parsed.id).toBe("pr");
    expect(parsed.review).toBeUndefined();
    expect(parsed.check_details).toBeUndefined();
  });
  it("degrades malformed additions without dropping the MR", () => {
    const parsed = GitHubPullRequestSchema.parse({ ...legacy, review: { state: 42, reviewers: "bad" }, check_details: "bad", commit_gate: { blocked: "yes" } });
    expect(parsed.id).toBe("pr");
    expect(parsed.review).toEqual({ state: "unknown", reviewers: [] });
    expect(parsed.check_details).toEqual([]);
    expect(parsed.commit_gate).toBeNull();
  });
  it("retains unknown review enums for the UI fallback", () => {
    const parsed = GitHubPullRequestSchema.parse({ ...legacy, review: { state: "future-state", reviewers: [{ username: "alice", type: "future-role", review_state: "future-vote" }] } });
    expect(parsed.review?.state).toBe("future-state");
    expect(parsed.review?.reviewers[0]?.review_state).toBe("future-vote");
  });
});

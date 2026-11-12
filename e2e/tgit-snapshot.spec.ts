import { randomUUID } from "node:crypto";
import { test, expect } from "@playwright/test";
import pg from "pg";
import { createTestApi, loginAsDefault, waitForPageText } from "./helpers";

const databaseURL = process.env.DATABASE_URL ?? "postgres://multica:multica@localhost:5432/multica?sslmode=disable";

test("TGit snapshots render independent CI, review and conflict status", async ({ page }, testInfo) => {
  test.setTimeout(120000);
  const api = await createTestApi();
  const db = new pg.Client(databaseURL);
  await db.connect();
  const connectionId = randomUUID();
  const prId = randomUUID();
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  try {
    const issue = await api.createIssue("Review authentication update");
    await db.query(`INSERT INTO vcs_connection(id,workspace_id,provider,instance_url,account_login,access_token_encrypted,webhook_secret_encrypted)
      VALUES($1,$2,'tgit','https://git.example.com','demo','fixture','fixture')`, [connectionId, issue.workspace_id]);
    await db.query(`INSERT INTO vcs_pull_request(id,workspace_id,connection_id,provider,repo_owner,repo_name,pr_number,title,state,html_url,head_sha,pr_created_at,pr_updated_at,snapshot_refresh_after)
      VALUES($1,$2,$3,'tgit','team','service',42,'Validate incoming requests','open','https://git.example.com/team/service/merge_requests/42','head','2026-10-26T06:00:00Z','2026-10-26T07:00:00Z',now()+interval '1 day')`, [prId, issue.workspace_id, connectionId]);
    await db.query("INSERT INTO issue_vcs_pull_request(issue_id,pull_request_id,linked_by_type) VALUES($1,$2,'member')", [issue.id, prId]);
    await page.setViewportSize({ width: 1440, height: 1000 });
    const slug = await loginAsDefault(page);
    await page.goto(`/${slug}/issues/${issue.id}`, { waitUntil: "domcontentloaded" });
    await waitForPageText(page, issue.title);
    const card = page.getByTestId("pull-request-row").locator("..");
    await expect(card).toContainText("Validate incoming requests");
    await expect(page.getByTestId("pull-request-review")).toHaveCount(0);
    await card.screenshot({ path: testInfo.outputPath("before-snapshot.png") });
    const snapshot = {
      head_sha: "head", target_sha: "target", mr_updated_at: "2026-10-26T07:00:00Z",
      commit_gate: { state: "pending", blocked: true }, check_aggregate: { state: "pending", blocked: true },
      checks: [
        { id: 1, context: "Unit tests", state: "success", target_url: "https://ci.example.com/tests" },
        { id: 2, context: "Release gate", state: "need_approve", target_url: "https://ci.example.com/approval" },
      ],
      review: { state: "approving", reviewers: [
        { username: "alice", type: "necessary", review_state: "approved" },
        { username: "bob", type: "necessary", review_state: "approving" },
      ] }, has_conflicts: true,
    };
    await db.query("UPDATE vcs_pull_request SET api_snapshot=$2,snapshot_fetched_at=now(),snapshot_error=false WHERE id=$1", [prId, JSON.stringify(snapshot)]);
    await page.reload({ waitUntil: "domcontentloaded" });
    await expect(card).toContainText("Conflicts");
    await expect(card).toContainText("Review: Pending");
    await card.getByText("Review: Pending", { exact: true }).click();
    await card.getByText("Checks (2)", { exact: true }).click();
    await expect(card.getByText("@alice", { exact: true })).toBeVisible();
    await expect(card.getByText("Awaiting CI approval", { exact: true })).toBeVisible();
    await expect(card.getByText("CI gate blocks merging", { exact: true })).toBeVisible();
    expect(await card.evaluate((node) => node.scrollWidth <= node.clientWidth + 1)).toBe(true);
    await card.screenshot({ path: testInfo.outputPath("after-snapshot.png") });
    expect(errors).toEqual([]);
  } finally {
    await db.query("DELETE FROM issue_vcs_pull_request WHERE pull_request_id=$1", [prId]);
    await db.query("DELETE FROM vcs_pull_request WHERE id=$1", [prId]);
    await db.query("DELETE FROM vcs_connection WHERE id=$1", [connectionId]);
    await db.end();
    await api.cleanup();
  }
});

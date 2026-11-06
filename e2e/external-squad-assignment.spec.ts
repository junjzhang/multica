import { test, expect } from "@playwright/test";
import { execFile } from "node:child_process";
import { writeFile, rm } from "node:fs/promises";
import { promisify } from "node:util";
import pg from "pg";
import { createTestApi, loginAsDefault } from "./helpers";

test("explicit external squad recipient reaches the original isolated host", async ({ page }, testInfo) => {
  test.setTimeout(120_000);
  page.setDefaultTimeout(15_000);
  const api = await createTestApi();
  const squadName = `External acceptance ${Date.now()}`;
  const fixture = await api.seedExternalSquad(squadName);
  const issue = await api.createIssue("Browser external squad assignment");
  const input = testInfo.outputPath("isolated-assignment.json");
  const db = new pg.Client(process.env.DATABASE_URL);
  await db.connect();
  try {
    const slug = await loginAsDefault(page);
    await page.goto(`/${slug}/issues/${issue.identifier}`);
    await page.getByRole("button", { name: "Unassigned", exact: true }).click();
    await page.getByRole("button", { name: squadName, exact: true }).click();
    await expect(page.getByRole("radio", { name: /Chosen receipt session/ })).not.toBeChecked();
    await expect(page.getByRole("radio", { name: /Other session/ })).not.toBeChecked();
    await page.screenshot({ path: testInfo.outputPath("before-session-selection.png") });
    await page.getByText("Chosen receipt session", { exact: true }).click();
    await expect(page.getByRole("radio", { name: /Chosen receipt session/ })).toBeChecked();
    await page.screenshot({ path: testInfo.outputPath("selected-recipient.png") });
    await page.getByRole("button", { name: "Save session", exact: true }).click();
    const assigned = page.waitForResponse((response) => response.request().method() === "PUT" && response.url().endsWith(`/api/issues/${issue.id}`));
    await page.getByRole("button", { name: "Confirm assignment", exact: true }).click();
    const response = await assigned;
    expect(response.ok(), await response.text()).toBeTruthy();
    expect(response.request().postDataJSON().recipient_session_id).toBe(fixture.sessionId);
    const pending = await db.query(`SELECT id, target_session_id, kind FROM agent_mailbox_message WHERE issue_id=$1`, [issue.id]);
    expect(pending.rows).toHaveLength(1);
    expect(pending.rows[0].target_session_id).toBe(fixture.sessionId);
    expect(pending.rows[0].kind).toBe("issue_assignment");
    await writeFile(input, JSON.stringify({
      ServerURL: process.env.NEXT_PUBLIC_API_URL,
      Token: api.getToken(), WorkspaceID: fixture.workspaceId, DaemonID: fixture.daemonId,
      IssueID: issue.id, SquadID: fixture.squadId,
    }), { mode: 0o600 });
    const { stdout } = await promisify(execFile)("go", ["test", "./internal/daemon", "-run", "^TestExternalSquadBrowserAssignmentHostReceipt$", "-count=1", "-v"], {
      cwd: "server", env: { ...process.env, CGO_ENABLED: "0", MULTICA_TEST_ASSIGNMENT_INPUT: input }, timeout: 45_000,
    });
    expect(stdout).toContain("browser-selected recipient -> exact isolated host session -> accepted host receipt");
    await testInfo.attach("host-receipt-validation", { body: stdout, contentType: "text/plain" });
    const accepted = await db.query(`SELECT delivery_status, host_receipt FROM agent_mailbox_message WHERE id=$1`, [pending.rows[0].id]);
    expect(accepted.rows[0].delivery_status).toBe("delivered");
    expect(accepted.rows[0].host_receipt).toContain("paseo:chosen-host-session:");
    expect((await db.query(`SELECT count(*)::int AS n FROM agent_task_queue WHERE issue_id=$1`, [issue.id])).rows[0].n).toBe(0);
  } finally {
    await rm(input, { force: true });
    await db.end();
    await api.cleanup();
  }
});

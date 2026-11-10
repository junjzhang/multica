import { test, expect } from "@playwright/test";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { writeFile } from "node:fs/promises";
import { randomUUID } from "node:crypto";
import { createTestApi, loginAsDefault, waitForPageText } from "./helpers";

for (const kind of ["managed", "external"] as const) {
  test(`${kind} squad contract keeps real CLI authorship and updates its representative`, async ({ page }, info) => {
    test.setTimeout(180_000);
    const binary = process.env.MULTICA_CONTRACT_TEST_CLI;
    test.skip(!binary, "Build the local Multica CLI and set MULTICA_CONTRACT_TEST_CLI");
    const api = await createTestApi();
    try {
      const issue = await api.createIssue(`${kind} squad contract`, { status: "backlog" });
      const actor = kind === "managed"
        ? await api.contractSquadAgent(issue.id)
        : await api.contractExternalSquadAgent(issue.id);
      const before = await api.getIssue(issue.id);
      const env = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("MULTICA_") && !key.startsWith("PASEO_")));
      Object.assign(env, {
        MULTICA_SERVER_URL: actor.serverUrl, MULTICA_WORKSPACE_ID: actor.workspaceId,
        MULTICA_TOKEN: actor.token, MULTICA_AGENT_ID: actor.agentId,
      });
      if (actor.taskId) env.MULTICA_TASK_ID = actor.taskId;
      const cli = async (...args: string[]) => {
        const { stdout } = await promisify(execFile)(binary!, ["issue", "contract", ...args], { cwd: process.cwd(), env, timeout: 15_000 });
        return JSON.parse(stdout);
      };
      const slug = await loginAsDefault(page);
      await page.setViewportSize({ width: 1440, height: 1000 });
      await page.goto(`/${slug}/issues/${issue.identifier}`, { waitUntil: "domcontentloaded" });
      await waitForPageText(page, `${kind} squad contract`);
      const section = page.locator("[data-issue-contract]");
      await expect(section.getByText("No contract", { exact: true })).toBeVisible();
      await page.screenshot({ path: info.outputPath("before-contract.png"), fullPage: true });
      await section.getByRole("button", { name: "Create contract", exact: true }).click();
      const dialog = page.getByRole("dialog");
      await dialog.getByLabel("Goal", { exact: true }).fill("Deliver as a squad with a real agent author");
      await dialog.getByLabel("Acceptance criteria (one per line)").fill("Real author verified");
      await dialog.getByLabel("Authorization notes").fill("Submit test evidence only");
      await dialog.getByRole("button", { name: "Save version" }).click();
      await expect(dialog).not.toBeVisible();
      await expect(section.getByText("Squad representative", { exact: true })).toBeVisible();
      const initial = await cli("get", issue.id);
      expect(initial.executor).toEqual({ type: "squad", id: actor.squadId });
      expect(initial.executor_representative).toEqual({ type: "agent", id: actor.agentId });
      expect(initial.can_submit).toBe(true);
      expect(initial.can_edit).toBe(false);
      const requestId = randomUUID();
      const requestPath = info.outputPath("squad-delivery.json");
      await writeFile(requestPath, JSON.stringify({ action: "submit", client_request_id: requestId, expected_sequence: 1, version: 1, criteria: [0], evidence: `${kind} CLI delivery evidence` }));
      const submitted = await cli("apply", issue.id, "--request-file", requestPath);
      expect(submitted.contract.deliveries[0].author).toEqual({ type: "agent", id: actor.agentId });
      await expect(section.getByRole("button", { name: "Accept selected" })).toBeVisible();
      await section.getByText("Version and delivery history", { exact: true }).click();
      await expect(section.getByText("Submitted by", { exact: true })).toBeVisible();
      await expect(section.getByText(`${kind} CLI delivery evidence`, { exact: true })).toBeVisible();
      await page.screenshot({ path: info.outputPath("squad-awaiting-acceptance.png"), fullPage: true });
      await section.getByRole("checkbox", { name: "Real author verified", exact: true }).check();
      await section.getByRole("button", { name: "Accept selected" }).click();
      await expect(section.getByText("All criteria accepted", { exact: true })).toBeVisible();
      const accepted = await api.getIssue(issue.id);
      expect(accepted.status).toBe("done");
      expect(accepted.assignee_type).toBe("squad");
      expect(accepted.assignee_id).toBe(actor.squadId);
      if (kind === "external") expect(accepted.external_session_bindings).toEqual(before.external_session_bindings);
      const replacementIssue = await api.createIssue("Replacement leader fixture");
      const replacement = await api.contractAgent(replacementIssue.id);
      await api.updateSquadLeader(actor.squadId, replacement.agentId);
      await expect(section.getByText("Agreement changed · waiting for revision", { exact: true })).toBeVisible();
      const changed = await cli("get", issue.id);
      expect(changed.stale).toBe(true);
      expect(changed.can_submit).toBe(false);
      expect(changed.executor_representative.id).toBe(replacement.agentId);
      expect(changed.contract.deliveries[0].author.id).toBe(actor.agentId);
      if (kind === "external") expect((await api.getIssue(issue.id)).external_session_bindings).toEqual(before.external_session_bindings);
      expect(await section.evaluate((element) => element.scrollWidth <= element.clientWidth + 1)).toBe(true);
    } finally { await api.cleanup(); }
  });
}

import { test, expect } from "@playwright/test";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { randomUUID } from "node:crypto";
import { createTestApi, loginAsDefault, waitForPageText } from "./helpers";

for (const kind of ["managed", "external"] as const) {
  test(`${kind} squad contract keeps real CLI authorship and updates its representative`, async ({ page }, info) => {
    test.setTimeout(180_000);
    const binary = process.env.MULTICA_CONTRACT_TEST_CLI;
    test.skip(!binary, "Build the local Multica CLI and set MULTICA_CONTRACT_TEST_CLI");
    const api = await createTestApi();
    const cliDirectory = await mkdtemp(join(tmpdir(), "multica-squad-contract-"));
    try {
      const issue = await api.createIssue(`${kind} squad contract`, { status: "backlog" });
      const actor = kind === "managed"
        ? await api.contractSquadAgent(issue.id)
        : await api.contractExternalSquadAgent(issue.id);
      const before = await api.getIssue(issue.id);
      const env = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("MULTICA_") && !key.startsWith("PASEO_")));
      if ("tokenId" in actor) {
        const binding = join(cliDirectory, "session.json");
        await writeFile(binding, JSON.stringify({
          version: 1, provider: "paseo", host_id: "isolated-host", session_id: "chosen-host-session",
          agent_id: actor.agentId, daemon_id: actor.daemonId, server_url: actor.serverUrl,
          app_url: actor.serverUrl, workspace_id: actor.workspaceId, token: actor.token,
          token_id: actor.tokenId, expires_at: new Date(Date.now() + 600_000).toISOString(),
        }), { mode: 0o600 });
        env.PASEO_AGENT_ID = "chosen-host-session";
        env.MULTICA_EXTERNAL_SESSION_FILE = binding;
      } else {
        Object.assign(env, {
          MULTICA_SERVER_URL: actor.serverUrl, MULTICA_WORKSPACE_ID: actor.workspaceId,
          MULTICA_TOKEN: actor.token, MULTICA_AGENT_ID: actor.agentId, MULTICA_TASK_ID: actor.taskId,
        });
      }
      const cli = async (...args: string[]) => {
        const { stdout } = await promisify(execFile)(binary!, ["issue", "contract", ...args], { cwd: cliDirectory, env, timeout: 15_000 });
        return JSON.parse(stdout);
      };
      const slug = await loginAsDefault(page);
      await page.setViewportSize({ width: 1440, height: 1200 });
      await page.goto(`/${slug}/issues/${issue.identifier}`, { waitUntil: "domcontentloaded" });
      await waitForPageText(page, `${kind} squad contract`);
      const section = page.locator("[data-issue-contract]");
      await expect(section.getByText("No contract", { exact: true })).toBeVisible();
      await page.screenshot({ path: info.outputPath("before-contract.png"), fullPage: true, animations: "disabled" });
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
      const requestPath = join(cliDirectory, "squad-delivery.json");
      await writeFile(requestPath, JSON.stringify({ action: "submit", client_request_id: requestId, expected_sequence: 1, version: 1, criteria: [0], evidence: `${kind} CLI delivery evidence` }));
      const submitted = await cli("apply", issue.id, "--request-file", requestPath);
      expect(submitted.contract.deliveries[0].author).toEqual({ type: "agent", id: actor.agentId });
      await expect(section.getByRole("button", { name: "Accept selected" })).toBeVisible();
      await section.getByText("Version and delivery history", { exact: true }).click();
      await expect(section.getByText("Submitted by", { exact: true })).toBeVisible();
      await expect(section.getByText(`${kind} CLI delivery evidence`, { exact: true }).last()).toBeVisible();
      await page.screenshot({ path: info.outputPath("squad-awaiting-acceptance.png"), fullPage: true, animations: "disabled" });
      await section.getByRole("checkbox", { name: "Real author verified", exact: true }).check();
      await section.getByRole("button", { name: "Accept selected" }).click();
      await expect(section.getByText("All criteria accepted", { exact: true })).toBeVisible();
      const accepted = await api.getIssue(issue.id);
      expect(accepted.status).toBe("done");
      expect(accepted.assignee_type).toBe("squad");
      expect(accepted.assignee_id).toBe(actor.squadId);
      if (kind === "external") expect(accepted.external_session_bindings).toEqual(before.external_session_bindings);
      if (kind === "external") {
        await page.setViewportSize({ width: 390, height: 844 });
        await page.getByRole("button", { name: "Toggle right sidebar" }).click();
        await expect(section.getByText("Squad representative", { exact: true })).toBeVisible();
        expect(await section.evaluate((element) => element.scrollWidth <= element.clientWidth + 1)).toBe(true);
        await page.screenshot({ path: info.outputPath("squad-mobile.png"), fullPage: true, animations: "disabled" });
        await page.keyboard.press("Escape");
        await page.setViewportSize({ width: 1440, height: 1200 });
      }
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
    } finally {
      try { await api.cleanup(); }
      finally { await rm(cliDirectory, { recursive: true, force: true }); }
    }
  });
}

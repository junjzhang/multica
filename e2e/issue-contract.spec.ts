import { test, expect, type BrowserContext } from "@playwright/test";
import { createTestApi, loginAsDefault, waitForPageText } from "./helpers";
import type { TestApiClient } from "./fixtures";

test.describe("Issue contract", () => {
  test.use({ actionTimeout: 15_000 });
  let api: TestApiClient;
  let executorContext: BrowserContext | undefined;
  test.beforeEach(async () => {
    api = await createTestApi();
  });
  test.afterEach(async () => {
    await executorContext?.close();
    await api.cleanup();
  });

  test("sidebar agreement, delivery, return and partial acceptance survive refresh", async ({
    page,
    browser,
  }, info) => {
    test.setTimeout(180_000);
    const member = await api.contractMember(
      `contract-executor-${Date.now()}@multica.ai`,
      "Delivery Owner",
    );
    const issue = await api.createIssue("Contract browser acceptance", {
      assignee_type: "member",
      assignee_id: member.userId,
    });
    const child = await api.createIssue("Covered delivery", {
      parent_issue_id: issue.id,
      status: "in_review",
    });
    const independent = await api.createIssue("Independent follow-up", {
      parent_issue_id: issue.id,
      status: "blocked",
    });
    const slug = await loginAsDefault(page);
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.goto(`/${slug}/issues/${issue.identifier}`, {
      waitUntil: "domcontentloaded",
    });
    await waitForPageText(page, "Contract browser acceptance");
    await expect(page).toHaveURL(new RegExp(`/issues/${issue.identifier}$`));
    const section = page.locator("[data-issue-contract]");
    await expect(
      section.getByText("No contract", { exact: true }),
    ).toBeVisible();
    await page.screenshot({
      path: info.outputPath("before-contract.png"),
      fullPage: true,
    });
    await section
      .getByRole("button", { name: "Create contract", exact: true })
      .click();
    const dialog = page.getByRole("dialog");
    await dialog
      .getByLabel("Goal", { exact: true })
      .fill("Deliver a versioned agreement with evidence. ".repeat(10));
    await dialog
      .getByLabel("Scope", { exact: true })
      .fill("Shared issue details, API and CLI. ".repeat(8));
    await dialog
      .getByLabel("Acceptance criteria (one per line)")
      .fill("API and child delivery\nRefresh and browser evidence");
    await dialog
      .getByLabel("Authorization notes")
      .fill("Implement, test and submit. No production deploy or merge.");
    await dialog.getByRole("button", { name: "Save version" }).click();
    await expect(dialog).not.toBeVisible();
    await expect(
      section.getByText("0/2 accepted", { exact: true }),
    ).toBeVisible();
    await expect(
      section.getByRole("button", { name: "Contract", exact: true }),
    ).toHaveAttribute("aria-expanded", "true");
    await expect(
      section.getByRole("button", { name: "Submit delivery" }),
    ).toHaveCount(0);
    await section.getByText("Goal and scope", { exact: true }).click();
    await page.screenshot({
      path: info.outputPath("contract-long-content.png"),
      fullPage: true,
    });

    executorContext = await browser.newContext({
      viewport: { width: 1440, height: 1000 },
    });
    await executorContext.addInitScript((token) => {
      localStorage.setItem("multica_token", token!);
      localStorage.setItem("multica:chat:isOpen", "false");
    }, member.api.getToken());
    const executor = await executorContext.newPage();
    await executor.goto(`/${slug}/issues/${issue.identifier}`, {
      waitUntil: "domcontentloaded",
    });
    await expect(executor).toHaveURL(
      new RegExp(`/issues/${issue.identifier}$`),
    );
    const execution = executor.locator("[data-issue-contract]");
    const submit = async (
      criteria: string[],
      evidence: string,
      includeChild = false,
    ) => {
      for (const criterion of criteria)
        await execution
          .getByRole("checkbox", { name: criterion, exact: true })
          .check();
      await execution
        .getByLabel("Delivery evidence", { exact: true })
        .fill(evidence);
      if (includeChild)
        await execution
          .getByRole("checkbox", { name: /Cover child delivery:/ })
          .check();
      if (includeChild)
        await executor.screenshot({
          path: info.outputPath("executor-form.png"),
          fullPage: true,
        });
      await expect(
        execution.getByLabel("Delivery evidence", { exact: true }),
      ).toHaveValue(evidence);
      await execution
        .getByRole("button", { name: "Submit delivery", exact: true })
        .click();
      await expect(
        execution.getByText("Waiting for acceptance", { exact: true }),
      ).toBeVisible();
    };
    await submit(["API and child delivery"], "Initial API evidence", true);
    await page.reload({ waitUntil: "domcontentloaded" });
    await section
      .getByRole("checkbox", { name: "API and child delivery", exact: true })
      .check();
    await section
      .getByRole("checkbox", { name: `Accept and close child ${child.id}` })
      .check();
    await section.getByRole("button", { name: "Accept selected" }).click();
    await expect(
      section.getByText("1/2 accepted", { exact: true }),
    ).toBeVisible();
    await executor.reload({ waitUntil: "domcontentloaded" });
    await submit(["Refresh and browser evidence"], "Needs more evidence");
    await page.reload({ waitUntil: "domcontentloaded" });
    await section
      .getByLabel("Review note (required for return)")
      .fill("Include refreshed screenshots.");
    await section.getByRole("button", { name: "Return delivery" }).click();
    await expect(
      section.getByText("Waiting for delivery", { exact: true }),
    ).toBeVisible();
    await executor.reload({ waitUntil: "domcontentloaded" });
    await submit(
      ["Refresh and browser evidence"],
      "Refreshed browser evidence",
    );
    await page.reload({ waitUntil: "domcontentloaded" });
    await section
      .getByRole("checkbox", {
        name: "Refresh and browser evidence",
        exact: true,
      })
      .check();
    await page.screenshot({
      path: info.outputPath("awaiting-acceptance.png"),
      fullPage: true,
    });
    await section.getByRole("button", { name: "Accept selected" }).click();
    await expect(
      section.getByText("All criteria accepted", { exact: true }),
    ).toBeVisible();
    await page.reload({ waitUntil: "domcontentloaded" });
    await expect(
      section.getByText("2/2 accepted", { exact: true }),
    ).toBeVisible();
    await section
      .getByText("Version and delivery history", { exact: true })
      .click();
    await expect(
      section.getByText("Include refreshed screenshots.", { exact: true }),
    ).toBeVisible();
    await page.screenshot({
      path: info.outputPath("accepted-history.png"),
      fullPage: true,
    });
    expect((await api.getIssue(issue.id)).status).toBe("done");
    expect((await api.getIssue(child.id)).status).toBe("done");
    expect((await api.getIssue(independent.id)).status).toBe("blocked");
    await page.setViewportSize({ width: 390, height: 844 });
    await page.getByRole("button", { name: "Toggle right sidebar" }).click();
    await expect(
      section.getByText("2/2 accepted", { exact: true }),
    ).toBeVisible();
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= window.innerWidth + 1,
      ),
    ).toBe(true);
    await page.screenshot({
      path: info.outputPath("mobile-web-contract.png"),
      fullPage: true,
    });
  });
});

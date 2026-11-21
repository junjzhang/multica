import { test, expect } from "@playwright/test";
import { createTestApi, loginAsDefault, waitForPageText } from "./helpers";
import type { TestApiClient } from "./fixtures";

test.describe("Issue records", () => {
  let api: TestApiClient;
  test.beforeEach(async () => {
    api = await createTestApi();
  });
  test.afterEach(async () => {
    await api.cleanup();
  });

  test("issue details show records with their corrections", async ({
    page,
  }, info) => {
    const issue = await api.createIssue("Tune attention backward kernel");
    await api.addIssueRecord(issue.id, {
      kind: "run",
      summary: "FA3 dQ repeat on 8xH20",
      rationale: "Check whether the 128x64 tile regression reproduces",
      evidence: ["https://example.com/ci-run/0817b8c4c486", "commit 3f2a91c"],
      verdict: "pending",
      fields: { protocol: "v1", gpus: 8 },
    });
    await api.addIssueRecord(issue.id, {
      kind: "decision",
      summary: "Keep 128x64 tile",
      rationale: "Backward is 6% faster and numerics match within atol",
      verdict: "keep",
    });
    await api.addIssueRecord(issue.id, {
      kind: "run",
      summary: "FA3 dQ repeat concluded",
      verdict: "pass",
      fields: { speedup: 1.06 },
      supersedes_seq: 1,
    });

    const slug = await loginAsDefault(page);
    await page.goto(`/${slug}/issues/${issue.identifier}`, {
      waitUntil: "domcontentloaded",
    });
    await waitForPageText(page, "Tune attention backward kernel");

    const section = page.getByRole("region", { name: "Records" });
    await expect(section).toBeVisible();
    await expect(section.locator("[data-record-seq]")).toHaveCount(3);
    const superseded = section.locator('[data-record-seq="1"]');
    await expect(superseded.getByText("superseded by #3")).toBeVisible();
    await expect(
      section.locator('[data-record-seq="3"]').getByText("corrects #1"),
    ).toBeVisible();
    await expect(
      superseded.getByRole("link", {
        name: "https://example.com/ci-run/0817b8c4c486",
      }),
    ).toBeVisible();

    await info.attach("issue-records", {
      body: await section.screenshot(),
      contentType: "image/png",
    });
    if (process.env.ISSUE_RECORDS_SCREENSHOT_DIR) {
      await page.screenshot({
        path: `${process.env.ISSUE_RECORDS_SCREENSHOT_DIR}/issue-records-page.png`,
      });
      await section.screenshot({
        path: `${process.env.ISSUE_RECORDS_SCREENSHOT_DIR}/issue-records-section.png`,
      });
    }
  });
});

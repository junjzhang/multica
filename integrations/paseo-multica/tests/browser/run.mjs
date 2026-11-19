import { build } from "esbuild";
import { chromium, expect } from "@playwright/test";
import { createServer } from "node:http";
import { mkdir } from "node:fs/promises";
import { resolve, join } from "node:path";

const output = resolve(process.env.BROWSER_EVIDENCE_DIR ?? "tests/browser/artifacts");
await mkdir(output, { recursive: true });
const bundle = await build({
  entryPoints: ["tests/browser/fixture.tsx"], bundle: true, write: false, jsx: "automatic", platform: "browser",
  alias: { "react-native": resolve("tests/browser/native.tsx"), "@getpaseo/plugin/client/ui": resolve("tests/browser/rpc.tsx"), "@getpaseo/plugin/client": resolve("tests/browser/rpc.tsx") },
});
const server = createServer((req, res) => {
  res.setHeader("Content-Type", "text/html");
  if (req.url?.startsWith("/acme/issues/")) {
    res.end(`<html><body style="font:20px system-ui"><h1>${req.url.split("/").at(-1)}</h1><p>Task detail fixture</p></body></html>`);
    return;
  }
  res.end(`<html><head><style>body{font:15px system-ui;margin:24px;color:#17202a}nav button{padding:8px;margin:4px;border:1px solid #ccc;border-radius:6px;background:#f7f8fa;cursor:pointer}aside{min-width:400px}</style></head><body><div id="root"></div><script>${bundle.outputFiles[0].text.replace(/<\/script/gi, "<\\/script")}</script></body></html>`);
});
await new Promise(done => server.listen(0, "127.0.0.1", done));
let browser;
try {
  browser = await chromium.launch({ ...(process.env.PLAYWRIGHT_BROWSER_EXECUTABLE ? { executablePath: process.env.PLAYWRIGHT_BROWSER_EXECUTABLE } : {}), headless: true });
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
  const errors = [];
  page.on("pageerror", error => errors.push(error.message));
  await page.goto(`http://127.0.0.1:${server.address().port}`);
  async function openTask(name) {
    await page.getByRole("link", { name: new RegExp(`Only ${name}`) }).click();
  }
  await expect(page.getByText("Only session-a", { exact: false })).toBeVisible();
  await expect(page.getByText("投递失败：会话已归档 · 在 Paseo 中打开")).toBeVisible();
  await page.screenshot({ path: join(output, "archived-delivery.png") });
  await page.getByRole("link", { name: /WORK-3.+会话已归档/ }).click();
  await expect(page.locator("body")).toHaveAttribute("data-opened-agent", "same-host/archived-session");
  await openTask("session-a");
  await expect(page.frameLocator("iframe").getByRole("heading", { name: "WORK-1" })).toBeVisible();
  await page.screenshot({ path: join(output, "session-a-details.png") });
  await page.getByRole("button", { name: "session-b", exact: true }).click();
  await expect(page.getByText("Only session-b", { exact: false })).toBeVisible();
  await expect(page.getByText("Only session-a", { exact: false })).toHaveCount(0);
  await expect(page.getByText("投递失败：会话已归档 · 在 Paseo 中打开")).toHaveCount(0);
  await expect(page.locator("iframe")).toHaveCount(0);
  await expect(page.getByText("Select a task to open its details.")).toBeVisible();
  await openTask("session-b");
  await expect(page.frameLocator("iframe").getByRole("heading", { name: "WORK-2" })).toBeVisible();
  await page.screenshot({ path: join(output, "session-b-details.png") });
  await page.getByRole("button", { name: "session-a", exact: true }).click();
  await expect(page.frameLocator("iframe").getByRole("heading", { name: "WORK-1" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Back", exact: true })).toHaveCount(0);
  await page.getByRole("button", { name: "empty", exact: true }).click();
  await expect(page.getByText("暂无任务", { exact: true })).toBeVisible();
  await expect(page.locator("iframe")).toHaveCount(0);
  await page.screenshot({ path: join(output, "empty-session.png") });
  await page.getByRole("button", { name: "disconnected", exact: true }).click();
  await expect(page.getByText("This Paseo session has no Multica binding.", { exact: false })).toBeVisible();
  await expect(page.locator("iframe")).toHaveCount(0);
  await page.getByRole("button", { name: "mr", exact: true }).click();
  const panel = page.locator("section");
  const body = page.locator("body");
  const failedChip = page.getByRole("link", { name: "打开 sglang!833 !833 CI 失败" });
  await expect(failedChip).toBeVisible();
  await expect(page.getByRole("link", { name: "打开 slime!16 !16 有冲突" })).toBeVisible();
  await expect(page.getByRole("link", { name: /^打开 sglang!901 !901 CI 失败/ })).toBeVisible();
  await expect(page.getByRole("link", { name: "打开 gw!41 !41 待你审批" })).toBeVisible();
  const sections = await panel.innerText();
  expect(sections.indexOf("需要你")).toBeGreaterThanOrEqual(0);
  expect(sections.indexOf("需要你")).toBeLessThan(sections.indexOf("待本会话处理"));
  expect(sections.slice(sections.indexOf("待本会话处理"))).toContain("复核调度器补丁");
  await expect(page.getByRole("button", { name: "打开 WORK-841 的执行会话" })).toHaveCount(0);
  await page.mouse.move(0, 0);
  await panel.screenshot({ path: join(output, "mr-light.png") });
  const sessionEntry = page.getByRole("button", { name: "打开 WORK-840 的执行会话" });
  await sessionEntry.hover();
  await expect(sessionEntry).toContainText("会话");
  await panel.screenshot({ path: join(output, "session-entry-hover.png") });
  await sessionEntry.click();
  await expect(body).toHaveAttribute("data-opened-agent", "same-host/worker-session");
  await expect(page.locator("iframe")).toHaveCount(0);
  await page.getByRole("button", { name: "打开 WORK-830 的执行会话" }).click();
  await expect(body).toHaveAttribute("data-opened-agent", "same-host/worker-session");
  await expect(page.locator("iframe")).toHaveCount(0);
  await failedChip.hover();
  await expect(failedChip.locator("span", { hasText: "!833 CI 失败" })).toHaveCSS("text-decoration-line", "underline");
  await panel.screenshot({ path: join(output, "mr-chip-hover.png") });
  await failedChip.click();
  await expect(body).toHaveAttribute("data-opened-external-url", "https://git.woa.com/o/sglang/merge_requests/833");
  await expect(page.locator("iframe")).toHaveCount(0);
  await page.getByRole("link", { name: "WORK-831 已完成 接入 CI 结果" }).click();
  await expect(page.frameLocator("iframe").getByRole("heading", { name: "WORK-831" })).toBeVisible();
  await expect(body).toHaveAttribute("data-opened-external-url", "https://git.woa.com/o/sglang/merge_requests/833");
  const summary = page.getByRole("button", { name: /^4 个 MR / });
  await expect(summary).toHaveAttribute("aria-expanded", "false");
  await summary.click();
  await expect(summary).toHaveAttribute("aria-expanded", "true");
  for (const name of ["sglang!833 · WORK-831 接入 CI 结果 CI 失败", "slime!16 · WORK-832 迁移调度配置 有冲突", "gw!41 · WORK-835 灰度开关 待你审批", "mimikyu!20 · WORK-834 补齐压测脚本 CI 运行中"]) {
    await expect(page.getByRole("link", { name, exact: true })).toBeVisible();
  }
  await page.getByRole("link", { name: "mimikyu!20 · WORK-834 补齐压测脚本 CI 运行中" }).click();
  await expect(body).toHaveAttribute("data-opened-external-url", "https://git.woa.com/o/mimikyu/merge_requests/20");
  await page.mouse.move(0, 0);
  await panel.screenshot({ path: join(output, "mr-parent-summary.png") });
  await page.getByRole("button", { name: "theme", exact: true }).click();
  await panel.screenshot({ path: join(output, "mr-dark.png") });
  expect(errors).toEqual([]);
  console.log("PASS: Chromium same-host/workspace session tasks, detail history, empty and disconnected states, MR chips, session entries, split to-dos");
} finally {
  if (browser) await browser.close();
  await new Promise(done => server.close(done));
}

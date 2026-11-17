import { build } from "esbuild";
import { chromium, expect } from "@playwright/test";
import { createServer } from "node:http";
import { mkdir } from "node:fs/promises";
import { resolve, join } from "node:path";

const output = resolve(process.env.BROWSER_EVIDENCE_DIR ?? "tests/browser/artifacts");
await mkdir(output, { recursive: true });
const bundle = await build({
  entryPoints: ["tests/browser/fixture.tsx"], bundle: true, write: false, jsx: "automatic", platform: "browser",
  alias: { "react-native": resolve("tests/browser/native.tsx"), "@getpaseo/plugin/client/ui": resolve("tests/browser/rpc.ts"), "@getpaseo/plugin/client": resolve("tests/browser/rpc.ts") },
});
const server = createServer((req, res) => {
  res.setHeader("Content-Type", "text/html");
  if (req.url?.startsWith("/acme/issues/")) {
    res.end(`<html><body style="font:20px system-ui"><h1>${req.url.split("/").at(-1)}</h1><p>Task detail fixture</p></body></html>`);
    return;
  }
  res.end(`<html><head><style>body{font:15px system-ui;margin:24px;color:#17202a}button{padding:8px;margin:4px;border:1px solid #ccc;border-radius:6px;background:#f7f8fa;cursor:pointer}aside{min-width:400px}</style></head><body><div id="root"></div><script>${bundle.outputFiles[0].text.replace(/<\/script/gi, "<\\/script")}</script></body></html>`);
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
    await page.getByRole("button", { name: new RegExp(`Only ${name}`) }).click();
    await page.getByRole("link", { name: "Open task", exact: true }).click();
  }
  await expect(page.getByText("Only session-a", { exact: false })).toBeVisible();
  await openTask("session-a");
  await expect(page.frameLocator("iframe").getByRole("heading", { name: "WORK-1" })).toBeVisible();
  await page.screenshot({ path: join(output, "session-a-details.png") });
  await page.getByRole("button", { name: "session-b", exact: true }).click();
  await expect(page.getByText("Only session-b", { exact: false })).toBeVisible();
  await expect(page.getByText("Only session-a", { exact: false })).toHaveCount(0);
  await expect(page.locator("iframe")).toHaveCount(0);
  await expect(page.getByText("Select a task to open its details.")).toBeVisible();
  await openTask("session-b");
  await expect(page.frameLocator("iframe").getByRole("heading", { name: "WORK-2" })).toBeVisible();
  await page.screenshot({ path: join(output, "session-b-details.png") });
  await page.getByRole("button", { name: "session-a", exact: true }).click();
  await expect(page.frameLocator("iframe").getByRole("heading", { name: "WORK-1" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Back", exact: true })).toHaveCount(0);
  await page.getByRole("button", { name: "empty", exact: true }).click();
  await expect(page.getByText("Your tasks · 0", { exact: true })).toBeVisible();
  await expect(page.locator("iframe")).toHaveCount(0);
  await page.screenshot({ path: join(output, "empty-session.png") });
  await page.getByRole("button", { name: "disconnected", exact: true }).click();
  await expect(page.getByText("This Paseo session has no Multica binding.")).toBeVisible();
  await expect(page.locator("iframe")).toHaveCount(0);
  expect(errors).toEqual([]);
  console.log("PASS: Chromium same-host/workspace session tasks, detail history, empty and disconnected states");
} finally {
  if (browser) await browser.close();
  await new Promise(done => server.close(done));
}

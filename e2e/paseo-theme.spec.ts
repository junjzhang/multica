import { test, expect, chromium, _electron, type Page } from "@playwright/test";
import { build } from "esbuild";
import { createServer, type Server } from "node:http";
import { mkdtemp, readFile, writeFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

test.describe("Paseo embedded appearance", () => {
  test.skip(
    !process.env.PASEO_THEME_HOST_CHECKOUT,
    "Set PASEO_THEME_HOST_CHECKOUT to the SDK checkout",
  );
  let server: Server;
  let base: string;
  let root: string;
  const sdk = process.env.PASEO_THEME_HOST_CHECKOUT;
  test.beforeAll(async () => {
    root = await mkdtemp(join(tmpdir(), "multica-paseo-theme-"));
    const guest = await build({
      stdin: {
        contents: `import React,{useEffect} from 'react';import {createRoot} from 'react-dom/client';import {ThemeProvider} from './components/theme-provider';import {Toaster} from '@multica/ui/components/ui/sonner';import {toast} from 'sonner';function Fixture(){useEffect(()=>{toast('Theme probe',{id:'theme-probe',duration:Infinity});},[]);return <ThemeProvider><Toaster/><h1>WORK-398 · Theme fixture</h1><p>Production web ThemeProvider inside the production resident page.</p><textarea aria-label="Draft"/><div style={{minHeight:1500}}>Issue details</div></ThemeProvider>;}createRoot(document.getElementById('root')).render(<Fixture/>);`,
        resolveDir: resolve("apps/web"),
        loader: "tsx",
      },
      bundle: true,
      write: false,
      platform: "browser",
      jsx: "automatic",
      alias: {
        sonner: resolve("packages/ui/node_modules/sonner/dist/index.mjs"),
        react: resolve("apps/web/node_modules/react"),
        "react-dom": resolve("apps/web/node_modules/react-dom"),
      },
    });
    const host = await build({
      stdin: {
        contents: `import React,{useState} from 'react';import {createRoot} from 'react-dom/client';import {ResidentWebPage} from './client/web-page';import {toPluginTheme} from ${JSON.stringify(join(sdk!, "packages/app/src/plugins/theme.ts"))};
        const origin=location.origin.replace('127.0.0.1','localhost');const url=origin+(new URLSearchParams(location.search).get('entry')==='login'?'/paseo-login?k=fixture':'/guest');
        function App(){const [scheme,setScheme]=useState(new URLSearchParams(location.search).has('legacy')?undefined:'dark');const [open,setOpen]=useState(true);const [hostId,setHostId]=useState('first');const colorScheme=scheme?toPluginTheme({colorScheme:scheme,colors:{surface0:'#ffffff'}}).colorScheme:undefined;return <><nav><button onClick={()=>setScheme(s=>s==='dark'?'light':'dark')}>Toggle theme</button><button onClick={()=>setOpen(v=>!v)}>Toggle panel</button><button onClick={()=>setHostId(v=>v==='first'?'second':'first')}>Switch host</button><span>Host: {scheme??'dark (legacy bridge)'}</span></nav>{open&&<ResidentWebPage hostId={hostId} url={url} colorScheme={colorScheme} onError={()=>{throw Error('Guest failed')}}/>}</>};createRoot(document.getElementById('root')).render(<App/>);`,
        resolveDir: resolve("integrations/paseo-multica"),
        loader: "tsx",
      },
      bundle: true,
      write: false,
      platform: "browser",
      jsx: "automatic",
      alias: {
        sonner: resolve("packages/ui/node_modules/sonner/dist/index.mjs"),
        react: resolve("apps/web/node_modules/react"),
        "react-dom": resolve("apps/web/node_modules/react-dom"),
      },
      plugins: [
        {
          name: "native-platform",
          setup(builder) {
            builder.onResolve({ filter: /^react-native$/ }, () => ({
              path: "native-platform",
              namespace: "fixture",
            }));
            builder.onLoad({ filter: /.*/, namespace: "fixture" }, () => ({
              contents: 'export const Platform={OS:"web"};',
            }));
          },
        },
      ],
    });
    const tokens = await readFile("packages/ui/styles/tokens.css", "utf8");
    server = createServer((request, response) => {
      response.setHeader("Content-Type", "text/html");
      if (request.url?.startsWith("/paseo-login")) {
        response.writeHead(302, { Location: "/guest" });
        response.end();
        return;
      }
      if (request.url === "/frame.html") {
        response.end("<p>Nested content</p>");
        return;
      }
      const isGuest = request.url?.startsWith("/guest");
      const script = (isGuest ? guest : host).outputFiles[0].text.replace(
        /<\/script/gi,
        "<\\/script",
      );
      response.end(
        `<html><head><style>${isGuest ? tokens : ""}body{margin:0;font:16px system-ui;background:var(--background,#202020);color:var(--foreground,white)}nav{height:48px;display:flex;gap:16px;align-items:center;padding:0 16px}#root{height:calc(100vh - ${isGuest ? 0 : 48}px);display:flex;flex-direction:column}h1,p,textarea{margin:24px}textarea{min-height:100px}</style></head><body><div id="root"></div><script>${script}</script></body></html>`,
      );
    });
    await new Promise<void>((done) => server.listen(0, "0.0.0.0", done));
    base = `http://127.0.0.1:${(server.address() as { port: number }).port}`;
  });
  test.afterAll(async () => {
    if (server) await new Promise<void>((done) => server.close(() => done()));
    if (root) await rm(root, { recursive: true, force: true });
  });

  async function exercise(
    host: Page,
    electron: boolean,
    directory: string,
    entry: "direct" | "login",
  ) {
    host.on("pageerror", (error) => console.error("fixture page error", error.message));
    host.on("console", (message) => {
      if (message.type() === "error") console.error("fixture console", message.text());
    });
    host.on("requestfailed", (request) =>
      console.error("fixture request", request.url(), request.failure()),
    );
    await host.goto(base + `?legacy=1&entry=${entry}`);
    const getGuest = async () => {
      if (!electron) {
        const element = await host
          .locator('[data-multica-host][aria-hidden="false"] iframe')
          .elementHandle();
        return (await element!.contentFrame())!;
      }
      return host;
    };
    const read = async (expression: string) =>
      electron
        ? host
            .locator('[data-multica-host][aria-hidden="false"] webview')
            .evaluate(
              (view, code) =>
                (
                  view as unknown as { executeJavaScript(script: string): Promise<unknown> }
                ).executeJavaScript(code),
              expression,
            )
        : (await getGuest()).evaluate((code) => eval(code), expression);
    const waitScheme = async (scheme: string) => {
      await expect.poll(() => read("document.documentElement.className")).toContain(scheme);
      await expect
        .poll(() => read("document.querySelector('[data-sonner-toaster]')?.dataset.sonnerTheme"))
        .toBe(scheme);
    };
    await waitScheme("light");
    expect(await read("matchMedia('(prefers-color-scheme: dark)').matches")).toBe(false);
    await host.screenshot({ path: join(directory, "before-os-light-host-dark.png") });
    await host.goto(base + `?entry=${entry}`);
    await waitScheme("dark");
    await host.screenshot({ path: join(directory, "after-os-light-host-dark.png") });
    const original = await read("window.__themeIdentity ??= crypto.randomUUID()");
    await read(
      "document.querySelector('textarea').value='unsaved draft';window.scrollTo(0,180);true",
    );
    await read(
      "new Promise(resolve=>{const frame=document.createElement('iframe');frame.src='/frame.html';frame.onload=()=>resolve(true);document.body.appendChild(frame);})",
    );
    for (const scheme of ["light", "dark", "light"]) {
      await host.getByRole("button", { name: "Toggle theme" }).click();
      await waitScheme(scheme);
      expect(await read("window.__themeIdentity")).toBe(original);
      expect(await read("document.querySelector('textarea').value")).toBe("unsaved draft");
      expect(await read("window.scrollY")).toBe(180);
      expect(await read("localStorage.getItem('theme')")).toBe("light");
    }
    await host.getByRole("button", { name: "Toggle panel" }).click();
    await host.getByRole("button", { name: "Toggle theme" }).click();
    await host.getByRole("button", { name: "Toggle panel" }).click();
    await waitScheme("dark");
    expect(await read("window.__themeIdentity")).toBe(original);
    await host.getByRole("button", { name: "Switch host" }).click();
    await waitScheme("dark");
    await host.getByRole("button", { name: "Switch host" }).click();
    await waitScheme("dark");
    expect(await read("window.__themeIdentity")).toBe(original);
    await read("history.pushState({},'', '/guest/next');setTimeout(()=>location.reload(),0);true");
    await waitScheme("dark");
    for (const scheme of ["light", "dark"]) {
      await host.getByRole("button", { name: "Toggle theme" }).click();
      await waitScheme(scheme);
    }
    return { read, waitScheme };
  }

  for (const entry of ["direct", "login"] as const) {
    test(`browser iframe ${entry} follows mapped host themes without changing standalone storage`, async ({}, info) => {
      const browser = await chromium.launch({
        executablePath: process.env.PLAYWRIGHT_BROWSER_EXECUTABLE,
      });
      try {
        const context = await browser.newContext({
          viewport: { width: 1000, height: 800 },
          colorScheme: "light",
        });
        const standalone = await context.newPage();
        await standalone.goto(base.replace("127.0.0.1", "localhost") + "/guest");
        await standalone.evaluate(() => localStorage.setItem("theme", "light"));
        const host = await context.newPage();
        const { read, waitScheme } = await exercise(host, false, info.outputDir, entry);
        await host.emulateMedia({ colorScheme: "dark" });
        await host.getByRole("button", { name: "Toggle theme" }).click();
        await waitScheme("light");
        await expect
          .poll(() => read("matchMedia('(prefers-color-scheme: dark)').matches"))
          .toBe(true);
        await host.screenshot({ path: info.outputPath("after-os-dark-host-light.png") });
        await read(
          `window.dispatchEvent(new MessageEvent("message",{source:window.parent,origin:"https://untrusted.test",data:{type:"multica-paseo-theme",colorScheme:"dark"}}));true`,
        );
        await waitScheme("light");
        await standalone.reload();
        await expect(standalone.locator("html")).toHaveClass("light");
        expect(await read("localStorage.getItem('theme')")).toBe("light");
      } finally {
        await browser.close();
      }
    });

    test(`Electron webview ${entry} follows host themes in an isolated Paseo partition`, async ({}, info) => {
      test.skip(
        !process.env.PASEO_THEME_ELECTRON,
        "Set PASEO_THEME_ELECTRON to a local Electron executable",
      );
      const main = join(root, "main.cjs");
      await writeFile(
        main,
        `const {app,BrowserWindow,nativeTheme}=require('electron');app.commandLine.appendSwitch('no-sandbox');app.setPath('userData',${JSON.stringify(join(root, "profile"))});nativeTheme.themeSource='light';app.whenReady().then(()=>{const win=new BrowserWindow({width:1000,height:800,show:false,webPreferences:{webviewTag:true,contextIsolation:true,nodeIntegration:false}});win.webContents.on('did-finish-load',()=>win.webContents.executeJavaScript('window.paseoDesktop={browser:{profilePartition:"persist:paseo-browser"}}'));win.showInactive();win.loadURL(${JSON.stringify(base)});});`,
      );
      const app = await _electron.launch({
        executablePath: process.env.PASEO_THEME_ELECTRON,
        args: [main, "--no-sandbox"],
        env: { ...process.env, ELECTRON_RUN_AS_NODE: "" },
      });
      try {
        const host = await app.firstWindow();
        await host.addInitScript(() => {
          (window as unknown as { paseoDesktop: unknown }).paseoDesktop = {
            browser: { profilePartition: "persist:paseo-browser" },
          };
        });
        const sessionPage = await app.evaluate(async ({ BrowserWindow }) => {
          const win = new BrowserWindow({
            show: false,
            webPreferences: { partition: "persist:paseo-browser" },
          });
          return win.id;
        });
        await app.evaluate(
          async ({ BrowserWindow }, { id, url }) => {
            const win = BrowserWindow.fromId(id)!;
            await win.loadURL(url);
            await win.webContents.executeJavaScript('localStorage.setItem("theme","light")');
            win.destroy();
          },
          { id: sessionPage, url: base.replace("127.0.0.1", "localhost") + "/guest" },
        );
        const { read, waitScheme } = await exercise(host, true, info.outputDir, entry);
        await expect(
          host.locator('[data-multica-host][aria-hidden="false"] webview'),
        ).toHaveAttribute("partition", "persist:paseo-browser");
        await app.evaluate(({ nativeTheme }) => {
          nativeTheme.themeSource = "dark";
        });
        const guestId = await host
          .locator('[data-multica-host][aria-hidden="false"] webview')
          .evaluate((view) =>
            (view as unknown as { getWebContentsId(): number }).getWebContentsId(),
          );
        await app.evaluate(async ({ webContents }, id) => {
          const guest = webContents.fromId(id)!;
          guest.debugger.attach("1.3");
          await guest.debugger.sendCommand("Emulation.setEmulatedMedia", {
            features: [{ name: "prefers-color-scheme", value: "dark" }],
          });
        }, guestId);
        await host.getByRole("button", { name: "Toggle theme" }).click();
        await waitScheme("light");
        await expect
          .poll(() => read("matchMedia('(prefers-color-scheme: dark)').matches"))
          .toBe(true);
        await host.screenshot({ path: info.outputPath("after-os-dark-host-light.png") });
      } finally {
        await app.close();
      }
    });
  }
});

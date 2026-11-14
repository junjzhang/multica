import { createElement, useLayoutEffect, useRef } from "react";
import { Platform } from "react-native";

/*
 * Minimal DOM shapes. The plugin keeps the DOM lib out of its tsconfig so
 * native code cannot depend on it; only this file touches the document, and
 * only when Platform.OS is "web".
 */
interface Style {
  [property: string]: string;
}
interface Element {
  style: Style;
  inert: boolean;
  setAttribute(name: string, value: string): void;
  appendChild(child: Element): void;
  remove(): void;
  addEventListener(
    name: string,
    listener: (event: { errorCode?: number; isMainFrame?: boolean; isInPlace?: boolean }) => void,
  ): void;
  contentWindow?: { postMessage(value: unknown, origin: string): void };
  executeJavaScript?(script: string): Promise<unknown>;
  getBoundingClientRect(): { left: number; top: number; width: number; height: number };
}
interface Document {
  body: Element;
  createElement(tag: string): Element;
}
interface ResizeObserverInstance {
  observe(target: Element): void;
  disconnect(): void;
}
interface Host {
  document: Document;
  location: { origin: string };
  ResizeObserver: new (callback: () => void) => ResizeObserverInstance;
  requestAnimationFrame(callback: () => void): number;
  cancelAnimationFrame(handle: number): void;
  addEventListener(name: string, listener: () => void, capture?: boolean): void;
  removeEventListener(name: string, listener: () => void, capture?: boolean): void;
  paseoDesktop?: { browser?: { profilePartition?: string } };
  __paseoMulticaPages?: Map<string, ResidentPage>;
}

interface ResidentPage {
  surface: Element;
  view: Element;
  owner: symbol | null;
  failed: boolean;
  onError: (() => void) | null;
  colorScheme?: "light" | "dark";
  ready: boolean;
  syncTheme(): void;
}

const host = globalThis as unknown as Host;
const anchorStyle = { flex: 1, width: "100%", height: "100%", minHeight: 0 };

/** The page outlives panels and routes; it belongs to the renderer. */
function pages(): Map<string, ResidentPage> {
  return (host.__paseoMulticaPages ??= new Map());
}

export function resetResidentPage(hostId: string): void {
  if (Platform.OS !== "web") return;
  pages().get(hostId)?.surface.remove();
  pages().delete(hostId);
}

/** Keeps the guest alive and paintable off screen without shrinking its viewport. */
function park(page: ResidentPage): void {
  page.surface.setAttribute("aria-hidden", "true");
  page.surface.inert = true;
  Object.assign(page.surface.style, {
    left: "0px",
    top: "0px",
    width: "1px",
    height: "1px",
    pointerEvents: "none",
  });
}

function createPage(
  hostId: string,
  url: string,
  signInUrl?: string,
  colorScheme?: "light" | "dark",
): ResidentPage {
  const target = new URL(url);
  const origin = target.origin;
  if (colorScheme) {
    target.searchParams.set("paseoTheme", colorScheme);
    target.searchParams.set("paseoParentOrigin", host.location.origin);
  }
  const embeddedUrl = target.href;
  const { document } = host;
  const partition = host.paseoDesktop?.browser?.profilePartition;
  const surface = document.createElement("div");
  surface.setAttribute("data-multica-host", hostId);
  // Same paint plane as Paseo's resident browsers; its overlay root sits above at z-index 1.
  Object.assign(surface.style, {
    position: "fixed",
    zIndex: "0",
    overflow: "hidden",
    display: "block",
  });

  // Electron only attaches webviews that use Paseo's own browser profile, which
  // also keeps the session cookie across restarts. Browsers fall back to a frame.
  const view = document.createElement(partition ? "webview" : "iframe");
  view.setAttribute("title", "Multica");
  Object.assign(view.style, {
    display: "inline-flex",
    position: "absolute",
    left: "0px",
    top: "0px",
    width: "1280px",
    height: "800px",
    border: "0",
  });
  if (partition) {
    view.setAttribute("partition", partition);
    view.setAttribute("allowpopups", "true");
  } else {
    view.setAttribute("allow", "clipboard-read; clipboard-write");
    view.setAttribute("referrerpolicy", "no-referrer");
  }
  if (signInUrl) {
    let signedIn = false;
    view.addEventListener(partition ? "did-finish-load" : "load", () => {
      if (!signedIn) {
        signedIn = true;
        view.setAttribute("src", embeddedUrl);
      }
    });
  }
  view.setAttribute("src", signInUrl ?? embeddedUrl);

  const page: ResidentPage = {
    surface,
    view,
    owner: null,
    failed: false,
    onError: null,
    colorScheme,
    ready: !partition,
    syncTheme() {
      if (!page.colorScheme || !page.ready) return;
      if (!partition) {
        view.contentWindow?.postMessage(
          { type: "multica-paseo-theme", colorScheme: page.colorScheme },
          origin,
        );
        return;
      }
      // The only executable input is a validated appearance enum. Never evaluate page content.
      const script = `if (location.origin === ${JSON.stringify(origin)}) { window.__multicaPaseoTheme = ${JSON.stringify(page.colorScheme)}; window.dispatchEvent(new Event("multica-paseo-theme")); }`;
      void view.executeJavaScript?.(script).catch(() => {
        /* A navigation can destroy the guest context; dom-ready retries. */
      });
    },
  };
  view.addEventListener(partition ? "dom-ready" : "load", () => {
    page.ready = true;
    page.syncTheme();
  });
  if (partition)
    view.addEventListener("did-start-navigation", (event) => {
      if (event.isMainFrame && !event.isInPlace) page.ready = false;
    });
  view.addEventListener(partition ? "did-fail-load" : "error", (event) => {
    // The sign-in redirect aborts the first navigation in Chromium (ERR_ABORTED).
    if (event.errorCode === -3) return;
    page.failed = true;
    page.onError?.();
  });
  park(page);
  surface.appendChild(view);
  document.body.appendChild(surface);
  return page;
}

function present(page: ResidentPage, anchor: Element): void {
  const rect = anchor.getBoundingClientRect();
  if (rect.width <= 0 || rect.height <= 0) {
    park(page);
    return;
  }
  page.surface.setAttribute("aria-hidden", "false");
  page.surface.inert = false;
  Object.assign(page.surface.style, {
    left: `${rect.left}px`,
    top: `${rect.top}px`,
    width: `${rect.width}px`,
    height: `${rect.height}px`,
    pointerEvents: "auto",
  });
  page.view.style.width = `${rect.width}px`;
  page.view.style.height = `${rect.height}px`;
}

export function ResidentWebPage({
  hostId,
  url,
  signInUrl,
  colorScheme,
  onError,
  retain = true,
}: {
  hostId: string;
  url: string;
  signInUrl?: string;
  colorScheme?: "light" | "dark";
  onError(): void;
  retain?: boolean;
}) {
  const ref = useRef<Element>(null);
  useLayoutEffect(() => {
    if (Platform.OS !== "web" || !ref.current) return;
    const anchor = ref.current;
    let page = pages().get(hostId);
    if (!page) {
      page = createPage(hostId, url, signInUrl, colorScheme);
      pages().set(hostId, page);
    }
    const current = page;
    const owner = Symbol(hostId);
    current.owner = owner;
    current.onError = onError;
    current.colorScheme = colorScheme;
    if (colorScheme) current.syncTheme();
    if (current.failed) onError();

    let frame = 0;
    const update = () => {
      if (current.owner === owner) present(current, anchor);
    };
    const schedule = () => {
      host.cancelAnimationFrame(frame);
      frame = host.requestAnimationFrame(update);
    };
    const observer = new host.ResizeObserver(update);
    observer.observe(anchor);
    host.addEventListener("resize", schedule);
    host.addEventListener("scroll", schedule, true);
    update();
    return () => {
      observer.disconnect();
      host.removeEventListener("resize", schedule);
      host.removeEventListener("scroll", schedule, true);
      host.cancelAnimationFrame(frame);
      if (current.owner === owner) {
        current.owner = null;
        current.onError = null;
        if (retain) park(current);
        else if (pages().get(hostId) === current) resetResidentPage(hostId);
      }
    };
  }, [hostId, url, signInUrl, onError, retain]);

  useLayoutEffect(() => {
    if (Platform.OS !== "web") return;
    const page = pages().get(hostId);
    if (!page || !colorScheme) return;
    page.colorScheme = colorScheme;
    page.syncTheme();
  }, [hostId, colorScheme]);

  if (Platform.OS !== "web") return null;
  return createElement("div", { ref, style: anchorStyle, "data-multica-anchor": hostId });
}

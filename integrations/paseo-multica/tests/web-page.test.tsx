import React from "react";
import { act, create } from "react-test-renderer";
import { expect, it, vi } from "vitest";
vi.mock("react-native", () => ({ Platform: { OS: "web" } }));
import { ResidentWebPage, resetResidentPage } from "../client/web-page";

it("signs in before navigating to the task and releases the task page on unmount", async () => {
  const elements: Array<{ attrs: Record<string, string>; listeners: Record<string, () => void>; remove: ReturnType<typeof vi.fn> }> = [];
  const makeElement = () => {
    const element = { attrs: {} as Record<string, string>, listeners: {} as Record<string, () => void>, style: {}, inert: false,
      setAttribute(name: string, value: string) { this.attrs[name] = value; }, appendChild: vi.fn(), remove: vi.fn(),
      addEventListener(name: string, listener: () => void) { this.listeners[name] = listener; },
      getBoundingClientRect: () => ({ left: 0, top: 0, width: 300, height: 500 }) };
    elements.push(element); return element;
  };
  const host = globalThis as any;
  const old = { document: host.document, ResizeObserver: host.ResizeObserver, addEventListener: host.addEventListener, removeEventListener: host.removeEventListener, requestAnimationFrame: host.requestAnimationFrame, cancelAnimationFrame: host.cancelAnimationFrame };
  Object.assign(host, { document: { body: makeElement(), createElement: makeElement }, ResizeObserver: class { observe() {} disconnect() {} }, addEventListener: vi.fn(), removeEventListener: vi.fn(), requestAnimationFrame: vi.fn(), cancelAnimationFrame: vi.fn() });
  try {
    let renderer: ReturnType<typeof create>;
    await act(async () => { renderer = create(<ResidentWebPage hostId="test-task" url="https://example.test/acme/issues/WORK-1" signInUrl="https://example.test/paseo-login" retain={false} onError={() => {}} />, { createNodeMock: makeElement }); });
    const view = elements.find((e) => e.attrs.src)!;
    expect(view.attrs.src).toBe("https://example.test/paseo-login");
    view.listeners.load!();
    expect(view.attrs.src).toBe("https://example.test/acme/issues/WORK-1");
    const surface = elements.find((e) => e.attrs["data-multica-host"] === "test-task")!;
    await act(async () => { renderer!.unmount(); });
    expect(surface.remove).toHaveBeenCalledOnce();
  } finally { resetResidentPage("test-task"); Object.assign(host, old); }
});

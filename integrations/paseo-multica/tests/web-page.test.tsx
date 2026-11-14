import React from "react";
import { act, create } from "react-test-renderer";
import { expect, it, vi } from "vitest";
vi.mock("react-native", () => ({ Platform: { OS: "web" } }));
import { ResidentWebPage, resetResidentPage } from "../client/web-page";

it("signs in before navigating to the task and releases the task page on unmount", async () => {
  const elements: Array<{
    attrs: Record<string, string>;
    listeners: Record<string, () => void>;
    remove: ReturnType<typeof vi.fn>;
  }> = [];
  const makeElement = () => {
    const element = {
      attrs: {} as Record<string, string>,
      listeners: {} as Record<string, () => void>,
      style: {},
      inert: false,
      setAttribute(name: string, value: string) {
        this.attrs[name] = value;
      },
      appendChild: vi.fn(),
      remove: vi.fn(),
      addEventListener(name: string, listener: () => void) {
        const previous = this.listeners[name];
        this.listeners[name] = () => {
          previous?.();
          listener();
        };
      },
      getBoundingClientRect: () => ({ left: 0, top: 0, width: 300, height: 500 }),
    };
    elements.push(element);
    return element;
  };
  const host = globalThis as any;
  const old = {
    document: host.document,
    ResizeObserver: host.ResizeObserver,
    addEventListener: host.addEventListener,
    removeEventListener: host.removeEventListener,
    requestAnimationFrame: host.requestAnimationFrame,
    cancelAnimationFrame: host.cancelAnimationFrame,
  };
  Object.assign(host, {
    document: { body: makeElement(), createElement: makeElement },
    ResizeObserver: class {
      observe() {}
      disconnect() {}
    },
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
    requestAnimationFrame: vi.fn(),
    cancelAnimationFrame: vi.fn(),
  });
  try {
    let renderer: ReturnType<typeof create>;
    await act(async () => {
      renderer = create(
        <ResidentWebPage
          hostId="test-task"
          url="https://example.test/acme/issues/WORK-1"
          signInUrl="https://example.test/paseo-login"
          retain={false}
          onError={() => {}}
        />,
        { createNodeMock: makeElement },
      );
    });
    const view = elements.find((e) => e.attrs.src)!;
    expect(view.attrs.src).toBe("https://example.test/paseo-login");
    view.listeners.load!();
    expect(view.attrs.src).toBe("https://example.test/acme/issues/WORK-1");
    const surface = elements.find((e) => e.attrs["data-multica-host"] === "test-task")!;
    await act(async () => {
      renderer!.unmount();
    });
    expect(surface.remove).toHaveBeenCalledOnce();
  } finally {
    resetResidentPage("test-task");
    Object.assign(host, old);
  }
});

it("updates a resident iframe in place and applies the latest theme after parking", async () => {
  const elements: any[] = [];
  const makeElement = () => {
    const element = {
      attrs: {} as Record<string, string>,
      listeners: {} as Record<string, () => void>,
      style: {},
      inert: false,
      contentWindow: { postMessage: vi.fn() },
      setAttribute(name: string, value: string) {
        this.attrs[name] = value;
      },
      appendChild: vi.fn(),
      remove: vi.fn(),
      addEventListener(name: string, listener: () => void) {
        const previous = this.listeners[name];
        this.listeners[name] = () => {
          previous?.();
          listener();
        };
      },
      getBoundingClientRect: () => ({ left: 0, top: 0, width: 300, height: 500 }),
    };
    elements.push(element);
    return element;
  };
  const host = globalThis as any;
  const old = Object.fromEntries(
    [
      "document",
      "location",
      "ResizeObserver",
      "addEventListener",
      "removeEventListener",
      "requestAnimationFrame",
      "cancelAnimationFrame",
    ].map((key) => [key, host[key]]),
  );
  Object.assign(host, {
    document: { body: makeElement(), createElement: makeElement },
    location: { origin: "https://paseo.test" },
    ResizeObserver: class {
      observe() {}
      disconnect() {}
    },
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
    requestAnimationFrame: vi.fn(),
    cancelAnimationFrame: vi.fn(),
  });
  const onError = vi.fn();
  const component = (colorScheme: "light" | "dark") => (
    <ResidentWebPage
      hostId="theme-test"
      url="https://multica.test/work/issues/WORK-1"
      colorScheme={colorScheme}
      onError={onError}
    />
  );
  let renderer: ReturnType<typeof create>;
  try {
    await act(async () => {
      renderer = create(component("dark"), { createNodeMock: makeElement });
    });
    const view = elements.find((e) => e.attrs.src)!;
    const initialUrl = view.attrs.src;
    expect(new URL(initialUrl).searchParams.get("paseoTheme")).toBe("dark");
    expect(new URL(initialUrl).searchParams.get("paseoParentOrigin")).toBe("https://paseo.test");
    await act(async () => {
      renderer!.update(component("light"));
    });
    expect(view.attrs.src).toBe(initialUrl);
    expect(view.contentWindow.postMessage).toHaveBeenLastCalledWith(
      { type: "multica-paseo-theme", colorScheme: "light" },
      "https://multica.test",
    );
    await act(async () => {
      renderer!.unmount();
    });
    expect(view.remove).not.toHaveBeenCalled();
    await act(async () => {
      renderer = create(component("dark"), { createNodeMock: makeElement });
    });
    expect(elements.filter((e) => e.attrs.src)).toHaveLength(1);
    expect(view.contentWindow.postMessage).toHaveBeenLastCalledWith(
      { type: "multica-paseo-theme", colorScheme: "dark" },
      "https://multica.test",
    );
    await act(async () => {
      renderer!.unmount();
    });
    // An older plugin's renderer cache has no theme transport callback.
    delete host.__paseoMulticaPages.get("theme-test").syncTheme;
    await act(async () => {
      renderer = create(
        <ResidentWebPage
          hostId="theme-test"
          url="https://multica.test/work/issues/WORK-1"
          onError={onError}
        />,
        { createNodeMock: makeElement },
      );
    });
    expect(elements.filter((e) => e.attrs.src)).toHaveLength(1);
    expect(onError).not.toHaveBeenCalled();
  } finally {
    await act(async () => renderer?.unmount());
    resetResidentPage("theme-test");
    Object.assign(host, old);
  }
});

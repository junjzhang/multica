"use client";

import { useEffect, useState } from "react";
import { ThemeProvider as SharedThemeProvider } from "@multica/ui/components/common/theme-provider";

type ColorScheme = "light" | "dark";

declare global {
  interface Window {
    __multicaPaseoTheme?: ColorScheme;
  }
}

function initialTheme(): ColorScheme | undefined {
  if (typeof window === "undefined") return undefined;
  const value =
    window.__multicaPaseoTheme ?? new URLSearchParams(window.location.search).get("paseoTheme");
  return value === "light" || value === "dark" ? value : undefined;
}

export function ThemeProvider(props: React.ComponentProps<typeof SharedThemeProvider>) {
  const [hostTheme, setHostTheme] = useState(initialTheme);
  useEffect(() => {
    const parentOrigin = new URLSearchParams(window.location.search).get("paseoParentOrigin");
    const update = () => setHostTheme(initialTheme());
    const receive = (event: MessageEvent) => {
      if (
        window.parent === window ||
        event.source !== window.parent ||
        event.origin !== parentOrigin ||
        event.data?.type !== "multica-paseo-theme"
      )
        return;
      const scheme = event.data.colorScheme;
      if (scheme === "light" || scheme === "dark") setHostTheme(scheme);
    };
    update();
    window.addEventListener("multica-paseo-theme", update);
    window.addEventListener("message", receive);
    return () => {
      window.removeEventListener("multica-paseo-theme", update);
      window.removeEventListener("message", receive);
    };
  }, []);
  return (
    <SharedThemeProvider {...props} forcedTheme={hostTheme ?? props.forcedTheme}>
      <script
        suppressHydrationWarning
        dangerouslySetInnerHTML={{
          __html: `(function(){var t=new URLSearchParams(location.search).get("paseoTheme");if(t==="light"||t==="dark"){window.__multicaPaseoTheme=t;document.documentElement.classList.remove("light","dark");document.documentElement.classList.add(t);document.documentElement.style.colorScheme=t;}})();`,
        }}
      />
      {props.children}
    </SharedThemeProvider>
  );
}

// Suppress React 19 false-positive about next-themes' inline <script>.
// The script works correctly; React 19 just warns about any <script> in components.
// See: https://github.com/pacocoursey/next-themes/issues/337
if (typeof window !== "undefined" && process.env.NODE_ENV === "development") {
  const orig = console.error;
  console.error = (...args: unknown[]) => {
    if (typeof args[0] === "string" && args[0].includes("Encountered a script tag")) return;
    orig.apply(console, args);
  };
}

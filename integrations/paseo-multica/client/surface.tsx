import type { PluginSurfaceProps } from "@getpaseo/plugin/client";
import { openExternalUrl, useRpc } from "@getpaseo/plugin/client";
import { useQuery } from "@tanstack/react-query";
import { useCallback, useMemo, useState } from "react";
import { Platform, Pressable, Text, View } from "react-native";
import { openWeb } from "../shared/web";
import { resetResidentPage, ResidentWebPage } from "./web-page";

export function MulticaSurface({ host, theme, layout, path, pageKey }: PluginSurfaceProps & { path?: string; pageKey?: string }) {
  const pageId = path ? `${host.id}:${pageKey}:${path}` : host.id;
  const getLink = useRpc(openWeb);
  const [loadError, setLoadError] = useState("");
  const [revision, setRevision] = useState(0);
  const link = useQuery({
    queryKey: ["multica", "web", host.id, path],
    queryFn: () => getLink(path ? { path } : {}),
    retry: false,
    gcTime: Infinity,
    staleTime: Infinity,
    refetchOnWindowFocus: false,
  });
  const refetch = link.refetch;
  const error = link.error?.message || loadError;
  const styles = useMemo(
    () => ({
      container: { flex: 1, gap: 16, padding: layout.compact ? 16 : 24, backgroundColor: theme.colors.surface0 },
      text: { color: theme.colors.foreground },
      muted: { color: theme.colors.foregroundMuted },
      action: { color: theme.colors.accent },
    }),
    [theme, layout.compact],
  );

  const failed = useCallback(() => setLoadError("Multica could not be loaded. Check the server, then retry."), []);
  const retry = useCallback(() => {
    resetResidentPage(pageId);
    setLoadError("");
    setRevision((value) => value + 1);
    void refetch();
  }, [pageId, refetch]);
  const openInBrowser = useCallback(() => {
    if (link.data)
      void openExternalUrl(link.data.externalUrl).catch(() => setLoadError("The browser could not be opened."));
  }, [link.data]);

  if (link.data && !error && Platform.OS === "web") {
    return <ResidentWebPage key={revision} hostId={pageId} url={link.data.url} signInUrl={link.data.signInUrl} onError={failed} retain={!path} />;
  }
  return (
    <View style={styles.container}>
      <Text style={styles.text}>{error || (link.data ? "Open Multica in your browser." : "Opening Multica…")}</Text>
      {error ? (
        <Pressable accessibilityRole="button" onPress={retry}>
          <Text style={styles.action}>Retry</Text>
        </Pressable>
      ) : null}
      {link.data ? (
        <Pressable accessibilityRole="button" onPress={openInBrowser}>
          <Text style={styles.action}>Open in browser</Text>
        </Pressable>
      ) : null}
    </View>
  );
}

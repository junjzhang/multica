import React from "react";
export const Platform = { OS: "web" };
export const Linking = { openURL: async (url: string) => { document.body.dataset.openedExternalUrl = url; } };
export function View({ children, style }: any) { return <div style={{ display: "flex", flexDirection: "column", ...style }}>{children}</div>; }
export const ScrollView = View;
export function Text({ children, style }: any) { return <span style={style}>{children}</span>; }
export function Pressable({ children, onPress, accessibilityRole, accessibilityLabel, accessibilityState, disabled }: any) { return <button role={accessibilityRole} aria-label={accessibilityLabel} aria-expanded={accessibilityState?.expanded} disabled={disabled} onClick={onPress}>{children}</button>; }
export function ActivityIndicator() { return <span role="progressbar" />; }

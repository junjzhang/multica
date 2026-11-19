import React from "react";
export const Platform = { OS: "web" };
export const Linking = { openURL: async (url: string) => { document.body.dataset.openedExternalUrl = url; } };
// React Native layout defaults and the shorthand props a DOM element does not understand.
const BOX = { display: "flex", flexDirection: "column", position: "relative", boxSizing: "border-box", minWidth: 0, flexShrink: 0 };
function css({ paddingHorizontal: px, paddingVertical: py, marginHorizontal: mx, transform, borderWidth, ...rest }: any = {}) {
  return {
    ...(px === undefined ? {} : { paddingLeft: px, paddingRight: px }),
    ...(py === undefined ? {} : { paddingTop: py, paddingBottom: py }),
    ...(mx === undefined ? {} : { marginLeft: mx, marginRight: mx }),
    ...(borderWidth === undefined ? {} : { borderWidth, borderStyle: "solid" }),
    ...(transform ? { transform: transform.map((step: object) => Object.entries(step).map(([key, value]) => `${key}(${value})`).join(" ")).join(" ") } : {}),
    ...rest,
  };
}
export function View({ children, style }: any) { return <div style={{ ...BOX, ...css(style) } as any}>{children}</div>; }
export const ScrollView = View;
export function Text({ children, style, numberOfLines }: any) {
  return <span style={{ ...(numberOfLines === 1 ? { whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis", minWidth: 0 } : {}), ...css(style) }}>{children}</span>;
}
export function Pressable({ children, style, onPress, onHoverIn, onHoverOut, onPressIn, onPressOut, accessibilityRole, accessibilityLabel, accessibilityState, disabled }: any) {
  return <button
    role={accessibilityRole} aria-label={accessibilityLabel} aria-expanded={accessibilityState?.expanded} disabled={disabled} onClick={onPress}
    onMouseEnter={onHoverIn} onMouseLeave={onHoverOut} onMouseDown={onPressIn} onMouseUp={onPressOut}
    style={{ all: "unset", ...BOX, cursor: disabled ? "default" : "pointer", ...css(style) } as any}
  >{children}</button>;
}
export function ActivityIndicator({ color }: any) { return <span role="progressbar" style={{ width: 8, height: 8, borderRadius: 4, border: `1.5px solid ${color}`, borderTopColor: "transparent" }} />; }

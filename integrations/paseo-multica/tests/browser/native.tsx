import React, { useEffect, useImperativeHandle, useRef, useState } from "react";
import { createPortal } from "react-dom";
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
export function View({ children, style, accessibilityRole, accessibilityLabel }: any) { return <div role={accessibilityRole} aria-label={accessibilityLabel} style={{ ...BOX, ...css(style) } as any}>{children}</div>; }
export const ScrollView = View;
export function Text({ children, style, numberOfLines }: any) {
  return <span style={{ ...(numberOfLines === 1 ? { whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis", minWidth: 0 } : {}), ...css(style) }}>{children}</span>;
}
export function Pressable({ ref, children, style, onPress, onHoverIn, onHoverOut, onPressIn, onPressOut, onFocus, onBlur, accessibilityRole, accessibilityLabel, accessibilityState, disabled }: any) {
  const node = useRef<HTMLButtonElement>(null);
  useImperativeHandle(ref, () => ({
    measureInWindow(done: (x: number, y: number, width: number, height: number) => void) {
      const box = node.current!.getBoundingClientRect();
      done(box.x, box.y, box.width, box.height);
    },
  }));
  return <button
    ref={node} role={accessibilityRole} aria-label={accessibilityLabel} aria-expanded={accessibilityState?.expanded} aria-busy={accessibilityState?.busy} disabled={disabled} onClick={onPress}
    onMouseEnter={onHoverIn} onMouseLeave={onHoverOut} onMouseDown={onPressIn} onMouseUp={onPressOut} onFocus={onFocus} onBlur={onBlur}
    style={{ all: "unset", ...BOX, cursor: disabled ? "default" : "pointer", ...css(style) } as any}
  >{children}</button>;
}
/** Like react-native-web: a full-window layer in document.body; Escape requests closing. */
export function Modal({ visible, onRequestClose, children }: any) {
  useEffect(() => {
    if (!visible) return;
    const onKey = (event: KeyboardEvent) => { if (event.key === "Escape") onRequestClose?.(); };
    document.addEventListener("keyup", onKey);
    return () => document.removeEventListener("keyup", onKey);
  }, [visible, onRequestClose]);
  return visible ? createPortal(<div style={{ position: "fixed", inset: 0, zIndex: 9999 }}>{children}</div>, document.body) : null;
}
export function useWindowDimensions() {
  const [size, setSize] = useState({ width: window.innerWidth, height: window.innerHeight });
  useEffect(() => {
    const onResize = () => setSize({ width: window.innerWidth, height: window.innerHeight });
    window.addEventListener("resize", onResize);
    return () => window.removeEventListener("resize", onResize);
  }, []);
  return size;
}
export function ActivityIndicator({ color }: any) { return <span role="progressbar" style={{ width: 8, height: 8, borderRadius: 4, border: `1.5px solid ${color}`, borderTopColor: "transparent" }} />; }

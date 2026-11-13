import { defineRpc } from "@getpaseo/plugin";
import { z } from "zod";

export const WEB_SCREEN_ID = "web";
export const WEB_PANEL_ID = "web";

export const openWeb = defineRpc({
  name: "web.open",
  input: z.object({ path: z.string().optional() }),
  output: z.object({ url: z.url(), externalUrl: z.url(), signInUrl: z.url().optional() }),
});

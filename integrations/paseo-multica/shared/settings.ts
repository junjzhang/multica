import { defineSettings } from "@getpaseo/plugin";
import { z } from "zod";

/**
 * Host-side operator configuration. It holds references only: the owner
 * profile's credential stays on disk and is read by the plugin backend.
 */
export const connectionSettings = defineSettings({
  id: "connection",
  scope: "host",
  version: 1,
  schema: z.object({
    ownerProfile: z.string().trim().max(100).default(""),
    daemonId: z.string().trim().max(200).default(""),
  }),
});

export type ConnectionSettings = z.infer<typeof connectionSettings.schema>;

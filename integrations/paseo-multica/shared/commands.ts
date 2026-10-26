import { connectSession, disconnectSession, statusSession, type StatusView } from "./contracts";

export type MulticaSubcommand = "connect" | "status" | "disconnect";
export type SessionContract = typeof connectSession | typeof statusSession | typeof disconnectSession;

export const STATUS_PANEL_ID = "status";

const SUBCOMMANDS: readonly MulticaSubcommand[] = ["connect", "status", "disconnect"];

/** `/multica connect`, `/multica status`, `/multica disconnect`; nothing else. */
export function parseSubcommand(raw: string): MulticaSubcommand {
  const words = raw.trim().split(/\s+/).filter(Boolean);
  if (words.length === 0) {
    throw new Error("Usage: /multica connect | status | disconnect");
  }
  const [word, ...rest] = words;
  if (rest.length > 0) {
    throw new Error(`Usage: /multica ${word}. This command takes no further arguments.`);
  }
  if (!SUBCOMMANDS.includes(word as MulticaSubcommand)) {
    throw new Error(`Unknown /multica command "${word}". Use connect, status, or disconnect.`);
  }
  return word as MulticaSubcommand;
}

export interface CommandHost {
  rpc(contract: SessionContract, input: { agentId: string }): Promise<StatusView>;
  openPanel(id: string, options?: { location?: "workspace" | "explorer" }): void;
}

const CONTRACTS: Record<MulticaSubcommand, SessionContract> = {
  connect: connectSession,
  status: statusSession,
  disconnect: disconnectSession,
};

/**
 * Runs one composer command for the exact agent the composer belongs to. The
 * agent id comes from the composer context, never from model-supplied text.
 * The host toasts a thrown error; success opens the concise status panel.
 */
export async function runMulticaCommand(
  host: CommandHost,
  agentId: string,
  rawArgs: string,
): Promise<{ subcommand: MulticaSubcommand; view: StatusView }> {
  const subcommand = parseSubcommand(rawArgs);
  const view = await host.rpc(CONTRACTS[subcommand], { agentId });
  host.openPanel(STATUS_PANEL_ID, { location: "explorer" });
  return { subcommand, view };
}

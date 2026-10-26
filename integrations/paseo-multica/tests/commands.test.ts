import { describe, expect, it, vi } from "vitest";
import { STATUS_PANEL_ID, parseSubcommand, runMulticaCommand, type CommandHost } from "../shared/commands";
import type { SessionContract } from "../shared/commands";
import type { StatusView } from "../shared/contracts";
import { SESSION_A } from "./fixtures";

const VIEW: StatusView = {
  state: "connected",
  agentId: "0199a0b1-2222-7222-8222-222222222222",
  checkedAt: "2026-10-26T12:00:00.000Z",
};

describe("composer commands", () => {
  it("parses only the three supported subcommands", () => {
    expect(parseSubcommand("connect")).toBe("connect");
    expect(parseSubcommand("  status  ")).toBe("status");
    expect(parseSubcommand("disconnect")).toBe("disconnect");
    expect(() => parseSubcommand("")).toThrow(/Usage: \/multica connect/);
    expect(() => parseSubcommand("connect now")).toThrow(/takes no further arguments/);
    expect(() => parseSubcommand("rebind")).toThrow(/Unknown \/multica command/);
  });

  it("uses the composer agent id and opens the status panel", async () => {
    const calls: { contract: SessionContract; agentId: string }[] = [];
    const openPanel = vi.fn();
    const host: CommandHost = {
      async rpc(contract, input) {
        calls.push({ contract, agentId: input.agentId });
        return VIEW;
      },
      openPanel,
    };

    const result = await runMulticaCommand(host, SESSION_A, "connect");
    expect(result.subcommand).toBe("connect");
    expect(result.view).toEqual(VIEW);
    expect(calls).toHaveLength(1);
    expect(calls[0]?.agentId).toBe(SESSION_A);
    expect(calls[0]?.contract.name).toBe("session.connect");
    expect(openPanel).toHaveBeenCalledWith(STATUS_PANEL_ID, { location: "explorer" });
  });

  it("does not open the panel when the command fails", async () => {
    const openPanel = vi.fn();
    const host: CommandHost = {
      async rpc() {
        throw new Error("not connected");
      },
      openPanel,
    };
    await expect(runMulticaCommand(host, SESSION_A, "status")).rejects.toThrow(/not connected/);
    expect(openPanel).not.toHaveBeenCalled();
  });
});

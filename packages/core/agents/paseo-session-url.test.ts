import { describe, expect, it } from "vitest";
import {
  buildPaseoSessionUrl,
  isValidPaseoSessionUrl,
  parsePaseoSessionUrl,
} from "./paseo-session-url";

describe("Paseo session URL", () => {
  it("builds exactly paseo://h/<serverId>/agent/<agentId>", () => {
    expect(buildPaseoSessionUrl("server-1", "agent-1")).toBe(
      "paseo://h/server-1/agent/agent-1",
    );
  });

  it("round-trips canonical encoded identifiers", () => {
    const url = buildPaseoSessionUrl("srv one", "ag/ent");
    expect(url).toBe("paseo://h/srv%20one/agent/ag%2Fent");
    expect(parsePaseoSessionUrl(url)).toEqual({ serverId: "srv one", agentId: "ag/ent" });
  });

  it("rejects credentials, query, fragment, extra paths and wrong hosts", () => {
    for (const url of [
      "paseo://user:pass@h/server-1/agent/agent-1",
      "paseo://h/server-1/agent/agent-1?x=1",
      "paseo://h/server-1/agent/agent-1#frag",
      "paseo://h/server-1/agent/agent-1/extra",
      "paseo://evil/server-1/agent/agent-1",
      "paseo://h/server-1/agent/",
      "paseo://h//agent/agent-1",
      "paseo://h/../agent/agent-1",
      "paseo://h/%2E%2E/agent/agent-1",
      "PASEO://h/server-1/agent/agent-1",
      "paseo://h/server-1/agent/agent-1\n",
    ]) {
      expect(parsePaseoSessionUrl(url)).toBeNull();
      expect(isValidPaseoSessionUrl(url)).toBe(false);
    }
  });

  it("rejects non-canonical escapes and non-Paseo schemes", () => {
    expect(parsePaseoSessionUrl("paseo://h/server%2d1/agent/agent-1")).toBeNull();
    expect(parsePaseoSessionUrl("https://h/server-1/agent/agent-1")).toBeNull();
    expect(parsePaseoSessionUrl("javascript:alert(1)")).toBeNull();
    expect(parsePaseoSessionUrl(null)).toBeNull();
  });

  it("rejects empty and oversized input", () => {
    expect(parsePaseoSessionUrl("")).toBeNull();
    expect(parsePaseoSessionUrl("paseo://h/" + "a".repeat(2048) + "/agent/agent-1")).toBeNull();
  });
});

import { describe, expect, it } from "vitest";
import { resolveHostSession } from "../server/host";
import { HOST_ID, SESSION_A, fakeHost } from "./fixtures";

describe("host session resolution", () => {
  it("uses the actual host id and the verified composer agent", async () => {
    const session = await resolveHostSession(fakeHost(), SESSION_A);
    expect(session).toEqual({
      hostId: HOST_ID,
      hostLabel: "Workstation",
      sessionId: SESSION_A,
      title: "Fix the build",
    });
  });

  it("rejects an invalid host id, a missing session, and an archived session", async () => {
    await expect(resolveHostSession(fakeHost({ serverId: "bad id" }), SESSION_A)).rejects.toThrow(/valid server id/);
    await expect(resolveHostSession(fakeHost({ exists: false }), SESSION_A)).rejects.toThrow(/no longer exists/);
    await expect(
      resolveHostSession(fakeHost({ archivedAt: "2026-10-26T00:00:00Z" }), SESSION_A),
    ).rejects.toThrow(/archived/);
  });

  it("rejects a composer id that is not a canonical locator", async () => {
    await expect(resolveHostSession(fakeHost(), "../escape")).rejects.toThrow(/invalid/);
  });
});

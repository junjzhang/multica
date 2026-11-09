import { mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { loadOwnerProfile, ownerProfilePath } from "../server/owner-profile";
import { APP_URL, OWNER_TOKEN, SERVER_URL, WORKSPACE_ID, makeHome } from "./fixtures";

describe("owner profile", () => {
  it("reads the default profile without exposing its credential", async () => {
    const home = await makeHome();
    const profile = await loadOwnerProfile(home, "");
    expect(profile).toMatchObject({
      serverUrl: SERVER_URL,
      appUrl: APP_URL,
      workspaceId: WORKSPACE_ID,
      token: OWNER_TOKEN,
    });
  });

  it("reads a named profile and falls back to the API base for the app URL", async () => {
    const home = await makeHome();
    const directory = join(home, ".multica", "profiles", "work");
    await mkdir(directory, { recursive: true });
    await writeFile(
      join(directory, "config.json"),
      JSON.stringify({ token: OWNER_TOKEN, server_url: "http://127.0.0.1:8080/", workspace_id: WORKSPACE_ID }),
    );

    const profile = await loadOwnerProfile(home, "work");
    expect(profile.serverUrl).toBe("http://127.0.0.1:8080");
    expect(profile.appUrl).toBe("http://127.0.0.1:8080");
  });

  it("rejects missing, unreadable, and incomplete profiles with actionable messages", async () => {
    const home = await makeHome();
    await expect(loadOwnerProfile(home, "missing")).rejects.toThrow(/was not found/);

    const directory = join(home, ".multica", "profiles", "broken");
    await mkdir(directory, { recursive: true });
    await writeFile(join(directory, "config.json"), "{not-json");
    await expect(loadOwnerProfile(home, "broken")).rejects.toThrow(/not valid JSON/);

    await writeFile(join(directory, "config.json"), JSON.stringify({ server_url: SERVER_URL, workspace_id: WORKSPACE_ID }));
    await expect(loadOwnerProfile(home, "broken")).rejects.toThrow(/has no credential/);
  });

  it("never includes a stored credential in an error", async () => {
    const home = await makeHome();
    const directory = join(home, ".multica", "profiles", "leaky");
    await mkdir(directory, { recursive: true });
    await writeFile(
      join(directory, "config.json"),
      JSON.stringify({ token: OWNER_TOKEN, server_url: "ftp://bad", workspace_id: WORKSPACE_ID }),
    );
    let message = "";
    try {
      await loadOwnerProfile(home, "leaky");
    } catch (error) {
      message = String(error);
    }
    expect(message).toMatch(/must use http or https/);
    expect(message).not.toContain(OWNER_TOKEN);
  });

  it("rejects profile names that are paths", () => {
    expect(() => ownerProfilePath("/home/test", "../escape")).toThrow(/invalid/);
    expect(() => ownerProfilePath("/home/test", "a/b")).toThrow(/invalid/);
  });
});

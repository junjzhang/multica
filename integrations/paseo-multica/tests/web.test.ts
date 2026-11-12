import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import { webSignInLink } from "../server/web";

const KEY = "test-sign-in-key_0123456789abcdefghijklmn";
const directories: string[] = [];

afterEach(async () => {
  await Promise.all(directories.splice(0).map((directory) => rm(directory, { recursive: true, force: true })));
});

async function configFile(content?: unknown): Promise<string> {
  const directory = await mkdtemp(join(tmpdir(), "paseo-multica-web-"));
  directories.push(directory);
  const path = join(directory, "paseo-web.json");
  if (content !== undefined) {
    await writeFile(path, typeof content === "string" ? content : JSON.stringify(content), { mode: 0o600 });
  }
  return path;
}

describe("web sign-in link", () => {
  it("points the configured origin at the proxy sign-in path with the key", async () => {
    const path = await configFile({ url: "https://multica.example", key: KEY });
    expect(await webSignInLink(path)).toEqual({
      url: `https://multica.example/paseo-login?k=${KEY}`,
      externalUrl: "https://multica.example/",
    });
  });

  it("sends an external browser to its own origin without the key", async () => {
    const path = await configFile({ url: "https://multica.example", key: KEY, externalUrl: "http://10.0.0.5:3210" });
    const link = await webSignInLink(path);
    expect(link.externalUrl).toBe("http://10.0.0.5:3210/");
    expect(link.externalUrl).not.toContain(KEY);
  });

  it("accepts an origin with a port", async () => {
    const path = await configFile({ url: "http://10.0.0.5:3210", key: KEY });
    expect((await webSignInLink(path)).url).toBe(`http://10.0.0.5:3210/paseo-login?k=${KEY}`);
  });

  it("names the missing file so the operator can create it", async () => {
    const path = await configFile();
    await expect(webSignInLink(path)).rejects.toThrow(path);
  });

  it.each([
    "file:///tmp/page",
    "https://user:password@multica.example",
    "https://multica.example/workspace",
    "https://multica.example/?k=other",
    "https://multica.example/#fragment",
  ])("rejects a url or externalUrl that is not a bare HTTP(S) origin: %s", async (url) => {
    await expect(webSignInLink(await configFile({ url, key: KEY }))).rejects.toThrow(/url must be/);
    const external = await configFile({ url: "https://multica.example", key: KEY, externalUrl: url });
    await expect(webSignInLink(external)).rejects.toThrow(/externalUrl must be/);
  });

  it.each(["short", "has space and &redirect=elsewhere", "x".repeat(31)])(
    "rejects a key that is short or not URL-safe: %s",
    async (key) => {
      const path = await configFile({ url: "https://multica.example", key });
      await expect(webSignInLink(path)).rejects.toThrow(/sign-in key/);
    },
  );

  it("rejects malformed JSON and unknown fields without echoing the key", async () => {
    for (const content of ["{broken", { url: "https://multica.example", key: KEY, extra: true }]) {
      const path = await configFile(content);
      const failure = await webSignInLink(path).then(
        () => null,
        (error: unknown) => error as Error,
      );
      expect(failure).toBeInstanceOf(Error);
      expect(failure?.message).toMatch(/a url, a key, and an optional externalUrl/);
      expect(failure?.message).not.toContain(KEY);
    }
  });
});

describe("issue deep links", () => {
  it("returns a same-workspace path and keeps the sign-in link separate", async () => {
    const config = await configFile({ url: "https://multica.example", key: KEY, externalUrl: "https://external.example" });
    expect(await webSignInLink(config, "/acme/issues/WORK-1", "acme")).toEqual({
      url: "https://multica.example/acme/issues/WORK-1",
      externalUrl: "https://external.example/acme/issues/WORK-1",
      signInUrl: `https://multica.example/paseo-login?k=${KEY}`,
    });
  });
  it.each(["https://evil.test/acme/issues/WORK-1", "//evil.test/acme/issues/WORK-1", "/acme/issues/../secret", "/acme/issues/%2e%2e", "/acme/issues/WORK-1?x=1", "/other/issues/WORK-1", "/acme/issues/WORK-1#x", "/acme/issues/a\\b"])("rejects %s", async (path) => {
    const config = await configFile({ url: "https://multica.example", key: KEY });
    await expect(webSignInLink(config, path, "acme")).rejects.toThrow(/Invalid/);
  });
});

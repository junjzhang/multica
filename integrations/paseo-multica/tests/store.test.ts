import { mkdir, readdir, stat, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import {
  contextDirectory,
  contextPath,
  readSessionContext,
  removeSessionContext,
  writeSessionContext,
} from "../server/store";
import { AGENT_B, SESSION_A, makeContext, makeHome } from "./fixtures";

describe("session context store", () => {
  it("writes the v1 file as 0600 inside a 0700 directory", async () => {
    const home = await makeHome();
    const context = makeContext();
    await writeSessionContext(home, context);

    const path = contextPath(home, SESSION_A);
    expect((await stat(path)).mode & 0o777).toBe(0o600);
    expect((await stat(contextDirectory(home))).mode & 0o777).toBe(0o700);
    expect(await readSessionContext(home, SESSION_A)).toEqual({ status: "ok", context });
  });

  it("replaces an existing binding atomically and leaves no temporary file", async () => {
    const home = await makeHome();
    await writeSessionContext(home, makeContext());
    const replacement = makeContext({ agent_id: AGENT_B });
    await writeSessionContext(home, replacement);

    const read = await readSessionContext(home, SESSION_A);
    expect(read.status).toBe("ok");
    if (read.status === "ok") expect(read.context.agent_id).toBe(AGENT_B);
    expect(await readdir(contextDirectory(home))).toEqual([`${SESSION_A}.json`]);
  });

  it("reports missing, malformed, unsupported, and foreign contexts distinctly", async () => {
    const home = await makeHome();
    expect(await readSessionContext(home, SESSION_A)).toEqual({ status: "missing" });

    const path = contextPath(home, SESSION_A);
    await mkdir(contextDirectory(home), { recursive: true });
    await writeFile(path, "{not json");
    expect(await readSessionContext(home, SESSION_A)).toMatchObject({ status: "invalid", kind: "malformed" });

    await writeFile(path, JSON.stringify({ ...makeContext(), version: 2 }));
    expect(await readSessionContext(home, SESSION_A)).toMatchObject({ status: "invalid", kind: "unsupported" });

    await writeFile(path, JSON.stringify({ ...makeContext(), session_id: "someone-else" }));
    expect(await readSessionContext(home, SESSION_A)).toMatchObject({ status: "invalid", kind: "foreign-session" });
  });

  it("rejects a session id that could escape the private directory", () => {
    expect(() => contextPath("/home/test", "../escape")).toThrow(/canonical Paseo agent id/);
    expect(() => contextPath("/home/test", "a/b")).toThrow(/canonical Paseo agent id/);
  });

  it("removes only its own context", async () => {
    const home = await makeHome();
    await writeSessionContext(home, makeContext());
    expect(await removeSessionContext(home, SESSION_A)).toBe(true);
    expect(await removeSessionContext(home, SESSION_A)).toBe(false);
    expect(await readSessionContext(home, SESSION_A)).toEqual({ status: "missing" });
  });

  it("does not create the directory when the source file cannot be written", async () => {
    const home = await makeHome();
    await mkdir(join(home, ".multica"), { recursive: true });
    await writeFile(join(home, ".multica", "external-sessions"), "not a directory");
    await expect(writeSessionContext(home, makeContext())).rejects.toThrow();
  });
});

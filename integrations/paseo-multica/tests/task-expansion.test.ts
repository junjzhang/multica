// @vitest-environment node
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { expect, it } from "vitest";
import { expansion } from "../server/task-expansion";
import { contextPath } from "../server/store";

it("persists expansion per session and serializes concurrent updates", async () => {
  const home = await mkdtemp(join(tmpdir(), "multica-expansion-"));
  const first = "0199a0b1-4444-7444-8444-444444444444";
  const second = "0199a0b1-5555-7555-8555-555555555555";
  try {
    await Promise.all([expansion(home, "session-a", first, true), expansion(home, "session-a", second, true)]);
    expect(await expansion(home, "session-a")).toEqual([first, second]);
    expect(await expansion(home, "session-b")).toEqual([]);
    await expansion(home, "session-a", first, false);
    expect(await expansion(home, "session-a")).toEqual([second]);
    await writeFile(`${contextPath(home, "session-a")}.expansion`, "broken");
    expect(await expansion(home, "session-a")).toEqual([]);
    await expansion(home, "session-a", first, true);
    expect(await expansion(home, "session-a")).toEqual([first]);
    await expect(expansion(home, "../escape")).rejects.toThrow();
  } finally { await rm(home, { recursive: true, force: true }); }
});

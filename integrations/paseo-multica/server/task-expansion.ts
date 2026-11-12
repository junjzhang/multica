import { randomUUID } from "node:crypto";
import { mkdir, readFile, rename, rm, writeFile } from "node:fs/promises";
import { dirname } from "node:path";
import { z } from "zod";
import { contextPath } from "./store";

const schema = z.array(z.string().uuid()).max(10000);
const locks = new Map<string, Promise<unknown>>();

export async function expansion(home: string, agentId: string, issueId?: string, expanded?: boolean): Promise<string[]> {
  const path = `${contextPath(home, agentId)}.expansion`;
  const previous = locks.get(path) ?? Promise.resolve();
  const next = previous.catch(() => {}).then(async () => {
    let values: string[] = [];
    try { values = schema.parse(JSON.parse(await readFile(path, "utf8"))); }
    catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error; }
    if (issueId !== undefined && expanded !== undefined) {
      values = values.filter((id) => id !== issueId);
      if (expanded) values.push(issueId);
      schema.parse(values);
      await mkdir(dirname(path), { recursive: true, mode: 0o700 });
      const tmp = `${path}.${randomUUID()}.tmp`;
      try { await writeFile(tmp, JSON.stringify(values), { mode: 0o600 }); await rename(tmp, path); }
      finally { await rm(tmp, { force: true }); }
    }
    return values;
  });
  locks.set(path, next);
  try { return await next; } finally { if (locks.get(path) === next) locks.delete(path); }
}

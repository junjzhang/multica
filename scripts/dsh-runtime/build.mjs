import { spawnSync } from "node:child_process";
import { mkdirSync, readdirSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// Keep the bridge out of the application workspace and pin the source that
// contains the DSH 0.2 protocol, prompt, and lifecycle adaptations.
const revision = "571d7c036d15f58ee88c89fd80afb0a6c7f0023c";
const repository = "https://github.com/multica-ai/dsh-multica-runtime.git";
const directory = process.argv[2];
if (!directory || process.argv.length !== 3) {
  throw new Error("usage: node scripts/dsh-runtime/build.mjs <empty-build-directory>");
}
const destination = resolve(directory);
mkdirSync(destination, { recursive: true });
if (readdirSync(destination).length) {
  throw new Error(`build directory must be empty: ${destination}`);
}

function run(command, args, cwd = destination) {
  const result = spawnSync(command, args, { cwd, stdio: "inherit" });
  if (result.error) throw result.error;
  if (result.status !== 0) {
    throw new Error(`${command} failed (${result.signal ?? result.status})`);
  }
}

run("git", ["init", "--quiet"]);
run("git", ["remote", "add", "origin", repository]);
run("git", ["fetch", "--depth=1", "origin", revision]);
run("git", ["checkout", "--quiet", "--detach", "FETCH_HEAD"]);
run("git", ["apply", "--unidiff-zero", resolve(dirname(fileURLToPath(import.meta.url)), "rc5-build.patch")]);
run("pnpm", ["install", "--ignore-workspace", "--frozen-lockfile"]);
run("pnpm", ["check"]);
run("pnpm", ["pack", "--out", resolve(destination, "..", "dsh-runtime-rc5.tgz")]);

console.log(`Built DSH bridge ${revision} with the 0.2.1-rc.5 package family.`);
console.log("Install the tarball with dsh plugin only after active DSH runs finish.");

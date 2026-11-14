#!/usr/bin/env node
// Path filtering and aggregate gates share this fail-closed decision contract.
import { appendFileSync, readFileSync } from "node:fs";
import { execFileSync } from "node:child_process";
import { matchesGlob } from "node:path";
import { pathToFileURL } from "node:url";

export const filters = JSON.parse(
  readFileSync(new URL("../.github/ci-paths.json", import.meta.url), "utf8"),
);

export function decideScopes(event, filtered, { full: requestedFull = false } = {}) {
  const full = event === "schedule" || (event === "workflow_dispatch" && requestedFull === true);
  if (!["schedule", "workflow_dispatch", "push", "pull_request"].includes(event)) {
    throw new Error(`Unsupported CI event: ${event}`);
  }
  const outputs = { full: String(full) };
  for (const scope of Object.keys(filters)) {
    if (!full && filtered[scope] !== "true" && filtered[scope] !== "false") {
      throw new Error(`Missing or invalid path-filter result: ${scope}`);
    }
    outputs[scope] = full ? "true" : filtered[scope];
  }
  // Product builds already install dependencies and run the shared checks.
  // Only allocate a separate runner when quality is the sole frontend work.
  outputs.quality_only = String(outputs.quality === "true" && outputs.frontend === "false");
  outputs.macos_runtime = String(full || (event === "workflow_dispatch" && outputs.runtime === "true"));
  return outputs;
}

export function planManualRun({ full = "false", ref, sha, defaultBranch, cwd = process.cwd() }) {
  const all = (reason) => ({ full: true, filtered: {}, reason });
  if (!["", "false", "true"].includes(full)) throw new Error("Invalid manual full input");
  if (full === "true") return all("Explicit full validation requested.");
  if (!defaultBranch || !ref?.startsWith("refs/heads/") || ref === `refs/heads/${defaultBranch}`) {
    return all("Default branch, tag or missing branch context requires full validation.");
  }
  const git = (...args) => execFileSync("git", args, {
    cwd, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"], maxBuffer: 16 * 1024 * 1024,
  });
  // Never attribute checks on a different checkout to the dispatched revision.
  if (!/^[a-f0-9]{40,64}$/.test(sha ?? "") || git("rev-parse", "HEAD").trim() !== sha) {
    throw new Error("Checkout does not match the dispatched revision");
  }
  let base;
  let files;
  try {
    if (git("rev-parse", "--is-shallow-repository").trim() !== "false") {
      return all("Incomplete history requires full validation.");
    }
    git("check-ref-format", `refs/heads/${defaultBranch}`);
    const target = git("rev-parse", "--verify", `refs/remotes/origin/${defaultBranch}^{commit}`).trim();
    const bases = git("merge-base", "--all", target, sha).trim().split("\n");
    if (bases.length !== 1 || !bases[0]) return all("No unique merge base; running full validation.");
    [base] = bases;
    // Treat renames as delete + add so both scopes remain covered. NUL separators
    // preserve spaces, newlines and Git's otherwise quoted non-ASCII filenames.
    files = git("diff", "--name-only", "--no-renames", "-z", base, sha, "--").split("\0").filter(Boolean);
  } catch {
    return all("Comparison unavailable; running full validation.");
  }
  if (!files.length) return all("No branch changes to compare; running full validation.");
  // paths-filter enables dot:true; Node's glob matcher does not. Avoid partial
  // matches (e.g. an image scope masking a hidden source directory) by running full.
  if (files.some((file) => file.split("/").some((part) => part.startsWith(".")))) {
    return all("Hidden paths require full validation.");
  }
  const matches = (file, patterns) => patterns.some((pattern) => matchesGlob(file, pattern));
  // Unmatched source/configuration must not silently turn a manual run green.
  const documentation = ["README.md", "CONTRIBUTING.md", "AGENTS.md", "LICENSE", "docs/**/*.md", "docs/**/*.mdx"];
  const patterns = Object.entries(filters).filter(([scope]) => scope !== "images").flatMap(([, paths]) => paths);
  if (files.some((file) => !matches(file, patterns) && !matches(file, documentation))) {
    return all("Unknown changed paths require full validation.");
  }
  const filtered = Object.fromEntries(Object.entries(filters).map(([scope, paths]) => [
    scope, String(files.some((file) => matches(file, paths))),
  ]));
  return { full: false, filtered, reason: `Incremental validation: ${base}..${sha} (${files.length} changed paths).` };
}

export function checkGate(needs, jobScopes) {
  if (needs.changes?.result !== "success") {
    throw new Error("Path filtering did not succeed");
  }
  for (const [job, scope] of Object.entries(jobScopes)) {
    const selected = needs.changes.outputs?.[scope];
    if (selected !== "true" && selected !== "false") {
      throw new Error(`Missing or invalid scope: ${scope}`);
    }
    const expected = selected === "true" ? "success" : "skipped";
    if (needs[job]?.result !== expected) {
      throw new Error(`${job}: expected ${expected}, got ${needs[job]?.result ?? "missing"}`);
    }
  }
  // A newly added dependency must have a declared scope before the gate can pass.
  for (const job of Object.keys(needs)) {
    if (job !== "changes" && !Object.hasOwn(jobScopes, job)) {
      throw new Error(`Unchecked dependency: ${job}`);
    }
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    switch (process.argv[2]) {
      case "decide": {
        const event = process.env.EVENT_NAME;
        const plan = event === "workflow_dispatch" ? planManualRun({
          full: process.env.FULL_RUN,
          ref: process.env.GITHUB_REF,
          sha: process.env.GITHUB_SHA,
          defaultBranch: process.env.DEFAULT_BRANCH,
        }) : { filtered: JSON.parse(process.env.FILTER_RESULTS), reason: `Event: ${event}.` };
        const outputs = decideScopes(event, plan.filtered, plan);
        const summary = `${plan.reason}\nSelected scopes: ${Object.entries(outputs).filter(([, value]) => value === "true").map(([key]) => key).join(", ") || "none (documentation only)"}\n`;
        console.log(summary);
        if (process.env.GITHUB_STEP_SUMMARY) appendFileSync(process.env.GITHUB_STEP_SUMMARY, summary);
        appendFileSync(process.env.GITHUB_OUTPUT,
          Object.entries(outputs).map(([key, value]) => `${key}=${value}\n`).join(""));
        break;
      }
      case "gate":
        checkGate(JSON.parse(process.env.NEEDS_JSON), JSON.parse(process.env.JOB_SCOPES));
        console.log("All selected CI jobs succeeded; only unselected jobs were skipped.");
        break;
      default:
        throw new Error("Usage: node scripts/ci-scope.mjs decide|gate");
    }
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}

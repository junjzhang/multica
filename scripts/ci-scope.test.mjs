import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, readFileSync, renameSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, matchesGlob } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { checkGate, decideScopes, filters, planManualRun } from "./ci-scope.mjs";

// The filters use only literal paths, * and **; Node's matcher exercises those
// against realistic changes, including files removed or renamed by a PR.
function filterFiles(files) {
  return Object.fromEntries(Object.entries(filters).map(([scope, patterns]) => [
    scope, String(files.some((file) => patterns.some((pattern) => matchesGlob(file, pattern)))),
  ]));
}

for (const [name, files, selected] of [
  ["readme only", ["README.md"], []],
  ["docs only", ["apps/docs/content/docs/guide.mdx"], ["quality"]],
  ["web changelog", ["apps/web/features/landing/i18n/en.ts"], ["frontend", "quality"]],
  ["UI Lab", ["apps/ui-lab/src/app.tsx"], ["frontend", "quality"]],
  ["mobile UI", ["apps/mobile/app/index.tsx"], ["quality"]],
  ["migration only", ["server/migrations/999_example.up.sql"], ["backend", "sqlc"]],
  ["agent process code", ["server/pkg/agent/cursor_background.go"], ["backend", "runtime"]],
  ["daemon dependency", ["server/internal/skill/service.go"], ["backend", "runtime"]],
  ["native test compilation dependency", ["server/pkg/db/generated/issues.sql.go"], ["backend", "sqlc", "runtime"]],
  ["Go dependencies", ["server/go.mod", "server/go.sum"], ["backend", "runtime"]],
  ["Helm only", ["deploy/helm/multica/templates/deployment.yaml"], ["scripts"]],
  ["container entrypoint", ["docker/entrypoint.sh"], ["scripts"]],
  ["selfhost config", [".env.example"], ["scripts", "installer"]],
  ["shell installer", ["scripts/install.sh"], ["scripts", "installer"]],
  ["PowerShell installer", ["scripts/install.ps1.test.ps1"], ["installer"]],
  ["cleanup script", ["scripts/drop-database.sh"], ["scripts"]],
  ["check script regression tests", ["scripts/check.test.sh"], ["scripts"]],
  ["performance harness", ["scripts/perf-compare.test.sh"], ["scripts"]],
  ["reserved slug source", ["server/internal/handler/reserved_slugs.json"], ["backend", "runtime", "scripts"]],
  ["reserved slug output", ["packages/core/paths/reserved-slugs.ts"], ["frontend", "quality", "scripts"]],
  ["integration key gates", ["server/cmd/server/router.go"], ["backend", "runtime", "scripts"]],
  ["cross-module runtime contract", ["packages/core/runtimes/cli-version.ts"], ["frontend", "backend", "runtime", "quality"]],
  ["lockfile", ["pnpm-lock.yaml"], ["frontend", "quality"]],
  ["workspace package patch", ["patches/example.patch"], ["frontend", "quality"]],
  ["mobile package patch", ["apps/mobile/patches/example.patch"], ["quality"]],
  ["radius policy", ["scripts/check-ui-radius-tokens.mjs"], ["quality"]],
  ["shared quality action", [".github/actions/frontend-quality/action.yml"], ["frontend", "quality"]],
  ["new bitmap", ["apps/web/public/hero.png"], ["frontend", "quality", "images"]],
  ["mixed docs and migration", ["apps/docs/content/guide.mdx", "server/migrations/999_example.up.sql"], ["quality", "backend", "sqlc"]],
  ["CI configuration", [".github/ci-paths.json"], Object.keys(filters)],
]) {
  test(`PR and main select only affected scopes: ${name}`, () => {
    for (const event of ["pull_request", "push"]) {
      const outputs = decideScopes(event, filterFiles(files));
      assert.equal(outputs.full, "false");
      assert.deepEqual(Object.keys(filters).filter((scope) => outputs[scope] === "true").sort(),
        [...selected].sort());
      assert.equal(outputs.quality_only, String(selected.includes("quality") && !selected.includes("frontend")));
    }
  });
}

test("scheduled and explicitly full manual runs select every scope without a path-filter result", () => {
  for (const event of ["schedule", "workflow_dispatch"]) {
    const { quality_only, ...scopes } = decideScopes(event, {}, { full: true });
    assert.ok(Object.values(scopes).every((value) => value === "true"));
    assert.equal(quality_only, "false", "the full frontend build owns quality checks");
  }
});

test("missing, malformed and unsupported filter results fail closed", () => {
  for (const value of [undefined, "", "unknown", true]) {
    assert.throws(() => decideScopes("push", { ...filterFiles([]), backend: value }), /backend/);
  }
  assert.throws(() => decideScopes("unknown", filterFiles([])), /Unsupported/);
});

// Read the production wiring without installing workspace dependencies in the
// lightweight changes job. These fields deliberately use single-line syntax;
// unsupported formatting fails the assertions instead of being silently ignored.
const workflow = readFileSync(new URL("../.github/workflows/ci.yml", import.meta.url), "utf8");
const jobSource = workflow.slice(workflow.indexOf("\njobs:\n") + "\njobs:\n".length);
const headings = [...jobSource.matchAll(/^  ([\w-]+):$/gm)];
const jobs = Object.fromEntries(headings.map((match, index) => [
  match[1], jobSource.slice(match.index, headings[index + 1]?.index),
]));
function field(source, pattern) {
  const match = source.match(pattern);
  assert.ok(match, `Missing production field: ${pattern}`);
  return match[1];
}
function productionMapping(gate) {
  return JSON.parse(field(jobs[gate], /^          JOB_SCOPES: '(.+)'$/m));
}
function productionNeeds(gate, outputs) {
  const dependencies = field(jobs[gate], /^    needs: \[(.+)\]$/m).split(", ");
  return Object.fromEntries(dependencies.map((job) => {
    if (job === "changes") return [job, { result: "success", outputs }];
    const scope = field(jobs[job], /^    if: \$\{\{ needs\.changes\.outputs\.(\w+) == 'true' \}\}$/m);
    return [job, { result: outputs[scope] === "true" ? "success" : "skipped" }];
  }));
}

for (const gate of ["frontend", "backend"]) {
  test(`production ${gate} gate matches every dependency, condition and scope output`, () => {
    const mapping = productionMapping(gate);
    assert.match(jobs[gate], /^    if: \$\{\{ !cancelled\(\) \}\}$/m);
    assert.match(jobs[gate], /^          NEEDS_JSON: \$\{\{ toJSON\(needs\) \}\}$/m);
    assert.match(jobs[gate], /^        run: node scripts\/ci-scope\.mjs gate$/m);
    for (const scope of Object.values(mapping)) {
      assert.ok(jobs.changes.includes(`      ${scope}: \${{ steps.decide.outputs.${scope} }}`));
    }
    const scopes = Object.keys(filters);
    // Exercise mixed scopes too: installers and quality may run without the
    // corresponding product backend/frontend suite being selected.
    for (let mask = 0; mask < 2 ** scopes.length; mask++) {
      const filtered = Object.fromEntries(scopes.map((scope, bit) => [scope, String(Boolean(mask & (1 << bit)))]));
      for (const event of ["pull_request", "push", "workflow_dispatch"]) {
        const outputs = decideScopes(event, filtered);
        checkGate(productionNeeds(gate, outputs), mapping);
      }
    }
    for (const event of ["schedule", "workflow_dispatch"]) {
      checkGate(productionNeeds(gate, decideScopes(event, {}, { full: true })), mapping);
    }
  });

  test(`production ${gate} gate rejects unsuccessful or missing selected jobs`, () => {
    const mapping = productionMapping(gate);
    for (const job of Object.keys(mapping)) {
      // A docs-only run selects the standalone quality runner; a full run
      // selects every other dependency, including the installer matrix.
      const outputs = job === "frontend-quality"
        ? decideScopes("pull_request", filterFiles(["apps/docs/content/guide.mdx"]))
        : decideScopes("workflow_dispatch", {}, { full: true });
      for (const result of ["failure", "cancelled", "skipped", undefined]) {
        const input = productionNeeds(gate, outputs);
        assert.equal(input[job].result, "success");
        input[job].result = result;
        assert.throws(() => checkGate(input, mapping), new RegExp(job));
      }
      const missing = productionNeeds(gate, outputs);
      delete missing[job];
      assert.throws(() => checkGate(missing, mapping), new RegExp(job));
    }
  });

  test(`production ${gate} gate rejects invalid filtering and unchecked dependencies`, () => {
    const mapping = productionMapping(gate);
    const unselected = () => productionNeeds(gate, decideScopes("pull_request", filterFiles([])));
    for (const result of ["failure", "cancelled", "skipped", undefined]) {
      const input = unselected();
      input.changes.result = result;
      assert.throws(() => checkGate(input, mapping), /Path filtering/);
    }
    for (const [job, scope] of Object.entries(mapping)) {
      const failed = unselected();
      failed[job].result = "failure";
      assert.throws(() => checkGate(failed, mapping), new RegExp(job));
      for (const value of [undefined, "", "unknown"]) {
        const invalid = unselected();
        invalid.changes.outputs[scope] = value;
        assert.throws(() => checkGate(invalid, mapping), /scope/);
      }
    }
    const extra = unselected();
    extra.extra = { result: "failure" };
    assert.throws(() => checkGate(extra, mapping), /Unchecked dependency/);
  });
}

test("the backend gate owns the three-platform installer matrix", () => {
  assert.equal(productionMapping("backend").installer, "installer");
  assert.match(jobs.installer, /^        os: \[ubuntu-latest, macos-latest, windows-latest\]$/m);
  assert.doesNotMatch(jobs.installer, /continue-on-error:/);
});

test("quality checks have exactly one runner and reuse the product build install", () => {
  const invocation = "uses: ./.github/actions/frontend-quality";
  const owners = Object.entries(jobs).filter(([, source]) => source.includes(invocation)).map(([job]) => job);
  assert.deepEqual(owners.sort(), ["frontend-build", "frontend-quality"]);
  assert.equal(productionMapping("frontend")["frontend-quality"], "quality_only");
  assert.match(jobs["frontend-build"], /      - name: Check frontend quality\n        uses: \.\/\.github\/actions\/frontend-quality\n/);
  for (const frontend of ["true", "false"]) {
    for (const quality of ["true", "false"]) {
      const outputs = decideScopes("pull_request", { ...filterFiles([]), frontend, quality });
      const results = productionNeeds("frontend", outputs);
      const runners = owners.filter((job) => results[job].result === "success");
      assert.equal(runners.length, frontend === "true" || quality === "true" ? 1 : 0);
    }
  }
  const action = readFileSync(new URL("../.github/actions/frontend-quality/action.yml", import.meta.url), "utf8");
  assert.match(action, /run: pnpm knip/);
  assert.match(action, /continue-on-error: true/);
  assert.doesNotMatch(action, /pnpm install/);
});

test("the CLI writes real Actions outputs and exits nonzero on a failed gate", (t) => {
  const dir = mkdtempSync(join(tmpdir(), "ci-scope-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const output = join(dir, "output");
  const script = fileURLToPath(new URL("./ci-scope.mjs", import.meta.url));
  const run = spawnSync(process.execPath, [script, "decide"], {
    env: { ...process.env, EVENT_NAME: "push", FILTER_RESULTS: JSON.stringify(filterFiles(["README.md"])), GITHUB_OUTPUT: output },
    encoding: "utf8",
  });
  assert.equal(run.status, 0, run.stderr);
  assert.match(readFileSync(output, "utf8"), /^backend=false$/m);
  const failed = productionNeeds("backend", decideScopes("workflow_dispatch", {}, { full: true }));
  failed["backend-tests"].result = "failure";
  const gate = spawnSync(process.execPath, [script, "gate"], {
    env: { ...process.env, NEEDS_JSON: JSON.stringify(failed), JOB_SCOPES: JSON.stringify(productionMapping("backend")) },
    encoding: "utf8",
  });
  assert.equal(gate.status, 1);
  assert.match(gate.stderr, /backend-tests/);
});

function gitFixture(t) {
  const cwd = mkdtempSync(join(tmpdir(), "ci-manual-"));
  t.after(() => rmSync(cwd, { recursive: true, force: true }));
  const git = (...args) => {
    const result = spawnSync("git", args, { cwd, encoding: "utf8" });
    assert.equal(result.status, 0, result.stderr);
    return result.stdout.trim();
  };
  const commit = (files) => {
    for (const [file, content] of Object.entries(files)) {
      mkdirSync(dirname(join(cwd, file)), { recursive: true });
      writeFileSync(join(cwd, file), content);
    }
    git("add", ".");
    git("-c", "user.name=CI Test", "-c", "user.email=ci@example.invalid", "commit", "-qm", "fixture");
    return git("rev-parse", "HEAD");
  };
  git("init", "-q", "--initial-branch=main");
  const base = commit({ "README.md": "base", "server/pkg/agent/old.go": "old" });
  git("update-ref", "refs/remotes/origin/main", base);
  git("switch", "-qc", "feature");
  const plan = (options = {}) => planManualRun({
    cwd, ref: "refs/heads/feature", sha: git("rev-parse", "HEAD"), defaultBranch: "main", ...options,
  });
  return { cwd, git, commit, plan, base };
}

test("manual default uses the merge base, not the last commit or the default branch tip", (t) => {
  const { git, commit, plan, base } = gitFixture(t);
  git("switch", "main");
  const main = commit({ "server/migrations/999.sql": "unrelated main change" });
  git("update-ref", "refs/remotes/origin/main", main);
  git("switch", "feature");
  commit({ "apps/web/a.tsx": "feature" });
  commit({ "README.md": "second feature commit" });
  const result = plan();
  assert.equal(result.full, false);
  assert.match(result.reason, new RegExp(base));
  const outputs = decideScopes("workflow_dispatch", result.filtered, result);
  assert.equal(outputs.frontend, "true");
  assert.equal(outputs.backend, "false");
  assert.equal(outputs.macos_runtime, "false");
});

test("manual runtime changes retain both native platform jobs and aggregate failure checks", (t) => {
  const { commit, plan } = gitFixture(t);
  commit({ "server/pkg/agent/cursor_background.go": "changed" });
  const result = plan();
  const outputs = decideScopes("workflow_dispatch", result.filtered, result);
  assert.equal(outputs.full, "false");
  assert.equal(outputs.frontend, "false");
  assert.equal(outputs.runtime, "true");
  assert.equal(outputs.macos_runtime, "true");
  const needs = productionNeeds("backend", outputs);
  checkGate(needs, productionMapping("backend"));
  for (const job of ["macos-runtime", "windows-execenv"]) {
    assert.equal(needs[job].result, "success");
    assert.throws(() => checkGate({ ...needs, [job]: { result: "skipped" } }, productionMapping("backend")), new RegExp(job));
  }
});

test("manual deletion and rename include the old path, including unusual filenames", (t) => {
  const { cwd, commit, plan } = gitFixture(t);
  renameSync(join(cwd, "server/pkg/agent/old.go"), join(cwd, "README.md"));
  commit({ "apps/web/space and\nnewline.tsx": "new" });
  const result = plan();
  assert.equal(result.full, false);
  assert.equal(result.filtered.runtime, "true");
  assert.equal(result.filtered.frontend, "true");
});

test("manual known documentation-only changes can skip product suites", (t) => {
  const { commit, plan } = gitFixture(t);
  commit({ "README.md": "docs", "docs/engineering/note.md": "docs" });
  const result = plan();
  assert.equal(result.full, false);
  assert.ok(Object.values(result.filtered).every((value) => value === "false"));
});

for (const file of ["new-tool/config.toml", "docs/build.sh", "new-tool/image.png"]) {
  test(`manual unknown path forces full coverage: ${file}`, (t) => {
    const { commit, plan } = gitFixture(t);
    commit({ [file]: "unknown", "apps/web/a.tsx": "known" });
    assert.equal(plan().full, true);
    assert.match(plan().reason, /Unknown/);
  });
}

test("manual default branch, tags, absent context, empty diff and explicit full never pass as no work", (t) => {
  const { plan } = gitFixture(t);
  for (const options of [{}, { ref: "refs/heads/main" }, { ref: "refs/tags/v1" }, { ref: "" }, { defaultBranch: "" }, { full: "true" }]) {
    const result = plan(options);
    assert.equal(result.full, true);
    const outputs = decideScopes("workflow_dispatch", result.filtered, result);
    assert.equal(outputs.full, "true");
    assert.equal(outputs.macos_runtime, "true");
    assert.ok(Object.keys(filters).every((scope) => outputs[scope] === "true"));
  }
  assert.throws(() => plan({ full: "typo" }), /full/);
});

test("manual missing baseline, shallow checkout and unrelated history fall back to full", (t) => {
  const { cwd, git, commit, plan, base } = gitFixture(t);
  commit({ "apps/web/a.tsx": "feature" });
  git("update-ref", "-d", "refs/remotes/origin/main");
  assert.equal(plan().full, true);
  git("update-ref", "refs/remotes/origin/main", base);
  writeFileSync(join(cwd, ".git/shallow"), `${base}\n`);
  assert.equal(plan().full, true);
  rmSync(join(cwd, ".git/shallow"));
  git("switch", "--orphan", "unrelated");
  commit({ "apps/web/b.tsx": "unrelated" });
  assert.equal(plan().full, true);
});

test("manual checkout revision mismatch fails instead of validating the wrong code", (t) => {
  const { commit, plan, base } = gitFixture(t);
  commit({ "apps/web/a.tsx": "feature" });
  assert.throws(() => plan({ sha: base }), /revision/);
});

test("manual hidden files cannot match only the image scope and miss their product suite", (t) => {
  const { commit, plan } = gitFixture(t);
  commit({ "packages/.hidden/icon.png": "hidden" });
  assert.equal(plan().full, true);
  assert.match(plan().reason, /Hidden/);
});

test("scope patterns remain in the literal, star and globstar subset shared by both matchers", () => {
  for (const pattern of Object.values(filters).flat()) assert.match(pattern, /^[\w./*-]+$/);
});

test("manual workflow wires the default, comparison history, input and platform gate", () => {
  assert.match(workflow, /workflow_dispatch:\n    inputs:\n      full:\n        description: .+\n        type: boolean\n        default: false/);
  assert.match(workflow, /schedule:\n    - cron: "23 3 \* \* \*"/);
  assert.match(jobs.changes, /fetch-depth: \$\{\{ github.event_name == 'workflow_dispatch' && !inputs.full && '0' \|\| '1' \}\}/);
  assert.match(jobs.changes, /FULL_RUN: \$\{\{ inputs.full \}\}/);
  assert.match(jobs.changes, /DEFAULT_BRANCH: \$\{\{ github.event.repository.default_branch \}\}/);
  assert.equal(productionMapping("backend")["macos-runtime"], "macos_runtime");
  assert.equal(productionMapping("backend")["windows-execenv"], "runtime");
});

test("manual CLI writes scoped outputs and a visible explanation, with explicit full override", (t) => {
  const { cwd, commit } = gitFixture(t);
  const sha = commit({ "apps/web/a.tsx": "feature" });
  const script = fileURLToPath(new URL("./ci-scope.mjs", import.meta.url));
  for (const full of ["false", "true"]) {
    const output = join(cwd, `output-${full}`);
    const summary = join(cwd, `summary-${full}`);
    const result = spawnSync(process.execPath, [script, "decide"], {
      cwd, encoding: "utf8", env: {
        ...process.env, EVENT_NAME: "workflow_dispatch", FULL_RUN: full,
        GITHUB_REF: "refs/heads/feature", GITHUB_SHA: sha, DEFAULT_BRANCH: "main",
        GITHUB_OUTPUT: output, GITHUB_STEP_SUMMARY: summary,
      },
    });
    assert.equal(result.status, 0, result.stderr);
    assert.match(readFileSync(output, "utf8"), new RegExp(`^full=${full}$`, "m"));
    assert.match(readFileSync(output, "utf8"), new RegExp(`^backend=${full}$`, "m"));
    assert.match(readFileSync(output, "utf8"), /^frontend=true$/m);
    assert.match(readFileSync(summary, "utf8"), full === "true" ? /Explicit full/ : /Incremental validation:/);
    assert.match(result.stdout, /Selected scopes:/);
  }
});

test("manual ambiguous criss-cross merge bases force full validation", (t) => {
  const { git, commit, plan, base } = gitFixture(t);
  const left = commit({ "apps/web/left.tsx": "left" });
  git("checkout", "--detach", base);
  const right = commit({ "apps/web/right.tsx": "right" });
  const tree = git("rev-parse", "HEAD^{tree}");
  const merge = (a, b) => git("-c", "user.name=CI Test", "-c", "user.email=ci@example.invalid",
    "commit-tree", tree, "-p", a, "-p", b, "-m", "merge fixture");
  const target = merge(left, right);
  const head = merge(right, left);
  git("update-ref", "refs/remotes/origin/main", target);
  git("checkout", "--detach", head);
  assert.equal(git("merge-base", "--all", target, head).split("\n").length, 2);
  assert.equal(plan().full, true);
  assert.match(plan().reason, /unique merge base/);
});

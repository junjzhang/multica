# DSH 0.2 runtime upgrade

WORK-112 upgrades the independently maintained Multica bridge without importing
the plugin into the application workspace. The installed `0.1.0-private.1`
bridge rejects DSH `0.2.1-rc.5`; permitting that version alone also leaves the
old tool-result format and lifecycle behavior in place.

Use [bridge revision 571d7c0](https://github.com/multica-ai/dsh-multica-runtime/commit/571d7c036d15f58ee88c89fd80afb0a6c7f0023c).
It updates tool-result messages, file blocks, descendant settling/cancellation,
`personaPrefix`, and the telemetry entry name for DSH 0.2. The build patch in
`scripts/dsh-runtime/rc5-build.patch` pins its development package family and
lockfile to the tested `0.2.1-rc.5`. It preserves upstream's compatibility guard.
Only that launcher version is qualified here; the upstream declared range is
not evidence that all later releases work.

## Build and isolated verification

From the repository root, choose an empty directory owned by this task:

```bash
node scripts/dsh-runtime/build.mjs .context/dsh-build/plugin
```

The script fetches the exact bridge revision, applies the build pins, runs
typecheck/tests/build, and produces `.context/dsh-build/dsh-runtime-rc5.tgz`.
It refuses to overwrite an existing build directory. This needs Git, Node.js
22.19+ or 24+, pnpm, and registry access. The application package lock is untouched.

Create a private isolated DSH home and copy only the existing Multica profile's
`package.json`, `cordis.yml`, and `cordis.patch.yml` into its `profiles/multica/`
directory. Retain the provider/model selection, endpoint and credential reference.
Add a `pnpm-workspace.yaml` containing `packages: []` in that profile directory
before invoking `dsh plugin`, so pnpm cannot discover an enclosing application
workspace. Do not copy session history, credentials, or change the live profile.

```bash
DSH_HOME=/absolute/isolated/home dsh plugin --profile multica add /absolute/dsh-runtime-rc5.tgz
DSH_HOME=/absolute/isolated/home dsh --profile multica --probe
DSH_HOME=/absolute/isolated/home dsh --profile multica --list-models
```

Use the configured provider's existing credential source for model discovery and
execution. DSH 0.2's pi-ai adapter returns an empty list for a provider whose
`apiKeyEnv` credential is absent. Do not remove the credential reference or
replace the model to make the test pass. Keep credentials in process environment,
outside logs, command arguments, and committed files.

The opt-in backend smoke crosses the real JSONL process boundary, checks tool
call/result IDs and names, checks a file written by the tool, and resumes the
same durable session in another process. It creates an isolated session root.
Run it only when real-model testing is authorized:

```bash
cd server
DSH_HOME=/absolute/isolated/home \
MULTICA_DSH_PATH=/absolute/current/dsh \
MULTICA_DSH_SMOKE_MODEL=provider/model \
MULTICA_RUN_REAL_AGENT_SMOKE=1 CGO_ENABLED=0 \
go test -tags=agentintegration ./pkg/agent -run '^TestDshRealRuntimeSmoke$' -count=1 -v
```

To qualify migration, also set `MULTICA_DSH_SMOKE_PREVIOUS_PATH` to the old
launcher and `MULTICA_DSH_SMOKE_PREVIOUS_HOME` to its profile home. The first
turn then runs with the old bridge, and the second uses the new bridge against
that session. The test never opens a user's existing session.

## Host cutover and rollback

Cutover must run from the host operator's shell. Managed task credentials cannot
start/stop/restart the daemon or mutate local runtime paths; restarting the
hosting daemon also interrupts the task doing the upgrade.

1. Record `multica daemon status --output json`, active DSH runs and their
   session roots. Wait for active runs to finish. Back up the Multica profile
   manifests, lockfile, overrides and the old bridge build. Retain the old DSH
   installation until migration and managed execution are accepted.
2. Install the built tarball into the real `multica` profile. Keep its provider
   override and session-root layout. Validate the normal PATH entry with
   `dsh --profile multica --probe` and `--list-models`, using the same provider
   credential source the daemon/agents normally use.
3. Remove `MULTICA_DSH_PATH` from the host daemon's startup environment or service
   override, then restart the daemon normally. Confirm its registered DSH version
   is `0.2.1-rc.5`, and run an isolated managed task to terminal state. Verify
   model output, tool events, stored result and return delivery. Resume it and
   verify context survives. An `online` row alone is insufficient.
4. On failure, finish or stop only the new test run, restore the backed-up
   profile and old launcher override, and restart from the host. Preserve all
   session files and task records; never roll back the database or delete
   upgraded sessions. Do not assume an old bridge can read a session written by
   a newer bridge.

The daemon change in this task retains a bounded, redacted probe stderr detail
in the existing skipped-runtime reason, including when a compatibility refusal
is followed by a timeout. Valid probe frames still come exclusively from stdout.
An installed profile's failed probe retains its existing non-destructive
classification; this change does not overwrite or demote it.

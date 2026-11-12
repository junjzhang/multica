import { mkdir, readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import type { PaseoHostAdapter } from "../server/host";
import { MulticaApiError } from "../server/api";
import { ConfigurationError } from "../server/owner-profile";
import { createSessionService } from "../server/service";
import {
  contextPath,
  readSessionContext,
  removeSessionContext,
  writeSessionContext,
} from "../server/store";
import {
  AGENT_A,
  AGENT_B,
  DAEMON_ID,
  EXTERNAL_TOKEN,
  HOST_ID,
  OWNER_TOKEN,
  SERVER_URL,
  SESSION_A,
  SESSION_B,
  WORKSPACE_ID,
  FakeApi,
  fakeHost,
  makeContext,
  makeHome,
  makeTasksWire,
} from "./fixtures";

const NOW = new Date("2026-10-26T12:00:00.000Z");
const TTL = 90 * 24 * 60 * 60 * 1000;

async function setup(options: {
  api?: FakeApi;
  host?: PaseoHostAdapter;
  settings?: { ownerProfile: string; daemonId: string };
  home?: string;
} = {}) {
  const home = options.home ?? (await makeHome());
  const api = options.api ?? new FakeApi();
  const service = createSessionService({
    home,
    paseo: options.host ?? fakeHost(),
    api,
    readSettings: async () => options.settings ?? { ownerProfile: "", daemonId: DAEMON_ID },
    now: () => NOW,
    tokenTtlMs: TTL,
  });
  return { home, api, service };
}

describe("session service", () => {
  it("registers the current session once and writes its v1 binding", async () => {
    const { home, api, service } = await setup();
    const view = await service.connect(SESSION_A);

    expect(view.state).toBe("connected");
    expect(view.agentId).toBe(AGENT_A);
    expect(view.workspaceId).toBe(WORKSPACE_ID);
    expect(view.returnAddress).toBe(`paseo://h/${HOST_ID}/agent/${SESSION_A}`);
    expect(view.expiresAt).toBe(new Date(NOW.getTime() + TTL).toISOString());
    expect(api.registrations).toHaveLength(1);
    expect(api.registrations[0]).toMatchObject({
      hostId: HOST_ID,
      sessionId: SESSION_A,
      daemonId: DAEMON_ID,
      url: `paseo://h/${HOST_ID}/agent/${SESSION_A}`,
    });
    expect(api.tokensRequested).toHaveLength(1);
    expect(api.credentialCalls[0]?.token).toBe(OWNER_TOKEN);

    const written = await readSessionContext(home, SESSION_A);
    expect(written.status).toBe("ok");
    if (written.status === "ok") {
      expect(written.context).toMatchObject({
        version: 1,
        provider: "paseo",
        host_id: HOST_ID,
        session_id: SESSION_A,
        agent_id: AGENT_A,
        daemon_id: DAEMON_ID,
        server_url: SERVER_URL,
        workspace_id: WORKSPACE_ID,
        token: EXTERNAL_TOKEN,
      });
    }
    expect(JSON.stringify(view)).not.toContain(EXTERNAL_TOKEN);
    expect(JSON.stringify(view)).not.toContain(OWNER_TOKEN);
  });

  it("reuses a valid binding instead of registering or minting again", async () => {
    const { api, service } = await setup();
    await service.connect(SESSION_A);
    const second = await service.connect(SESSION_A);

    expect(second.state).toBe("connected");
    expect(api.registrations).toHaveLength(1);
    expect(api.tokensRequested).toHaveLength(1);
    expect(api.verifyCalls).toBe(1);
    // The credential is checked against the Multica agent it was minted for,
    // never against the composer's Paseo session id.
    expect(api.verified).toEqual([{ agentId: AGENT_A, token: EXTERNAL_TOKEN }]);
  });

  it("reports a remotely revoked credential from status without repairing it", async () => {
    const { home, api, service } = await setup();
    await service.connect(SESSION_A);
    api.verifyResult = "invalid";

    const view = await service.status(SESSION_A);
    expect(view.state).toBe("invalid");
    expect(view.reason).toMatch(/no longer accepts/);
    expect(view.reason).toMatch(/\/multica connect/);
    expect(api.tokensRequested).toHaveLength(1);
    expect((await readSessionContext(home, SESSION_A)).status).toBe("ok");
  });

  it("does not claim connected when the remote check cannot complete", async () => {
    const { api, service } = await setup();
    await service.connect(SESSION_A);
    api.verifyResult = "throw";

    const view = await service.status(SESSION_A);
    expect(view.state).toBe("unverified");
    expect(view.reason).toMatch(/network unavailable/);
    expect(view.reason).toMatch(/local binding was left unchanged/);
  });

  it("rejects a local credential minted for another Multica agent", async () => {
    const { home, api, service } = await setup();
    await service.connect(SESSION_A);
    const binding = await readSessionContext(home, SESSION_A);
    expect(binding.status).toBe("ok");
    if (binding.status !== "ok") return;

    // A context that declares agent B while carrying agent A's credential.
    await writeSessionContext(home, { ...binding.context, agent_id: AGENT_B });

    const view = await service.status(SESSION_A);
    expect(view.state).toBe("invalid");
    expect(api.verified.at(-1)).toEqual({ agentId: AGENT_B, token: EXTERNAL_TOKEN });

    await expect(service.connect(SESSION_A)).rejects.toThrow(/different agent identity/);
    expect(api.tokensRequested).toHaveLength(1);
    expect((await readSessionContext(home, SESSION_A)).status).toBe("ok");
  });

  it("serializes concurrent connects into one identity and credential", async () => {
    const { home, api, service } = await setup();
    const views = await Promise.all(Array.from({ length: 6 }, () => service.connect(SESSION_A)));

    expect(views.every((view) => view.state === "connected")).toBe(true);
    expect(api.registrations).toHaveLength(1);
    expect(api.tokensRequested).toHaveLength(1);
    expect(api.verifyCalls).toBe(5);
    const written = await readSessionContext(home, SESSION_A);
    expect(written.status).toBe("ok");
  });

  it("keeps two sessions isolated", async () => {
    const { home, api, service } = await setup();
    const a = await service.connect(SESSION_A);
    const b = await service.connect(SESSION_B);

    expect(a.agentId).toBe(AGENT_A);
    expect(b.agentId).not.toBe(AGENT_A);
    expect(api.registrations.map((entry) => entry.sessionId)).toEqual([SESSION_A, SESSION_B]);
    expect(api.tokensRequested).toHaveLength(2);
    expect(await readFile(contextPath(home, SESSION_A), "utf8")).toContain(AGENT_A);
    expect(await readFile(contextPath(home, SESSION_B), "utf8")).not.toContain(AGENT_A);
  });

  it("falls back to per-session registration when templates are disabled", async () => {
    const { api, service } = await setup();
    api.templateDisabled = true;

    const a = await service.connect(SESSION_A);
    const b = await service.connect(SESSION_B);

    expect(a.state).toBe("connected");
    expect(b.state).toBe("connected");
    expect(api.registrations).toHaveLength(2);
    for (const entry of api.registrations) {
      expect(entry.template).toBe(false);
      expect(entry.templateName).toBeUndefined();
      // The legacy name is the one the previous plugin version used, so a
      // second session of the same host never collides with the template name.
      expect(entry.name).toBe("Workstation · Fix the build");
    }
    expect(api.tokensRequested).toHaveLength(2);
  });

  it("repairs an expired credential only through explicit connect", async () => {
    const { home, api, service } = await setup();
    const expired = makeContext({ expires_at: new Date(NOW.getTime() - 60_000).toISOString() });
    await writeSessionContext(home, expired);

    const before = await service.status(SESSION_A);
    expect(before.state).toBe("invalid");
    expect(before.reason).toMatch(/expired/);
    expect(api.tokensRequested).toHaveLength(0);

    const after = await service.connect(SESSION_A);
    expect(after.state).toBe("connected");
    expect(api.tokensRequested).toHaveLength(1);
    expect(api.verifyCalls).toBe(0);
    const written = await readSessionContext(home, SESSION_A);
    expect(written.status).toBe("ok");
    if (written.status === "ok") expect(written.context.expires_at).toBe(after.expiresAt);
  });

  it("repairs a revoked credential with a fresh token for the same agent", async () => {
    const { home, api, service } = await setup();
    const existing = makeContext({ expires_at: new Date(NOW.getTime() + 60_000).toISOString() });
    await writeSessionContext(home, existing);
    api.verifyResult = "invalid";

    const view = await service.connect(SESSION_A);
    expect(view.state).toBe("connected");
    expect(api.verifyCalls).toBe(1);
    expect(api.registrations).toHaveLength(1);
    expect(api.tokensRequested).toHaveLength(1);
    const written = await readSessionContext(home, SESSION_A);
    expect(written.status).toBe("ok");
    if (written.status === "ok") {
      expect(written.context.agent_id).toBe(AGENT_A);
      expect(written.context.token_id).not.toBe(existing.token_id);
    }
  });

  it("does not mint a replacement when credential verification is inconclusive", async () => {
    const { home, api, service } = await setup();
    const existing = makeContext({ expires_at: new Date(NOW.getTime() + 60_000).toISOString() });
    await writeSessionContext(home, existing);
    api.verifyResult = "throw";

    await expect(service.connect(SESSION_A)).rejects.toThrow(/network unavailable/);
    expect(api.tokensRequested).toHaveLength(0);
    const written = await readSessionContext(home, SESSION_A);
    expect(written.status).toBe("ok");
    if (written.status === "ok") expect(written.context.token_id).toBe(existing.token_id);
  });

  it("refuses to rebind a binding issued for another host", async () => {
    const { home, api, service } = await setup();
    const foreign = makeContext({ host_id: "srv_other" });
    await writeSessionContext(home, foreign);

    await expect(service.connect(SESSION_A)).rejects.toThrow(/Refusing to rebind/);
    expect(api.registrations).toHaveLength(0);
    expect(api.tokensRequested).toHaveLength(0);
    const written = await readSessionContext(home, SESSION_A);
    expect(written.status).toBe("ok");
    if (written.status === "ok") expect(written.context.host_id).toBe("srv_other");
  });

  it("refuses to overwrite a context owned by another session", async () => {
    const { home, api, service } = await setup();
    await mkdir(join(home, ".multica", "external-sessions", "paseo"), { recursive: true });
    await writeFile(contextPath(home, SESSION_A), JSON.stringify({ ...makeContext(), session_id: "other" }));

    await expect(service.connect(SESSION_A)).rejects.toThrow(/Refusing to overwrite/);
    expect(api.registrations).toHaveLength(0);
  });

  it("replaces a malformed context because the path is this session's own", async () => {
    const { home, api, service } = await setup();
    await mkdir(join(home, ".multica", "external-sessions", "paseo"), { recursive: true });
    await writeFile(contextPath(home, SESSION_A), "{broken");
    const view = await service.connect(SESSION_A);

    expect(view.state).toBe("connected");
    expect(api.tokensRequested).toHaveLength(1);
    expect((await readSessionContext(home, SESSION_A)).status).toBe("ok");
  });

  it("reports an archived session and never registers it", async () => {
    const { api, service } = await setup({ host: fakeHost({ archivedAt: "2026-10-26T00:00:00Z" }) });
    await expect(service.connect(SESSION_A)).rejects.toThrow(/archived/);
    expect(api.registrations).toHaveLength(0);
    expect(api.tokensRequested).toHaveLength(0);
  });

  it("refuses to bind a session the host reports under another id", async () => {
    const { api, service } = await setup({ host: fakeHost({ reportedId: SESSION_B }) });
    await expect(service.connect(SESSION_A)).rejects.toThrow(/different session id/);
    expect(api.registrations).toHaveLength(0);
    expect(api.tokensRequested).toHaveLength(0);
  });

  it("reports a missing delivery daemon instead of guessing one", async () => {
    const { api, service } = await setup({ settings: { ownerProfile: "", daemonId: "" } });
    await expect(service.connect(SESSION_A)).rejects.toThrow(/delivery daemon/);
    expect(api.registrations).toHaveLength(0);
  });

  it.each([
    { revokeError: null, recovery: /The newly issued credential was revoked; Run multica session connect/, revokedAgents: [AGENT_A] },
    { revokeError: new Error("network failed"), recovery: new RegExp(`Revoke token [0-9a-f-]{36} for agent ${AGENT_A} from the owner profile`), revokedAgents: [] },
  ])("keeps actionable recovery for a save failure ($recovery)", async ({ revokeError, recovery, revokedAgents }) => {
    const home = await makeHome();
    const api = new FakeApi();
    api.revokeError = revokeError;
    const service = createSessionService({
      home,
      paseo: fakeHost(),
      api,
      store: {
        read: readSessionContext,
        write: async () => {
          throw new Error(`disk full ${EXTERNAL_TOKEN}`);
        },
        remove: removeSessionContext,
      },
      readSettings: async () => ({ ownerProfile: "", daemonId: DAEMON_ID }),
      now: () => NOW,
    });

    const error = await service.connect(SESSION_A).catch((error: unknown) => error);
    expect(error).toBeInstanceOf(ConfigurationError);
    if (!(error instanceof Error)) throw new Error("Expected connection error");
    expect(error.message).toMatch(/^\[multica:session-connect\] /);
    expect(error.message).toMatch(recovery);
    expect(error.message).toContain("disk full [redacted]");
    expect(error.message).not.toContain(EXTERNAL_TOKEN);
    expect(api.revoked.map((record) => record.agentId)).toEqual(revokedAgents);
  });

  it("reports status, revocation failure, and idempotent disconnect", async () => {
    const { home, api, service } = await setup();
    const connected = await service.connect(SESSION_A);
    const binding = await readSessionContext(home, SESSION_A);
    expect(binding.status).toBe("ok");
    if (binding.status !== "ok") return;

    expect(connected.state).toBe("connected");
    api.revokeError = new Error("server unavailable");
    await expect(service.disconnect(SESSION_A)).rejects.toThrow(/local context was kept/);
    expect((await readSessionContext(home, SESSION_A)).status).toBe("ok");
    expect(api.revoked).toHaveLength(0);

    api.revokeError = null;
    const result = await service.disconnect(SESSION_A);
    expect(result.state).toBe("disconnected");
    expect(api.revoked).toEqual([{ agentId: AGENT_A, tokenId: binding.context.token_id }]);
    expect((await readSessionContext(home, SESSION_A)).status).toBe("missing");

    const again = await service.disconnect(SESSION_A);
    expect(again.state).toBe("disconnected");
  });

  it("disconnects only the requested session and leaves its peer intact", async () => {
    const { home, api, service } = await setup();
    await service.connect(SESSION_A);
    await service.connect(SESSION_B);
    const bindingA = await readSessionContext(home, SESSION_A);
    const bindingB = await readSessionContext(home, SESSION_B);
    expect(bindingA.status).toBe("ok");
    expect(bindingB.status).toBe("ok");
    if (bindingA.status !== "ok" || bindingB.status !== "ok") return;

    await service.disconnect(SESSION_A);

    expect(api.revoked).toEqual([{ agentId: AGENT_A, tokenId: bindingA.context.token_id }]);
    expect(api.revoked.some((entry) => entry.tokenId === bindingB.context.token_id)).toBe(false);
    expect((await readSessionContext(home, SESSION_A)).status).toBe("missing");
    expect((await readSessionContext(home, SESSION_B)).status).toBe("ok");
  });

  it.each([
    ["server", { server_url: "https://other.example" }],
    ["workspace", { workspace_id: "0199a0b1-4444-7444-8444-444444444444" }],
  ] as const)("keeps the binding when the owner profile cannot revoke it (%s mismatch)", async (_label, overrides) => {
    const { home, api, service } = await setup();
    const context = makeContext(overrides);
    await writeSessionContext(home, context);

    const error = await service.disconnect(SESSION_A).catch((cause: unknown) => cause);
    expect(error).toBeInstanceOf(ConfigurationError);
    const message = error instanceof Error ? error.message : "";
    expect(message).toContain(context.agent_id);
    expect(message).toContain(context.token_id);
    expect(message).toContain(context.server_url);
    expect(message).toContain(context.workspace_id);
    expect(message).toMatch(/owner profile/);
    expect(message).toMatch(/local context was kept/);

    expect(api.revoked).toHaveLength(0);
    const kept = await readSessionContext(home, SESSION_A);
    expect(kept.status).toBe("ok");
    if (kept.status === "ok") expect(kept.context.token_id).toBe(context.token_id);
  });

  it("reports disconnected and invalid states without a credential", async () => {
    const { home, service } = await setup();
    const missing = await service.status(SESSION_A);
    expect(missing.state).toBe("disconnected");
    expect(missing.reason).toMatch(/\/multica connect/);

    await writeSessionContext(home, makeContext({ expires_at: new Date(NOW.getTime() - 1_000).toISOString() }));
    const expired = await service.status(SESSION_A);
    expect(expired.state).toBe("invalid");
    expect(expired.expiresAt).toBeDefined();
    expect(JSON.stringify(expired)).not.toContain(EXTERNAL_TOKEN);

    const malformedHome = await makeHome();
    await mkdir(join(malformedHome, ".multica", "external-sessions", "paseo"), { recursive: true });
    await writeFile(contextPath(malformedHome, SESSION_A), "{broken");
    const malformed = await createSessionService({
      home: malformedHome,
      paseo: fakeHost(),
      api: new FakeApi(),
      readSettings: async () => ({ ownerProfile: "", daemonId: DAEMON_ID }),
      now: () => NOW,
    }).status(SESSION_A);
    expect(malformed.state).toBe("invalid");
  });
});
describe("session tasks service", () => {
  it("returns the parsed server projection for the bound session", async () => {
    const { home, api, service } = await setup();
    await writeSessionContext(home, makeContext());
    api.tasksResult = makeTasksWire({
      owned: [
        {
          issue_id: "0199a0b1-4444-7444-8444-444444444444",
          identifier: "WORK-1",
          title: "Owned task",
          status: "in_progress",
          status_category: "started",
          url: "https://app.multica.example/acme/issues/WORK-1",
          role: "executor",
          updated_at: "2026-10-26T11:00:00Z",
        },
        {
          issue_id: "0199a0b1-7777-7777-8777-777777777777",
          title: "Unsafe link",
          status: "todo",
          status_category: "unstarted",
          url: "javascript:alert(1)",
          role: "leader",
        },
      ],
    });

    const view = await service.tasks(SESSION_A);
    expect(view.state).toBe("ready");
    if (view.state !== "ready") return;
    expect(view.agentId).toBe(AGENT_A);
    expect(view.sessionId).toBe(api.tasksResult.session_id);
    expect(view.checkedAt).toBe(NOW.toISOString());
    expect(view.owned[0]).toMatchObject({
      issueId: "0199a0b1-4444-7444-8444-444444444444",
      identifier: "WORK-1",
      role: "executor",
      url: "https://app.multica.example/acme/issues/WORK-1",
    });
    expect(view.owned[1]?.url).toBeUndefined();
    expect(view.accepting[0]?.role).toBe("acceptor");
    expect(view.returning[0]).toMatchObject({ role: "return", taskStatus: "running" });
    expect(api.tasksCalls).toEqual([{ agentId: AGENT_A, token: EXTERNAL_TOKEN }]);
    expect(JSON.stringify(view)).not.toContain(EXTERNAL_TOKEN);
  });

  it("explains a missing binding and never calls the server", async () => {
    const { api, service } = await setup();
    const view = await service.tasks(SESSION_A);
    expect(view.state).toBe("disconnected");
    if (view.state !== "disconnected") return;
    expect(view.reason).toMatch(/no Multica binding/);
    expect(view.reason).toMatch(/\/multica connect/);
    expect(api.tasksCalls).toHaveLength(0);
  });

  it("explains a malformed local context instead of guessing", async () => {
    const home = await makeHome();
    await mkdir(join(home, ".multica", "external-sessions", "paseo"), { recursive: true });
    await writeFile(contextPath(home, SESSION_A), "{broken");
    const { service } = await setup({ home });
    const view = await service.tasks(SESSION_A);
    expect(view.state).toBe("disconnected");
    if (view.state !== "disconnected") return;
    expect(view.reason).toMatch(/not valid JSON/);
  });

  it("does not send an expired credential and says how to repair it", async () => {
    const { home, api, service } = await setup();
    await writeSessionContext(home, makeContext({ expires_at: new Date(NOW.getTime() - 1_000).toISOString() }));
    const view = await service.tasks(SESSION_A);
    expect(view.state).toBe("unavailable");
    if (view.state !== "unavailable") return;
    expect(view.reason).toMatch(/expired/);
    expect(view.reason).toMatch(/\/multica connect/);
    expect(api.tasksCalls).toHaveLength(0);
  });

  it.each([
    [403, /no registered session/],
    [404, /does not expose session task summaries/],
    [401, /no longer accepts/],
  ] as const)("degrades a %d response to an explanatory view", async (status, reason) => {
    const { home, api, service } = await setup();
    await writeSessionContext(home, makeContext());
    api.tasksResult = new MulticaApiError(status, "rejected");
    const view = await service.tasks(SESSION_A);
    expect(view.state).toBe("unavailable");
    if (view.state !== "unavailable") return;
    expect(view.reason).toMatch(reason);
  });

  it("degrades a transport failure without leaking the credential", async () => {
    const { home, api, service } = await setup();
    await writeSessionContext(home, makeContext());
    api.tasksResult = new Error(`socket closed for ${EXTERNAL_TOKEN}`);
    const view = await service.tasks(SESSION_A);
    expect(view.state).toBe("unavailable");
    if (view.state !== "unavailable") return;
    expect(view.reason).toMatch(/Could not read this session's Multica tasks/);
    expect(view.reason).not.toContain(EXTERNAL_TOKEN);
  });
});

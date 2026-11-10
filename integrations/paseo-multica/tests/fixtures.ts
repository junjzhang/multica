import { randomUUID } from "node:crypto";
import { mkdir, mkdtemp, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  MulticaApiError,
  type ApiCredential,
  type ExternalAgentRegistration,
  type IssuedToken,
  type MulticaApi,
  type RegisterExternalAgentInput,
} from "../server/api";
import type { PaseoHostAdapter } from "../server/host";
import type { SessionContext } from "../shared/session";

export const OWNER_TOKEN = "mul_owner_token_must_never_leak_0123456789";
export const EXTERNAL_TOKEN = "mea_external_token_must_never_leak_0123456789";
export const WORKSPACE_ID = "0199a0b1-1111-7111-8111-111111111111";
export const AGENT_A = "0199a0b1-2222-7222-8222-222222222222";
export const AGENT_B = "0199a0b1-3333-7333-8333-333333333333";
export const HOST_ID = "srv_workstation";
export const DAEMON_ID = "daemon_workstation";
export const SERVER_URL = "https://multica.example";
export const APP_URL = "https://app.multica.example";
export const SESSION_A = "paseo-session-a";
export const SESSION_B = "paseo-session-b";

export async function makeHome(): Promise<string> {
  const home = await mkdtemp(join(tmpdir(), "paseo-multica-test-"));
  await mkdir(join(home, ".multica"), { recursive: true });
  await writeFile(
    join(home, ".multica", "config.json"),
    JSON.stringify({
      token: OWNER_TOKEN,
      server_url: SERVER_URL,
      app_url: APP_URL,
      workspace_id: WORKSPACE_ID,
    }),
  );
  return home;
}

export class FakeApi implements MulticaApi {
  readonly registrations: RegisterExternalAgentInput[] = [];
  readonly credentialCalls: ApiCredential[] = [];
  readonly tokensRequested: { agentId: string; sessionId: string; expiresAt: string }[] = [];
  readonly revoked: { agentId: string; tokenId: string }[] = [];
  readonly verified: { agentId: string; token: string }[] = [];
  verifyCalls = 0;
  verifyResult: "subject" | "invalid" | "throw" = "subject";
  registerError: Error | null = null;
  /** Models a server whose template-write switch is still closed. */
  templateDisabled = false;
  tokenError: Error | null = null;
  revokeError: Error | null = null;
  private tokenSequence = 0;
  private readonly tokenSubjects = new Map<string, string>();
  private readonly registeredSessions = new Map<string, string>();

  async registerExternalAgent(
    credential: ApiCredential,
    input: RegisterExternalAgentInput,
  ): Promise<ExternalAgentRegistration> {
    this.credentialCalls.push(credential);
    if (this.registerError) throw this.registerError;
    if (input.template && this.templateDisabled) {
      throw new MulticaApiError(
        400,
        'Multica API POST /api/agents/external returned 400: {"code":"external_agent_templates_disabled","error":"external agent templates are not enabled on this server"}',
        "external_agent_templates_disabled",
      );
    }
    this.registrations.push(input);
    let registered = this.registeredSessions.get(input.sessionId);
    if (!registered) {
      registered = randomUUID();
      this.registeredSessions.set(input.sessionId, registered);
    }
    return {
      id: input.sessionId === SESSION_B ? AGENT_B : AGENT_A,
      name: input.name,
      sessionId: registered,
      ...(input.template ? { templateId: randomUUID() } : {}),
    };
  }

  async createExternalAgentToken(
    credential: ApiCredential,
    agentId: string,
    externalSessionId: string,
    expiresAt: string,
  ): Promise<IssuedToken> {
    this.credentialCalls.push(credential);
    if (this.tokenError) throw this.tokenError;
    this.tokensRequested.push({ agentId, sessionId: externalSessionId, expiresAt });
    this.tokenSequence += 1;
    const token = this.tokenSequence === 1 ? EXTERNAL_TOKEN : `${EXTERNAL_TOKEN}-${this.tokenSequence}`;
    this.tokenSubjects.set(token, agentId);
    return { id: randomUUID(), token, expiresAt };
  }

  async revokeExternalAgentToken(credential: ApiCredential, agentId: string, tokenId: string): Promise<void> {
    this.credentialCalls.push(credential);
    if (this.revokeError) throw this.revokeError;
    this.revoked.push({ agentId, tokenId });
  }

  async verifyExternalAgentToken(credential: ApiCredential, agentId: string): Promise<"valid" | "invalid"> {
    this.verifyCalls += 1;
    this.verified.push({ agentId, token: credential.token });
    if (this.verifyResult === "throw") throw new Error("network unavailable");
    if (this.verifyResult === "invalid") return "invalid";
    // The real mailbox endpoint authorizes the credential's subject, so the
    // path agent must be the agent the token was minted for.
    return this.tokenSubjects.get(credential.token) === agentId ? "valid" : "invalid";
  }
}

export interface FakeHostOptions {
  serverId?: string;
  label?: string;
  title?: string | null;
  archivedAt?: string | null;
  exists?: boolean;
  reportedId?: string;
}

export function fakeHost(options: FakeHostOptions = {}): PaseoHostAdapter {
  const {
    serverId = HOST_ID,
    label = "Workstation",
    title = "Fix the build",
    archivedAt = null,
    exists = true,
    reportedId,
  } = options;
  return {
    async listHosts() {
      return { self: { serverId, label } };
    },
    async refreshAgent(agentId) {
      if (!exists) return null;
      return { agent: { id: reportedId ?? agentId, title, archivedAt } };
    },
  };
}

export function makeContext(overrides: Partial<SessionContext> = {}): SessionContext {
  return {
    version: 1,
    provider: "paseo",
    host_id: HOST_ID,
    session_id: SESSION_A,
    agent_id: AGENT_A,
    daemon_id: DAEMON_ID,
    server_url: SERVER_URL,
    app_url: APP_URL,
    workspace_id: WORKSPACE_ID,
    token: EXTERNAL_TOKEN,
    token_id: randomUUID(),
    expires_at: new Date(Date.now() + 60_000).toISOString(),
    ...overrides,
  };
}

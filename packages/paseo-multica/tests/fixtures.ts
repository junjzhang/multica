import { randomUUID } from "node:crypto";
import { mkdir, mkdtemp, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type {
  ApiCredential,
  ExternalAgentRegistration,
  IssuedToken,
  MulticaApi,
  RegisterExternalAgentInput,
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
  readonly tokensRequested: { agentId: string; expiresAt: string }[] = [];
  readonly revoked: { agentId: string; tokenId: string }[] = [];
  verifyCalls = 0;
  verifyResult: "valid" | "invalid" | "throw" = "valid";
  registerError: Error | null = null;
  tokenError: Error | null = null;
  revokeError: Error | null = null;
  private tokenSequence = 0;

  async registerExternalAgent(
    credential: ApiCredential,
    input: RegisterExternalAgentInput,
  ): Promise<ExternalAgentRegistration> {
    this.credentialCalls.push(credential);
    if (this.registerError) throw this.registerError;
    this.registrations.push(input);
    return { id: input.sessionId === SESSION_B ? AGENT_B : AGENT_A, name: input.name };
  }

  async createExternalAgentToken(
    credential: ApiCredential,
    agentId: string,
    expiresAt: string,
  ): Promise<IssuedToken> {
    this.credentialCalls.push(credential);
    if (this.tokenError) throw this.tokenError;
    this.tokensRequested.push({ agentId, expiresAt });
    this.tokenSequence += 1;
    return { id: randomUUID(), token: EXTERNAL_TOKEN, expiresAt };
  }

  async revokeExternalAgentToken(credential: ApiCredential, agentId: string, tokenId: string): Promise<void> {
    this.credentialCalls.push(credential);
    if (this.revokeError) throw this.revokeError;
    this.revoked.push({ agentId, tokenId });
  }

  async verifyExternalAgentToken(): Promise<"valid" | "invalid"> {
    this.verifyCalls += 1;
    if (this.verifyResult === "throw") throw new Error("network unavailable");
    return this.verifyResult;
  }
}

export interface FakeHostOptions {
  serverId?: string;
  label?: string;
  title?: string | null;
  archivedAt?: string | null;
  exists?: boolean;
}

export function fakeHost(options: FakeHostOptions = {}): PaseoHostAdapter {
  const {
    serverId = HOST_ID,
    label = "Workstation",
    title = "Fix the build",
    archivedAt = null,
    exists = true,
  } = options;
  return {
    async listHosts() {
      return { self: { serverId, label } };
    },
    async refreshAgent(agentId) {
      if (!exists) return null;
      return { agent: { id: agentId, title, archivedAt } };
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

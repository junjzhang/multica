import { z } from "zod";
import { redactSecrets } from "../shared/redaction";
import { isUuid } from "../shared/session";

export interface ApiCredential {
  serverUrl: string;
  workspaceId: string;
  token: string;
}

export interface RegisterExternalAgentInput {
  name: string;
  hostId: string;
  sessionId: string;
  displayName: string;
  url: string;
  daemonId: string;
}

export interface ExternalAgentRegistration {
  id: string;
  name: string;
}

export interface IssuedToken {
  id: string;
  token: string;
  expiresAt: string;
}

export interface MulticaApi {
  registerExternalAgent(
    credential: ApiCredential,
    input: RegisterExternalAgentInput,
  ): Promise<ExternalAgentRegistration>;
  createExternalAgentToken(
    credential: ApiCredential,
    agentId: string,
    expiresAt: string,
  ): Promise<IssuedToken>;
  revokeExternalAgentToken(credential: ApiCredential, agentId: string, tokenId: string): Promise<void>;
  /** A live external credential for this exact agent is the only "valid" answer. */
  verifyExternalAgentToken(credential: ApiCredential, agentId: string): Promise<"valid" | "invalid">;
}

export class MulticaApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = "MulticaApiError";
  }
}

const agentResponseSchema = z.object({
  id: z.string(),
  name: z.string().optional(),
});

const tokenResponseSchema = z.object({
  id: z.string(),
  token: z.string(),
  expires_at: z.string(),
});

const MAX_ERROR_BODY = 500;

export function createMulticaApi(options: {
  fetchImpl?: typeof fetch;
  timeoutMs?: number;
} = {}): MulticaApi {
  const fetchImpl = options.fetchImpl ?? fetch;
  const timeoutMs = options.timeoutMs ?? 30_000;

  async function request(
    credential: ApiCredential,
    method: "GET" | "POST" | "DELETE",
    path: string,
    token: string,
    body?: unknown,
  ): Promise<string> {
    const headers: Record<string, string> = {
      Authorization: `Bearer ${token}`,
      "X-Workspace-ID": credential.workspaceId,
      "X-Client-Platform": "paseo-plugin",
      "X-Client-Version": "0.1.0",
    };
    if (body !== undefined) headers["Content-Type"] = "application/json";

    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), timeoutMs);
    try {
      let response: Response;
      let text: string;
      try {
        response = await fetchImpl(`${credential.serverUrl}${path}`, {
          method,
          headers,
          body: body === undefined ? undefined : JSON.stringify(body),
          signal: controller.signal,
        });
        text = await response.text();
      } catch (error) {
        throw new MulticaApiError(
          0,
          `Could not reach the Multica server (${method} ${path}): ${describe(error)}`,
        );
      }
      if (response.status >= 400) {
        const detail = redactSecrets(text.slice(0, MAX_ERROR_BODY)).trim();
        throw new MulticaApiError(
          response.status,
          `Multica API ${method} ${path} returned ${response.status}${detail ? `: ${detail}` : ""}`,
        );
      }
      return text;
    } finally {
      clearTimeout(timer);
    }
  }

  return {
    async registerExternalAgent(credential, input) {
      const text = await request(credential, "POST", "/api/agents/external", credential.token, {
        name: input.name,
        external_session: {
          provider: "paseo",
          host_id: input.hostId,
          session_id: input.sessionId,
          display_name: input.displayName,
          url: input.url,
          daemon_id: input.daemonId,
        },
      });
      const parsed = agentResponseSchema.safeParse(parseJson(text));
      if (!parsed.success || !isUuid(parsed.data.id)) {
        throw new MulticaApiError(0, "Multica did not return a valid external agent registration.");
      }
      return { id: parsed.data.id, name: parsed.data.name ?? input.name };
    },

    async createExternalAgentToken(credential, agentId, expiresAt) {
      const text = await request(
        credential,
        "POST",
        `/api/agents/${encodeURIComponent(agentId)}/tokens`,
        credential.token,
        { expires_at: expiresAt },
      );
      const parsed = tokenResponseSchema.safeParse(parseJson(text));
      if (!parsed.success || !isUuid(parsed.data.id) || !parsed.data.token.startsWith("mea_")) {
        throw new MulticaApiError(0, "Multica did not return a valid external agent credential.");
      }
      return { id: parsed.data.id, token: parsed.data.token, expiresAt: parsed.data.expires_at };
    },

    async revokeExternalAgentToken(credential, agentId, tokenId) {
      await request(
        credential,
        "DELETE",
        `/api/agents/${encodeURIComponent(agentId)}/tokens/${encodeURIComponent(tokenId)}`,
        credential.token,
      );
    },

    async verifyExternalAgentToken(credential, agentId) {
      try {
        await request(
          credential,
          "GET",
          `/api/agents/${encodeURIComponent(agentId)}/mailbox?limit=1`,
          credential.token,
        );
        return "valid";
      } catch (error) {
        if (
          error instanceof MulticaApiError &&
          (error.status === 401 || error.status === 403 || error.status === 404)
        ) {
          return "invalid";
        }
        throw error;
      }
    },
  };
}

function parseJson(text: string): unknown {
  try {
    return JSON.parse(text);
  } catch {
    return null;
  }
}

function describe(error: unknown): string {
  if (error instanceof Error) return redactSecrets(error.message);
  return redactSecrets(String(error));
}

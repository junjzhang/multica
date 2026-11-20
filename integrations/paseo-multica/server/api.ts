import { z } from "zod";
import { redactSecrets } from "../shared/redaction";
import { isUuid } from "../shared/session";
import { sessionTasksWireSchema, type SessionTasksWire } from "../shared/tasks";

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
  /**
   * Ask the server to attach a new session to the host template instead of
   * minting one agent per session. Servers that have not opened template
   * writes reject it; the caller falls back to legacy registration.
   */
  template?: boolean;
  /**
   * Host template display name. Kept separate from name so a legacy server
   * (which ignores this field) still mints a session-scoped agent name.
   */
  templateName?: string;
}

export interface ExternalAgentRegistration {
  id: string;
  name: string;
  /** Registered session UUID the issued credential will act for. */
  sessionId: string;
  templateId?: string;
}

export interface IssuedToken {
  id: string;
  token: string;
  expiresAt: string;
}

export interface MulticaApi {
  findPaseoSession(credential: ApiCredential, hostId: string, sessionId: string): Promise<RegisteredPaseoSession | null>;
  registerExternalAgent(
    credential: ApiCredential,
    input: RegisterExternalAgentInput,
  ): Promise<ExternalAgentRegistration>;
  createExternalAgentToken(
    credential: ApiCredential,
    agentId: string,
    externalSessionId: string,
    expiresAt: string,
  ): Promise<IssuedToken>;
  revokeExternalAgentToken(credential: ApiCredential, agentId: string, tokenId: string): Promise<void>;
  /** A live external credential for this exact agent is the only "valid" answer. */
  verifyExternalAgentToken(credential: ApiCredential, agentId: string): Promise<"valid" | "invalid">;
  /** Read-only "this session's tasks" projection; scoped by the credential. */
  listSessionTasks(credential: ApiCredential, agentId: string, sessionId?: string): Promise<SessionTasksWire>;
  /** A read-only GET of an existing Multica endpoint; the caller validates the body. */
  get(credential: ApiCredential, path: string): Promise<unknown>;
  /** `PUT /api/issues/{id}` with only `status`. */
  updateIssueStatus(credential: ApiCredential, issueId: string, status: string): Promise<void>;
  /** `POST /api/issues/{id}/rerun` with no body: the server reruns for the current assignee. */
  rerunIssue(credential: ApiCredential, issueId: string): Promise<void>;
}

export class MulticaApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
    /** Machine-readable error code from the response body, when present. */
    readonly code?: string,
    /** The server's own redacted explanation (`error` in the body), when present. */
    readonly reason?: string,
  ) {
    super(message);
    this.name = "MulticaApiError";
  }
}

const agentResponseSchema = z.object({
  id: z.string(),
  name: z.string().optional(),
  external_session: z.object({ id: z.string() }).nullish(),
  external_template: z.object({ id: z.string() }).nullish(),
});

const registeredPaseoSessionSchema = z.object({ agent_id: z.uuid(), session_id: z.uuid(), daemon_id: z.string().min(1) });
type RegisteredPaseoSession = z.infer<typeof registeredPaseoSessionSchema>;

const tokenResponseSchema = z.object({
  id: z.string(),
  token: z.string(),
  expires_at: z.string(),
  external_session_id: z.string().optional(),
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
    method: "GET" | "POST" | "PUT" | "DELETE",
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
        const detail = redactSecrets(text.replaceAll(token, "[redacted]").slice(0, MAX_ERROR_BODY)).trim();
        const body = parseJson(text) as { code?: unknown; error?: unknown } | null;
        const code = typeof body?.code === "string" && body.code !== "" ? body.code : undefined;
        const reason = typeof body?.error === "string" ? redactSecrets(body.error.replaceAll(token, "[redacted]").slice(0, MAX_ERROR_BODY)).trim() : "";
        throw new MulticaApiError(
          response.status,
          `Multica API ${method} ${path} returned ${response.status}${detail ? `: ${detail}` : ""}`,
          code,
          reason || undefined,
        );
      }
      return text;
    } finally {
      clearTimeout(timer);
    }
  }

  return {
    async get(credential, path) {
      if (!path.startsWith("/api/")) throw new Error("Invalid Multica API path");
      return parseJson(await request(credential, "GET", path, credential.token));
    },
    async updateIssueStatus(credential, issueId, status) {
      await request(credential, "PUT", `/api/issues/${encodeURIComponent(issueId)}`, credential.token, { status });
    },
    async rerunIssue(credential, issueId) {
      await request(credential, "POST", `/api/issues/${encodeURIComponent(issueId)}/rerun`, credential.token);
    },
    async findPaseoSession(credential, hostId, sessionId) {
      const query = new URLSearchParams({ host_id: hostId, session_id: sessionId });
      const text = await request(credential, "GET", `/api/agents/external/session?${query}`, credential.token);
      const parsed = registeredPaseoSessionSchema.nullable().safeParse(text.trim() === "null" ? null : parseJson(text) ?? undefined);
      if (!parsed.success) throw new MulticaApiError(0, "Multica did not return a valid Paseo session.");
      return parsed.data;
    },
    async registerExternalAgent(credential, input) {
      const text = await request(credential, "POST", "/api/agents/external", credential.token, {
        name: input.name,
        template: input.template === true ? true : undefined,
        template_name: input.templateName,
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
      const sessionId = parsed.success ? parsed.data.external_session?.id : undefined;
      if (!parsed.success || !isUuid(parsed.data.id) || !sessionId || !isUuid(sessionId)) {
        throw new MulticaApiError(0, "Multica did not return a valid external agent registration.");
      }
      return {
        id: parsed.data.id,
        name: parsed.data.name ?? input.name,
        sessionId,
        ...(parsed.data.external_template?.id ? { templateId: parsed.data.external_template.id } : {}),
      };
    },

    async createExternalAgentToken(credential, agentId, externalSessionId, expiresAt) {
      const text = await request(
        credential,
        "POST",
        `/api/agents/${encodeURIComponent(agentId)}/tokens`,
        credential.token,
        { expires_at: expiresAt, external_session_id: externalSessionId },
      );
      const parsed = tokenResponseSchema.safeParse(parseJson(text));
      if (!parsed.success || !isUuid(parsed.data.id) || !parsed.data.token.startsWith("mea_")) {
        throw new MulticaApiError(0, "Multica did not return a valid external agent credential.");
      }
      // A credential for another session would defeat the session boundary the
      // plugin exists to establish.
      if (parsed.data.external_session_id && parsed.data.external_session_id !== externalSessionId) {
        throw new MulticaApiError(0, "Multica bound the new credential to a different session.");
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

    async listSessionTasks(credential, agentId, sessionId) {
      const query = sessionId ? `?session_id=${encodeURIComponent(sessionId)}` : "";
      const text = await request(
        credential,
        "GET",
        `/api/agents/${encodeURIComponent(agentId)}/session-tasks${query}`,
        credential.token,
      );
      const parsed = sessionTasksWireSchema.safeParse(parseJson(text));
      if (!parsed.success) {
        throw new MulticaApiError(0, "Multica did not return a valid session task summary.");
      }
      return parsed.data;
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

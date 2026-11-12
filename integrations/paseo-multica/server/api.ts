import { z } from "zod";
import { redactSecrets } from "../shared/redaction";
import { isUuid } from "../shared/session";
import { sessionTasksWireSchema, type SessionTasksWire } from "../shared/tasks";
import { detailWireSchemas, type DetailSection } from "../shared/task-details";

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
  listSessionTasks(credential: ApiCredential, agentId: string): Promise<SessionTasksWire>;
  taskDetail(credential: ApiCredential, issueId: string, section: DetailSection): Promise<unknown>;
}

export class MulticaApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
    /** Machine-readable error code from the response body, when present. */
    readonly code?: string,
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
        const body = parseJson(text) as { code?: unknown } | null;
        const code = typeof body?.code === "string" && body.code !== "" ? body.code : undefined;
        throw new MulticaApiError(
          response.status,
          `Multica API ${method} ${path} returned ${response.status}${detail ? `: ${detail}` : ""}`,
          code,
        );
      }
      return text;
    } finally {
      clearTimeout(timer);
    }
  }

  async function actorName(credential: ApiCredential, type: string, id: string): Promise<string> {
    if (!isUuid(id)) return id;
    try {
      if (type === "agent") {
        const data = z.object({ name: z.string() }).parse(parseJson(await request(credential, "GET", `/api/agents/${encodeURIComponent(id)}`, credential.token)));
        return data.name || id;
      }
      if (type === "user" || type === "member") {
        const members = z.array(z.object({ id: z.string(), user_id: z.string(), name: z.string() })).parse(parseJson(await request(credential, "GET", `/api/workspaces/${encodeURIComponent(credential.workspaceId)}/members`, credential.token)));
        return members.find((member) => member.id === id || member.user_id === id)?.name || id;
      }
    } catch { /* Names are optional; retain the stable identity. */ }
    return id;
  }

  return {
    async taskDetail(credential, issueId, section) {
      if (!isUuid(issueId)) throw new Error("Invalid issue ID");
      const suffix = { contract: "contract", runs: "task-runs", comments: "comments?recent=2&summary=true", prs: "pull-requests", children: "children" }[section];
      const raw = parseJson(await request(credential, "GET", `/api/issues/${encodeURIComponent(issueId)}/${suffix}`, credential.token));
      const parsed = detailWireSchemas[section].safeParse(raw);
      if (!parsed.success) throw new MulticaApiError(0, `Invalid ${section} response from Multica.`);
      if (section === "runs") {
        const runs = detailWireSchemas.runs.parse(parsed.data);
        const selected = runs.find((run) => run.status === "running") ?? runs[0];
        if (selected) selected.agent_name = await actorName(credential, "agent", selected.agent_id);
        return runs;
      }
      if (section === "comments") {
        const comments = detailWireSchemas.comments.parse(parsed.data).sort((a, b) => a.created_at.localeCompare(b.created_at)).slice(-2);
        await Promise.all(comments.map(async (comment) => { comment.author_name = await actorName(credential, comment.author_type, comment.author_id); }));
        return comments;
      }
      if (section === "children") {
        const children = detailWireSchemas.children.parse(parsed.data);
        await Promise.all(children.issues.map(async (child) => { if (child.assignee_id && child.assignee_type) child.assignee_name = await actorName(credential, child.assignee_type, child.assignee_id); }));
        return children;
      }
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

    async listSessionTasks(credential, agentId) {
      const text = await request(
        credential,
        "GET",
        `/api/agents/${encodeURIComponent(agentId)}/session-tasks`,
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

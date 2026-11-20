import { randomUUID } from "node:crypto";
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import { afterEach, describe, expect, it } from "vitest";
import { createMulticaApi, type ApiCredential } from "../server/api";
import { AGENT_A, AGENT_B, EXTERNAL_TOKEN, OWNER_TOKEN, SERVER_URL, WORKSPACE_ID } from "./fixtures";

interface Recorded {
  method: string;
  url: string;
  headers: IncomingMessage["headers"];
  body: string;
}

const SESSION_REGISTERED = randomUUID();
const servers: Array<() => Promise<void>> = [];

afterEach(async () => {
  await Promise.all(servers.splice(0).map((close) => close()));
});

async function startServer(
  handler: (request: Recorded, response: ServerResponse) => void,
): Promise<{ credential: ApiCredential; calls: Recorded[] }> {
  const calls: Recorded[] = [];
  const server = createServer((request, response) => {
    let body = "";
    request.on("data", (chunk) => {
      body += String(chunk);
    });
    request.on("end", () => {
      const recorded = {
        method: request.method ?? "",
        url: request.url ?? "",
        headers: request.headers,
        body,
      };
      calls.push(recorded);
      handler(recorded, response);
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address();
  if (!address || typeof address === "string") throw new Error("server did not bind a port");
  servers.push(
    () =>
      new Promise<void>((resolve, reject) => {
        server.close((error) => (error ? reject(error) : resolve()));
      }),
  );
  return {
    credential: {
      serverUrl: `http://127.0.0.1:${address.port}`,
      workspaceId: WORKSPACE_ID,
      token: OWNER_TOKEN,
    },
    calls,
  };
}

describe("Multica API client", () => {
  it("registers an external session with the owner credential", async () => {
    const { credential, calls } = await startServer((_request, response) => {
      response.writeHead(201, { "Content-Type": "application/json" });
      response.end(JSON.stringify({ id: AGENT_A, name: "Workstation · Fix the build", execution_kind: "external", external_session: { id: randomUUID() }, external_template: null }));
    });
    const api = createMulticaApi();

    const result = await api.registerExternalAgent(credential, {
      name: "Workstation · Fix the build",
      template: true,
      templateName: "Paseo · Workstation",
      hostId: "srv_workstation",
      sessionId: "paseo-session-a",
      displayName: "Fix the build",
      url: "paseo://h/srv_workstation/agent/paseo-session-a",
      daemonId: "daemon_workstation",
    });

    expect(result.id).toBe(AGENT_A);
    expect(result.sessionId).toMatch(/^[0-9a-f-]{36}$/);
    expect(calls[0]?.method).toBe("POST");
    expect(calls[0]?.url).toBe("/api/agents/external");
    expect(calls[0]?.headers.authorization).toBe(`Bearer ${OWNER_TOKEN}`);
    expect(calls[0]?.headers["x-workspace-id"]).toBe(WORKSPACE_ID);
    expect(JSON.parse(calls[0]?.body ?? "{}")).toMatchObject({
      name: "Workstation · Fix the build",
      template: true,
      template_name: "Paseo · Workstation",
      external_session: {
        provider: "paseo",
        host_id: "srv_workstation",
        session_id: "paseo-session-a",
        display_name: "Fix the build",
        url: "paseo://h/srv_workstation/agent/paseo-session-a",
        daemon_id: "daemon_workstation",
      },
    });
  });

  it("moves a status with PUT and reruns the given run with POST, surfacing the server's reason", async () => {
    const issueId = randomUUID();
    const { credential, calls } = await startServer((request, response) => {
      if (request.method === "PUT" && request.body.includes("cancelled")) {
        response.writeHead(409, { "Content-Type": "application/json" });
        response.end(JSON.stringify({ error: `accept the current contract first ${OWNER_TOKEN}` }));
        return;
      }
      response.writeHead(200, { "Content-Type": "application/json" });
      response.end("{}");
    });
    const api = createMulticaApi();
    await api.updateIssueStatus(credential, issueId, "blocked");
    const taskId = randomUUID();
    await api.rerunIssue(credential, issueId, taskId);
    expect(calls.map((call) => [call.method, call.url, call.body, call.headers.authorization])).toEqual([
      ["PUT", `/api/issues/${issueId}`, JSON.stringify({ status: "blocked" }), `Bearer ${OWNER_TOKEN}`],
      ["POST", `/api/issues/${issueId}/rerun`, JSON.stringify({ task_id: taskId }), `Bearer ${OWNER_TOKEN}`],
    ]);
    const refused = await api.updateIssueStatus(credential, issueId, "cancelled").catch((error: unknown) => error);
    expect(refused).toMatchObject({ status: 409, reason: "accept the current contract first [redacted]" });
    expect(String((refused as Error).message)).not.toContain(OWNER_TOKEN);
  });

  it("mints and revokes a credential through the documented endpoints", async () => {
    const tokenId = randomUUID();
    const { credential, calls } = await startServer((request, response) => {
      if (request.method === "POST") {
        response.writeHead(201, { "Content-Type": "application/json" });
        response.end(
          JSON.stringify({ id: tokenId, token: EXTERNAL_TOKEN, expires_at: "2027-01-01T00:00:00Z" }),
        );
        return;
      }
      response.writeHead(204);
      response.end();
    });
    const api = createMulticaApi();

    const issued = await api.createExternalAgentToken(credential, AGENT_A, SESSION_REGISTERED, "2027-01-01T00:00:00Z");
    expect(issued).toEqual({ id: tokenId, token: EXTERNAL_TOKEN, expiresAt: "2027-01-01T00:00:00Z" });
    expect(calls[0]?.url).toBe(`/api/agents/${AGENT_A}/tokens`);
    expect(JSON.parse(calls[0]?.body ?? "{}")).toEqual({ expires_at: "2027-01-01T00:00:00Z", external_session_id: SESSION_REGISTERED });

    await api.revokeExternalAgentToken(credential, AGENT_A, tokenId);
    expect(calls[1]?.method).toBe("DELETE");
    expect(calls[1]?.url).toBe(`/api/agents/${AGENT_A}/tokens/${tokenId}`);
  });

  it("verifies a credential against the agent's own mailbox", async () => {
    const { credential, calls } = await startServer((_request, response) => {
      response.writeHead(200, { "Content-Type": "application/json" });
      response.end(JSON.stringify({ messages: [], next_cursor: null }));
    });
    const api = createMulticaApi();
    const external = { ...credential, token: EXTERNAL_TOKEN };

    await expect(api.verifyExternalAgentToken(external, AGENT_A)).resolves.toBe("valid");
    expect(calls[0]?.url).toBe(`/api/agents/${AGENT_A}/mailbox?limit=1`);
    expect(calls[0]?.headers.authorization).toBe(`Bearer ${EXTERNAL_TOKEN}`);
  });

  it("checks the credential subject even when another agent is visible to its owner", async () => {
    const tokenA = EXTERNAL_TOKEN;
    const tokenB = "mea_external_token_b_0123456789";
    const { credential, calls } = await startServer((request, response) => {
      const url = request.url ?? "";
      if (url.endsWith("/mailbox?limit=1")) {
        const pathAgent = url.split("/")[3] ?? "";
        const presented = (request.headers.authorization ?? "").replace(/^Bearer /, "");
        const subject = presented === tokenA ? AGENT_A : presented === tokenB ? AGENT_B : "";
        const allowed = pathAgent === subject;
        response.writeHead(allowed ? 200 : 403, { "Content-Type": "application/json" });
        response.end(allowed ? JSON.stringify({ messages: [], next_cursor: null }) : '{"error":"forbidden"}');
        return;
      }
      // The actor-binding probe: a same-owner GET of agent B accepts agent A's
      // credential. Verification must not rely on that visibility.
      response.writeHead(200, { "Content-Type": "application/json" });
      response.end(JSON.stringify({ id: AGENT_B }));
    });
    const api = createMulticaApi();

    const visible = await fetch(`${credential.serverUrl}/api/agents/${AGENT_B}`, {
      headers: { Authorization: `Bearer ${tokenA}` },
    });
    expect(visible.status).toBe(200);
    expect(await visible.json()).toEqual({ id: AGENT_B });

    await expect(api.verifyExternalAgentToken({ ...credential, token: tokenA }, AGENT_B)).resolves.toBe(
      "invalid",
    );
    expect(calls.at(-1)?.url).toBe(`/api/agents/${AGENT_B}/mailbox?limit=1`);

    await expect(api.verifyExternalAgentToken({ ...credential, token: tokenB }, AGENT_B)).resolves.toBe(
      "valid",
    );
  });

  it("treats rejected and unknown credentials as invalid but propagates server failures", async () => {
    const { credential, calls } = await startServer((request, response) => {
      const status = calls.length === 1 ? 401 : 500;
      response.writeHead(status, { "Content-Type": "text/plain" });
      response.end(status === 401 ? "invalid token" : `upstream failed: ${EXTERNAL_TOKEN}`);
    });
    const api = createMulticaApi();
    const external = { ...credential, token: EXTERNAL_TOKEN };

    await expect(api.verifyExternalAgentToken(external, AGENT_A)).resolves.toBe("invalid");
    let message = "";
    try {
      await api.verifyExternalAgentToken(external, AGENT_A);
    } catch (error) {
      message = String(error);
    }
    expect(message).toMatch(/returned 500/);
    expect(message).toContain("[redacted]");
    expect(message).not.toContain(EXTERNAL_TOKEN);
  });

  it("rejects a response that is not a real external credential", async () => {
    const { credential } = await startServer((_request, response) => {
      response.writeHead(201, { "Content-Type": "application/json" });
      response.end(JSON.stringify({ id: randomUUID(), token: OWNER_TOKEN, expires_at: "2027-01-01T00:00:00Z" }));
    });
    const api = createMulticaApi();

    await expect(api.createExternalAgentToken(credential, AGENT_A, SESSION_REGISTERED, "2027-01-01T00:00:00Z")).rejects.toThrow(
      /valid external agent credential/,
    );
  });

  it("keeps API errors free of the credential and of the configured server URL path", async () => {
    const { credential } = await startServer((_request, response) => {
      response.writeHead(503, { "Content-Type": "text/plain" });
      response.end(`busy ${OWNER_TOKEN}`);
    });
    const api = createMulticaApi();
    let message = "";
    try {
      await api.registerExternalAgent(credential, {
        name: "n",
        hostId: "h",
        sessionId: "s",
        displayName: "d",
        url: "paseo://h/h/agent/s",
        daemonId: "d",
      });
    } catch (error) {
      message = String(error);
    }
    expect(message).toMatch(/returned 503/);
    expect(message).toContain("[redacted]");
    expect(message).not.toContain(OWNER_TOKEN);
    expect(message).not.toContain(SERVER_URL);
  });

  it("reads the session task summary with the external credential", async () => {
    const { credential, calls } = await startServer((_request, response) => {
      response.writeHead(200, { "Content-Type": "application/json" });
      response.end(
        JSON.stringify({
          agent_id: AGENT_A,
          session_id: SESSION_REGISTERED,
          workspace_id: WORKSPACE_ID,
          owned: [],
          accepting: [],
          returning: [],
          truncated: false,
        }),
      );
    });
    const api = createMulticaApi();

    const wire = await api.listSessionTasks({ ...credential, token: EXTERNAL_TOKEN }, AGENT_A);

    expect(wire.agent_id).toBe(AGENT_A);
    expect(wire.session_id).toBe(SESSION_REGISTERED);
    expect(calls[0]?.method).toBe("GET");
    expect(calls[0]?.url).toBe(`/api/agents/${AGENT_A}/session-tasks`);
    expect(calls[0]?.headers.authorization).toBe(`Bearer ${EXTERNAL_TOKEN}`);
    expect(calls[0]?.headers["x-workspace-id"]).toBe(WORKSPACE_ID);
  });

  it("rejects a session task summary that does not match the wire shape", async () => {
    const { credential } = await startServer((_request, response) => {
      response.writeHead(200, { "Content-Type": "application/json" });
      response.end(JSON.stringify({ agent_id: AGENT_A }));
    });
    const api = createMulticaApi();

    await expect(
      api.listSessionTasks({ ...credential, token: EXTERNAL_TOKEN }, AGENT_A),
    ).rejects.toThrow(/valid session task summary/);
  });
});

it("reads an API path with the session credential and refuses other paths", async () => {
  const { credential, calls } = await startServer((_request, response) => {
    response.writeHead(200, { "Content-Type": "application/json" });
    response.end(JSON.stringify({ issues: [] }));
  });
  const api = createMulticaApi();
  const sessionCredential = { ...credential, token: EXTERNAL_TOKEN };
  expect(await api.get(sessionCredential, "/api/issues/children?parent_ids=a")).toEqual({ issues: [] });
  expect(calls[0]?.url).toBe("/api/issues/children?parent_ids=a");
  expect(calls[0]?.headers.authorization).toBe(`Bearer ${EXTERNAL_TOKEN}`);
  expect(calls[0]?.headers["x-workspace-id"]).toBe(WORKSPACE_ID);
  await expect(api.get(sessionCredential, "https://elsewhere.example/api")).rejects.toThrow("Invalid Multica API path");
  expect(calls).toHaveLength(1);
});

import { test, expect } from "@playwright/test";
import { randomUUID } from "node:crypto";
import { createTestApi, loginAsDefault } from "./helpers";

const base = process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:8080";

test("explicit and legacy handoffs keep their distinct responsibilities", async ({ page }) => {
  test.setTimeout(120_000);
  const api = await createTestApi();
  const workspace = await api.ensureWorkspace();
  const suffix = randomUUID();
  // A daemon runtime is transport infrastructure; runtime registration requires a live daemon.
  const daemon = `handoff-${suffix}`;
  const runtime = await api.seedRuntime("Handoff test runtime", daemon);
  const human = api.getToken()!;
  let agent = "", managed = "", squad = "", root = "";
  const request = async (path: string, method = "GET", body?: unknown, token = human) => {
    return fetch(`${base}${path}`, { method, headers: { Authorization: `Bearer ${token}`, "X-Workspace-ID": workspace.id, "Content-Type": "application/json" }, body: body === undefined ? undefined : JSON.stringify(body) });
  };
  const json = async (path: string, method = "GET", body?: unknown, token = human) => {
    const response = await request(path, method, body, token);
    expect(response.ok, `${path}: ${response.status} ${await response.clone().text()}`).toBeTruthy();
    return response.json();
  };
  try {
    const host = `host-${suffix}`;
    const register = (session: string, display: string) => json("/api/agents/external", "POST", {
      name: `Handoff ${suffix}`, template: true,
      external_session: { provider: "paseo", host_id: host, session_id: session, display_name: display, daemon_id: daemon },
    });
    const first = await register(`first-${suffix}`, "Handoff old session");
    const second = await register(`second-${suffix}`, "Handoff new session");
    agent = first.id;
    expect(second.id).toBe(agent);
    const a = first.external_session.id, b = second.external_session.id;
    const tokenA = (await json(`/api/agents/${agent}/tokens`, "POST", { expires_at: new Date(Date.now() + 3600_000).toISOString(), external_session_id: a })).token;
    const managedAgent = await json("/api/agents", "POST", { name: `Default ${suffix}`, runtime_id: runtime, visibility: "workspace" });
    managed = managedAgent.id;
    const team = await json("/api/squads", "POST", { name: `Handoff squad ${suffix}`, leader_id: managed });
    squad = team.id;
    await json(`/api/squads/${squad}/members`, "POST", { member_type: "agent", member_id: agent });

    const parent = await json("/api/issues", "POST", { title: `Handoff root ${suffix}`, assignee_type: "squad", assignee_id: squad }, tokenA);
    root = parent.id;
    const child = await api.createIssue(`Handoff child ${suffix}`, { status: "backlog", parent_issue_id: root, leadership_root_issue_id: root, assignee_type: "agent", assignee_id: agent, recipient_session_id: a });
    expect(child.id).toBeTruthy();
    const slug = await loginAsDefault(page);
    await page.goto(`/${slug}/issues/${child.identifier}`);
    await page.getByRole("button", { name: "Hand off", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "Hand off task leader" });
    await dialog.getByText(`Handoff ${suffix}`, { exact: true }).click();
    await dialog.getByText("Handoff new session", { exact: true }).click();
    await dialog.getByLabel("Reason").fill("Recover the task leader");
    const dispatched = page.waitForResponse((r) => r.request().method() === "POST" && r.url().endsWith(`/api/issues/${child.id}/task-leader/handoff`));
    await dialog.getByRole("button", { name: "Hand off", exact: true }).click();
    const dispatchedResponse = await dispatched;
    expect(dispatchedResponse.ok(), await dispatchedResponse.text()).toBeTruthy();
    expect((await json(`/api/issues/${root}`)).leader_external_session_id).toBe(b);

    for (const path of ["executor/handoff", "leadership/handoff"]) {
      const issue = await api.createIssue(`Handoff executor ${path} ${suffix}`, { status: "backlog", assignee_type: "agent", assignee_id: agent, recipient_session_id: a });
      const contract = { action: "revise", expected_sequence: 0, client_request_id: randomUUID(), goal: "Recover executor", scope: "Test", requirements: ["Evidence"], authorization_note: "Test only" };
      await json(`/api/issues/${issue.id}/contract`, "POST", contract);
      const current = await json(`/api/issues/${issue.id}`);
      const body = { client_request_id: randomUUID(), expected_issue_revision: current.revision, previous_agent_id: agent, previous_session_id: a, agent_id: agent, session_id: b, reason: "Recover executor" };
      const response = await request(`/api/issues/${issue.id}/${path}`, "POST", body);
      expect(response.ok, await response.text()).toBeTruthy();
      if (path === "leadership/handoff") expect(response.headers.get("Deprecation")).toBe("true");
      expect((await json(`/api/issues/${issue.id}/contract`)).stale).toBe(true);
      await json(`/api/issues/${issue.id}/contract`, "POST", { ...contract, expected_sequence: 1, client_request_id: randomUUID() });
      const staleWrite = await request(`/api/issues/${issue.id}/contract`, "POST", { action: "submit", version: 2, expected_sequence: 2, client_request_id: randomUUID(), criteria: [0], evidence: "old session" }, tokenA);
      expect(staleWrite.status).toBe(409);
    }
  } finally {
    await api.cleanup();
    if (root) await api.deleteIssue(root);
    if (squad) await request(`/api/squads/${squad}`, "DELETE");
    if (agent) await request(`/api/agents/${agent}`, "DELETE");
    if (managed) await request(`/api/agents/${managed}`, "DELETE");
    await request(`/api/runtimes/${runtime}`, "DELETE");
  }
});

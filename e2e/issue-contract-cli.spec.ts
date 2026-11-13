import { test, expect } from "@playwright/test";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { writeFile } from "node:fs/promises";
import { randomUUID } from "node:crypto";
import { createTestApi } from "./helpers";

test("agent CLI reads and submits the same contract with a fixture task token", async ({}, info) => {
  const binary = process.env.MULTICA_CONTRACT_TEST_CLI;
  test.skip(
    !binary,
    "Build the local Multica CLI and set MULTICA_CONTRACT_TEST_CLI",
  );
  const api = await createTestApi();
  try {
    const issue = await api.createIssue("CLI agreement proof");
    const actor = await api.contractAgent(issue.id);
    await api.applyContract(issue.id, {
      action: "revise",
      client_request_id: randomUUID(),
      expected_sequence: 0,
      goal: "CLI delivery",
      scope: "Isolated fixture",
      authorization_note: "Submit evidence only",
      requirements: ["CLI and API agree"],
    });
    const env = Object.fromEntries(
      Object.entries(process.env).filter(
        ([key]) => !key.startsWith("MULTICA_") && !key.startsWith("PASEO_"),
      ),
    );
    Object.assign(env, {
      MULTICA_SERVER_URL: actor.serverUrl,
      MULTICA_WORKSPACE_ID: actor.workspaceId,
      MULTICA_TOKEN: actor.token,
      MULTICA_AGENT_ID: actor.agentId,
      MULTICA_TASK_ID: actor.taskId,
    });
    const cli = async (args: string[]) => {
      const { stdout } = await promisify(execFile)(binary!, args, {
        cwd: process.cwd(),
        env,
        timeout: 15_000,
      });
      return JSON.parse(stdout);
    };
    const initial = await cli([
      "issue",
      "contract",
      "get",
      issue.id,
      "--output",
      "json",
    ]);
    expect(initial.contract.versions[0].goal).toBe("CLI delivery");
    expect(initial.can_submit).toBe(true);
    const path = info.outputPath("delivery-request.json");
    const requestId = randomUUID();
    await writeFile(
      path,
      JSON.stringify({
        action: "submit",
        client_request_id: requestId,
        expected_sequence: 1,
        version: 1,
        criteria: [0],
        evidence: "Fixture CLI submission",
      }),
    );
    const submitted = await cli([
      "issue",
      "contract",
      "apply",
      issue.id,
      "--request-file",
      path,
    ]);
    expect(submitted.contract.sequence).toBe(2);
    const replay = await cli([
      "issue",
      "contract",
      "apply",
      issue.id,
      "--request-file",
      path,
    ]);
    expect(replay.contract.deliveries).toHaveLength(1);
    const accepted = await api.applyContract(issue.id, {
      action: "accept",
      client_request_id: randomUUID(),
      expected_sequence: 2,
      version: 1,
      delivery_id: requestId,
      criteria: [0],
    });
    expect(accepted.accepted).toEqual([0]);
    expect((await api.getIssue(issue.id)).status).toBe("done");
  } finally {
    await api.cleanup();
  }
});

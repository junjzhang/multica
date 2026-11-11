// @vitest-environment node
import { describe, expect, it, vi } from "vitest";
import { ApiClient, setApiInstance } from "../api";
import { QueryClient } from "@tanstack/react-query";
import type { Agent, Workspace } from "../types";
import { agentListOptions, workspaceBySlugOptions } from "./queries";

function makeWorkspace(slug: string): Workspace {
  return {
    id: `id-${slug}`,
    name: slug,
    slug,
    description: null,
    context: null,
    settings: {},
    repos: [],
    issue_prefix: slug.toUpperCase(),
    avatar_url: null,
    created_at: "",
    updated_at: "",
  };
}

describe("workspaceBySlugOptions", () => {
  const workspaces = [makeWorkspace("acme")];

  it("selects a matching workspace", () => {
    expect(workspaceBySlugOptions("acme").select?.(workspaces)).toEqual(
      workspaces[0],
    );
  });

  it("returns null after an authoritative list omits the slug", () => {
    expect(workspaceBySlugOptions("missing").select?.(workspaces)).toBeNull();
  });
});

describe("agentListOptions", () => {
  it("polls only while projected runtime availability can age offline", () => {
    const options = agentListOptions("ws-1");
    const interval = options.refetchInterval;
    expect(typeof interval).toBe("function");
    if (typeof interval !== "function") return;

    const queryState = (data: Agent[]) =>
      interval({ state: { status: "success", data } } as never);

    expect(
      queryState([{ runtime_availability: "online" } as Agent]),
    ).toBe(30_000);
    expect(
      queryState([{ runtime_availability: "unstable" } as Agent]),
    ).toBe(30_000);
    expect(
      queryState([{ runtime_availability: "offline" } as Agent]),
    ).toBe(false);
    expect(
      queryState([{ runtime_availability: undefined } as Agent]),
    ).toBe(false);
    expect(
      queryState([
        {
          archived_at: "2026-09-04T00:00:00Z",
          runtime_availability: "online",
        } as Agent,
      ]),
    ).toBe(false);
    expect(queryState([])).toBe(false);
  });
});

it("agent directory requests and preserves archived recipients for authorized replay", async () => {
  const archived = { id: "dispatcher", name: "Dispatcher", workspace_id: "workspace", owner_id: "owner", archived_at: "2026-11-03T00:00:00Z" };
  const fetch = vi.fn().mockResolvedValue(new Response(JSON.stringify([archived]), { status: 200 }));
  vi.stubGlobal("fetch", fetch);
  setApiInstance(new ApiClient("https://example.test"));
  const client = new QueryClient();
  try {
    const rows = await client.fetchQuery(agentListOptions("workspace"));
    expect(rows[0]).toMatchObject(archived);
    const url = new URL(fetch.mock.calls[0]![0]);
    expect(url.pathname).toBe("/api/agents");
    expect(url.searchParams.get("include_archived")).toBe("true");
    expect(url.searchParams.get("workspace_id")).toBe("workspace");
  } finally {
    client.clear(); setApiInstance(undefined as unknown as ApiClient); vi.unstubAllGlobals();
  }
});

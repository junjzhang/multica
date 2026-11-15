// @vitest-environment jsdom
import { beforeAll, beforeEach, afterEach, describe, expect, it } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { setCurrentWorkspace } from "../../platform/workspace-storage";
import { ViewStoreProvider, useViewStore } from "./view-store-context";
import {
  ISSUE_SURFACE_VIEW_STORAGE_KEY,
  clearIssueSurfaceViewState,
  getIssueSurfaceViewStateRegistrySnapshot,
  getIssueSurfaceViewStore,
  pruneIssueSurfaceViewStates,
  seedIssueSurfaceViewState,
} from "./surface-view-store";
import { DEFAULT_CARD_PROPERTIES, effectiveShowSubIssues } from "./view-store";

const flush = async () => {
  await new Promise((resolve) => queueMicrotask(() => resolve(null)));
  await new Promise((resolve) => queueMicrotask(() => resolve(null)));
};

beforeAll(() => {
  if (typeof globalThis.localStorage?.clear !== "function") {
    const values = new Map<string, string>();
    const storage: Storage = {
      get length() {
        return values.size;
      },
      clear: () => values.clear(),
      getItem: (key) => values.get(key) ?? null,
      key: (index) => Array.from(values.keys())[index] ?? null,
      removeItem: (key) => {
        values.delete(key);
      },
      setItem: (key, value) => {
        values.set(key, value);
      },
    };
    Object.defineProperty(globalThis, "localStorage", {
      configurable: true,
      value: storage,
    });
    Object.defineProperty(window, "localStorage", {
      configurable: true,
      value: storage,
    });
  }
});

beforeEach(async () => {
  localStorage.clear();
  pruneIssueSurfaceViewStates([]);
  setCurrentWorkspace(null, null);
  await flush();
});

afterEach(async () => {
  cleanup();
  pruneIssueSurfaceViewStates([]);
  setCurrentWorkspace(null, null);
  await flush();
});

describe("issue surface view store registry", () => {
  it("focuses a fresh workspace board on parents without narrowing other surfaces", async () => {
    setCurrentWorkspace("acme", "ws_a");
    await flush();
    const store = getIssueSurfaceViewStore("workspace:all");
    expect(effectiveShowSubIssues(store.getState())).toBe(false);
    expect(effectiveShowSubIssues(store.getState(), "child title")).toBe(true);
    for (const key of ["workspace:agents", "my:assigned", "actor:agent-1", "view:saved"]) {
      expect(effectiveShowSubIssues(getIssueSurfaceViewStore(key).getState())).toBe(true);
    }
    store.getState().toggleAssigneeFilter({ type: "agent", id: "worker" });
    expect(effectiveShowSubIssues(store.getState())).toBe(true);
    store.getState().clearFilters();
    expect(effectiveShowSubIssues(store.getState())).toBe(false);
    store.getState().setViewMode("list");
    expect(effectiveShowSubIssues(store.getState())).toBe(true);
  });

  it("focuses a fresh project board on parents like the workspace board", async () => {
    setCurrentWorkspace("acme", "ws_a");
    await flush();
    const store = getIssueSurfaceViewStore("project:p1");
    expect(store.getState().showSubIssuesExplicit).toBe(false);
    expect(effectiveShowSubIssues(store.getState())).toBe(false);
    expect(effectiveShowSubIssues(store.getState(), "child title")).toBe(true);
    expect(store.getState().cardProperties.project).toBe(false);
    store.getState().toggleAssigneeFilter({ type: "agent", id: "worker" });
    expect(effectiveShowSubIssues(store.getState())).toBe(true);
    store.getState().clearFilters();
    expect(effectiveShowSubIssues(store.getState())).toBe(false);
    store.getState().setViewMode("list");
    expect(effectiveShowSubIssues(store.getState())).toBe(true);
    // The fixed project scope is not a user filter: it must not keep
    // sub-issues visible after an explicit hide.
    store.getState().setShowSubIssues(false);
    expect(effectiveShowSubIssues(store.getState())).toBe(false);
    expect(effectiveShowSubIssues(store.getState(), "child title")).toBe(false);

    // The project's actor tabs keep the workspace actor-tab behavior.
    for (const key of ["project:p1:agents", "project:p1:members"]) {
      const tab = getIssueSurfaceViewStore(key).getState();
      expect(tab.showSubIssues).toBe(true);
      expect(tab.showSubIssuesExplicit).toBe(true);
      expect(tab.cardProperties.project).toBe(false);
    }
  });

  it("keeps an automatic default after unrelated edits and workspace rehydration", async () => {
    setCurrentWorkspace("acme", "ws_a");
    await flush();
    const store = getIssueSurfaceViewStore("workspace:all");
    store.getState().setSortBy("title");
    setCurrentWorkspace("other", "ws_b");
    await flush();
    setCurrentWorkspace("acme", "ws_a");
    await flush();
    expect(store.getState().showSubIssuesExplicit).toBe(false);
    expect(effectiveShowSubIssues(store.getState())).toBe(false);
    expect(effectiveShowSubIssues(store.getState(), "child")).toBe(true);
    store.getState().setShowSubIssues(false);
    expect(effectiveShowSubIssues(store.getState(), "child")).toBe(false);
    setCurrentWorkspace("other", "ws_b");
    await flush();
    setCurrentWorkspace("acme", "ws_a");
    await flush();
    expect(effectiveShowSubIssues(store.getState(), "child")).toBe(false);
    clearIssueSurfaceViewState("workspace:all");
    expect(effectiveShowSubIssues(store.getState(), "child")).toBe(true);
  });

  it.each([true, false])("preserves legacy explicit child visibility %s", async (showSubIssues) => {
    localStorage.setItem(`${ISSUE_SURFACE_VIEW_STORAGE_KEY}:acme`, JSON.stringify({
      version: 1,
      state: { surfaces: { "workspace:all": { state: { showSubIssues }, updatedAt: "2026-01-01" } } },
    }));
    setCurrentWorkspace("acme", "ws_a");
    await flush();
    const state = getIssueSurfaceViewStore("workspace:all").getState();
    expect(state.showSubIssuesExplicit).toBe(true);
    expect(effectiveShowSubIssues(state, "child")).toBe(showSubIssues);
  });

  it("treats saved view child visibility as an explicit setting", async () => {
    setCurrentWorkspace("acme", "ws_a");
    await flush();
    seedIssueSurfaceViewState("view:saved", { showSubIssues: false, showSubIssuesExplicit: false });
    expect(effectiveShowSubIssues(getIssueSurfaceViewStore("view:saved").getState(), "child")).toBe(false);
  });

  it("resets inherited project child visibility once without replaying the v1 defaults", async () => {
    localStorage.setItem(`${ISSUE_SURFACE_VIEW_STORAGE_KEY}:acme`, JSON.stringify({
      version: 1,
      state: {
        surfaces: {
          "project:shown": {
            state: {
              sortBy: "position",
              sortDirection: "asc",
              statusFilters: ["done"],
              showSubIssues: true,
              showSubIssuesExplicit: true,
              cardProperties: { ...DEFAULT_CARD_PROPERTIES, project: true },
            },
            updatedAt: "2026-01-01",
          },
          "project:closed": {
            state: { showSubIssues: false, showSubIssuesExplicit: true },
            updatedAt: "2026-01-01",
          },
          "project:early-closed": {
            state: { showSubIssues: false },
            updatedAt: "2026-01-01",
          },
          "project:tab:agents": {
            state: { showSubIssues: true, showSubIssuesExplicit: true },
            updatedAt: "2026-01-01",
          },
          "workspace:all": {
            state: {
              sortBy: "position",
              sortDirection: "asc",
              showSubIssues: true,
              showSubIssuesExplicit: true,
            },
            updatedAt: "2026-01-01",
          },
          "view:saved": {
            state: { showSubIssues: true, showSubIssuesExplicit: true },
            updatedAt: "2026-01-01",
          },
        },
      },
    }));
    setCurrentWorkspace("acme", "ws_a");
    await flush();

    // The old project default (`true`/explicit, indistinguishable from a
    // manual show) becomes the automatic root-only default.
    const shown = getIssueSurfaceViewStore("project:shown").getState();
    expect(shown.showSubIssues).toBe(false);
    expect(shown.showSubIssuesExplicit).toBe(false);
    // Other preferences survive: a v1 snapshot already went through the legacy
    // rewrite, so replaying it would discard the user's sort/status choice.
    expect([shown.sortBy, shown.sortDirection]).toEqual(["position", "asc"]);
    expect(shown.statusFilters).toEqual(["done"]);
    expect(shown.cardProperties.project).toBe(true);

    // An explicitly closed project stays closed rather than reopening when the
    // project board is searched or switched to a non-board mode.
    const closed = getIssueSurfaceViewStore("project:closed").getState();
    expect(closed.showSubIssues).toBe(false);
    expect(closed.showSubIssuesExplicit).toBe(true);
    expect(effectiveShowSubIssues(closed, "child")).toBe(false);

    // The explicit flag was added later; a bare `showSubIssues: false` from
    // that era is equally a manual close and must not become the automatic rule.
    const earlyClosed = getIssueSurfaceViewStore("project:early-closed").getState();
    expect(earlyClosed.showSubIssues).toBe(false);
    expect(earlyClosed.showSubIssuesExplicit).toBe(true);
    expect(effectiveShowSubIssues(earlyClosed, "child")).toBe(false);

    // The project actor tabs follow their workspace counterparts and keep
    // showing sub-issues; only the project's own board changes.
    const agentsTab = getIssueSurfaceViewStore("project:tab:agents").getState();
    expect(agentsTab.showSubIssues).toBe(true);
    expect(agentsTab.showSubIssuesExplicit).toBe(true);

    // Workspace boards and saved views keep their existing behavior.
    const workspace = getIssueSurfaceViewStore("workspace:all").getState();
    expect(workspace.showSubIssues).toBe(true);
    expect(workspace.showSubIssuesExplicit).toBe(true);
    expect([workspace.sortBy, workspace.sortDirection]).toEqual(["position", "asc"]);
    const saved = getIssueSurfaceViewStore("view:saved").getState();
    expect(saved.showSubIssues).toBe(true);
    expect(saved.showSubIssuesExplicit).toBe(true);
  });

  it("applies the legacy rewrite and the project child reset to a v0 snapshot exactly once", async () => {
    localStorage.setItem(`${ISSUE_SURFACE_VIEW_STORAGE_KEY}:acme`, JSON.stringify({
      version: 0,
      state: {
        surfaces: {
          "project:legacy": {
            state: {
              sortBy: "position",
              sortDirection: "asc",
              // The pre-v1 shape showed every card property.
              cardProperties: {
                priority: true,
                description: true,
                assignee: true,
                startDate: true,
                dueDate: true,
                project: true,
                childProgress: true,
                labels: true,
              },
              hiddenStatuses: [],
            },
            updatedAt: "2026-01-01",
          },
        },
      },
    }));
    setCurrentWorkspace("acme", "ws_a");
    await flush();

    const store = getIssueSurfaceViewStore("project:legacy");
    const migrated = store.getState();
    expect([migrated.sortBy, migrated.sortDirection]).toEqual(["updated_at", "desc"]);
    expect(migrated.cardProperties.project).toBe(false);
    expect(migrated.hiddenStatuses).toEqual(["cancelled"]);
    expect(migrated.showSubIssues).toBe(false);
    expect(migrated.showSubIssuesExplicit).toBe(false);

    // The migrated snapshot is now v2. A later choice must not be treated as a
    // stale v0/v1 default and reset on the next reload.
    store.getState().setSortBy("title");
    setCurrentWorkspace("beta", "ws_b");
    await flush();
    setCurrentWorkspace("acme", "ws_a");
    await flush();
    expect(store.getState().sortBy).toBe("title");
    expect(store.getState().showSubIssuesExplicit).toBe(false);
  });

  it("keeps project child visibility chosen after the reset across reloads", async () => {
    setCurrentWorkspace("acme", "ws_a");
    await flush();
    const store = getIssueSurfaceViewStore("project:p1");

    store.getState().setShowSubIssues(true);
    const raw = localStorage.getItem(`${ISSUE_SURFACE_VIEW_STORAGE_KEY}:acme`);
    expect(JSON.parse(raw as string).version).toBe(2);
    setCurrentWorkspace("beta", "ws_b");
    await flush();
    setCurrentWorkspace("acme", "ws_a");
    await flush();
    expect(store.getState().showSubIssues).toBe(true);
    expect(store.getState().showSubIssuesExplicit).toBe(true);

    store.getState().setShowSubIssues(false);
    setCurrentWorkspace("beta", "ws_b");
    await flush();
    setCurrentWorkspace("acme", "ws_a");
    await flush();
    expect(store.getState().showSubIssues).toBe(false);
    expect(store.getState().showSubIssuesExplicit).toBe(true);
    expect(effectiveShowSubIssues(store.getState(), "child")).toBe(false);
  });

  it("migrates legacy defaults without rewriting explicit preferences", async () => {
    localStorage.setItem(
      `${ISSUE_SURFACE_VIEW_STORAGE_KEY}:acme`,
      JSON.stringify({
        version: 0,
        state: {
          surfaces: {
            "workspace:legacy": {
              state: {
                sortBy: "position",
                sortDirection: "asc",
                cardProperties: {
                  priority: true,
                  description: true,
                  assignee: true,
                  startDate: true,
                  dueDate: true,
                  project: true,
                  childProgress: true,
                  labels: true,
                },
                hiddenStatuses: [],
              },
              updatedAt: "2026-01-01T00:00:00Z",
            },
            "workspace:custom": {
              state: {
                sortBy: "title",
                sortDirection: "desc",
                cardProperties: { description: false },
                hiddenStatuses: ["done"],
              },
              updatedAt: "2026-01-01T00:00:00Z",
            },
          },
        },
      }),
    );
    setCurrentWorkspace("acme", "ws_a");
    await flush();

    const legacy = getIssueSurfaceViewStore("workspace:legacy").getState();
    expect([legacy.sortBy, legacy.sortDirection]).toEqual([
      "updated_at",
      "desc",
    ]);
    expect(legacy.cardProperties.description).toBe(false);
    expect(legacy.hiddenStatuses).toEqual(["cancelled"]);

    const custom = getIssueSurfaceViewStore("workspace:custom").getState();
    expect([custom.sortBy, custom.sortDirection]).toEqual(["title", "desc"]);
    expect(custom.cardProperties.description).toBe(false);
    expect(custom.hiddenStatuses).toEqual(["done"]);
  });

  it("omits the redundant project property only on fresh project surfaces", async () => {
    setCurrentWorkspace("acme", "ws_a");
    await flush();

    expect(
      getIssueSurfaceViewStore("project:compact-card").getState().cardProperties
        .project,
    ).toBe(false);
    expect(
      getIssueSurfaceViewStore("workspace:compact-card").getState().cardProperties
        .project,
    ).toBe(true);
  });

  it("isolates view state by surface key inside one workspace registry", async () => {
    setCurrentWorkspace("acme", "ws_a");
    await flush();
    const projectA = getIssueSurfaceViewStore("project:a");
    const projectB = getIssueSurfaceViewStore("project:b");

    projectA.getState().setViewMode("list");
    projectA.getState().togglePriorityFilter("high");

    expect(projectA.getState().viewMode).toBe("list");
    expect(projectB.getState().viewMode).toBe("board");
    expect(projectB.getState().priorityFilters).toEqual([]);

    const raw = localStorage.getItem(`${ISSUE_SURFACE_VIEW_STORAGE_KEY}:acme`);
    expect(raw).not.toBeNull();
    const parsed = JSON.parse(raw as string);
    expect(parsed.state.surfaces["project:a"].state.viewMode).toBe("list");
    expect(parsed.state.surfaces["project:a"].state.priorityFilters).toEqual([
      "high",
    ]);
    expect(parsed.state.surfaces["project:b"]).toBeUndefined();
  });

  it("persists table columns, order, widths, grouping, and calculation per surface", async () => {
    setCurrentWorkspace("acme", "ws_a");
    await flush();
    const projectA = getIssueSurfaceViewStore("project:table-a");
    const projectB = getIssueSurfaceViewStore("project:table-b");

    projectA.getState().setViewMode("table");
    projectA.getState().toggleTableColumn("identifier");
    projectA.getState().toggleTableColumn("property:estimate");
    projectA
      .getState()
      .reorderTableColumn("property:estimate", "identifier");
    projectA.getState().setTableColumnWidth("property:estimate", 184);
    projectA.getState().setTableGrouping("status");
    projectA.getState().setTableCalculation("average");

    expect(projectA.getState().viewMode).toBe("table");
    expect(
      projectA.getState().tableColumns.map((column) => column.key),
    ).toEqual([
      "title",
      "status",
      "priority",
      "assignee",
      "due_date",
      "labels",
      "property:estimate",
      "identifier",
    ]);
    expect(
      projectA
        .getState()
        .tableColumns.find((column) => column.key === "property:estimate")
        ?.width,
    ).toBe(184);
    expect(projectA.getState().tableGrouping).toBe("status");
    expect(projectA.getState().tableCalculation).toBe("average");

    expect(projectB.getState().viewMode).toBe("board");
    expect(
      projectB.getState().tableColumns.some((column) =>
        column.key.startsWith("property:"),
      ),
    ).toBe(false);

    const raw = localStorage.getItem(`${ISSUE_SURFACE_VIEW_STORAGE_KEY}:acme`);
    const parsed = JSON.parse(raw as string);
    expect(
      parsed.state.surfaces["project:table-a"].state.tableColumns,
    ).toContainEqual({ key: "property:estimate", width: 184 });
  });

  it("rehydrates existing surface stores when the workspace changes", async () => {
    setCurrentWorkspace("acme", "ws_a");
    await flush();
    const projectA = getIssueSurfaceViewStore("project:a");
    projectA.getState().setViewMode("list");

    setCurrentWorkspace("beta", "ws_b");
    await flush();
    expect(projectA.getState().viewMode).toBe("board");
    projectA.getState().setViewMode("swimlane");

    setCurrentWorkspace("acme", "ws_a");
    await flush();
    expect(projectA.getState().viewMode).toBe("list");

    setCurrentWorkspace("beta", "ws_b");
    await flush();
    expect(projectA.getState().viewMode).toBe("swimlane");
  });

  // Later opens of a saved view read this local copy, not the definition, so
  // a condition missing here silently drops out of the view.
  it("keeps a saved view's agent running filter across a reload", async () => {
    setCurrentWorkspace("acme", "ws_a");
    await flush();
    seedIssueSurfaceViewState("view:running", { agentRunningFilter: true });
    const view = getIssueSurfaceViewStore("view:running");
    expect(view.getState().agentRunningFilter).toBe(true);

    setCurrentWorkspace("beta", "ws_b");
    await flush();
    expect(view.getState().agentRunningFilter).toBe(false);

    setCurrentWorkspace("acme", "ws_a");
    await flush();
    expect(view.getState().agentRunningFilter).toBe(true);
  });

  it("persists Idle per workspace and resets it with the other filters", async () => {
    setCurrentWorkspace("acme", "ws_a");
    await flush();
    seedIssueSurfaceViewState("view:idle", { agentIdleFilter: true });
    const view = getIssueSurfaceViewStore("view:idle");
    expect(view.getState().agentIdleFilter).toBe(true);
    setCurrentWorkspace("beta", "ws_b");
    await flush();
    expect(view.getState().agentIdleFilter).toBe(false);
    setCurrentWorkspace("acme", "ws_a");
    await flush();
    expect(view.getState().agentIdleFilter).toBe(true);
    view.getState().clearFilters();
    expect(view.getState().agentIdleFilter).toBe(false);
  });

  it("clears one surface without touching sibling surfaces", async () => {
    setCurrentWorkspace("acme", "ws_a");
    await flush();
    const projectA = getIssueSurfaceViewStore("project:a");
    const projectB = getIssueSurfaceViewStore("project:b");
    projectA.getState().setViewMode("list");
    projectB.getState().setViewMode("gantt");

    clearIssueSurfaceViewState("project:a");

    expect(projectA.getState().viewMode).toBe("board");
    expect(projectB.getState().viewMode).toBe("gantt");
    expect(getIssueSurfaceViewStateRegistrySnapshot()["project:a"]).toBeUndefined();
    expect(getIssueSurfaceViewStateRegistrySnapshot()["project:b"]?.state.viewMode).toBe(
      "gantt",
    );
  });

  it("prunes invalid surfaces and resets live stores for pruned keys", async () => {
    setCurrentWorkspace("acme", "ws_a");
    await flush();
    const projectA = getIssueSurfaceViewStore("project:a");
    const projectB = getIssueSurfaceViewStore("project:b");
    projectA.getState().setViewMode("list");
    projectB.getState().setViewMode("gantt");

    pruneIssueSurfaceViewStates(["project:a"]);

    expect(projectA.getState().viewMode).toBe("list");
    expect(projectB.getState().viewMode).toBe("board");
    expect(getIssueSurfaceViewStateRegistrySnapshot()["project:a"]?.state.viewMode).toBe(
      "list",
    );
    expect(getIssueSurfaceViewStateRegistrySnapshot()["project:b"]).toBeUndefined();
  });

  it("works as a real StoreApi with ViewStoreProvider subscriptions", async () => {
    setCurrentWorkspace("acme", "ws_a");
    await flush();
    const store = getIssueSurfaceViewStore("project:provider");

    function Probe() {
      const viewMode = useViewStore((state) => state.viewMode);
      const setViewMode = useViewStore((state) => state.setViewMode);
      return (
        <button type="button" onClick={() => setViewMode("list")}>
          {viewMode}
        </button>
      );
    }

    render(
      <ViewStoreProvider store={store}>
        <Probe />
      </ViewStoreProvider>,
    );

    expect(screen.getByRole("button", { name: "board" })).toBeTruthy();
    act(() => {
      fireEvent.click(screen.getByRole("button", { name: "board" }));
    });
    expect(screen.getByRole("button", { name: "list" })).toBeTruthy();
  });
});

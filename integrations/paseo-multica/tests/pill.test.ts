// @vitest-environment node
import { findSvgIconProblem } from "@getpaseo/plugin";
import { describe, expect, it } from "vitest";
import { PILL_ICON, TASK_LINE_UNITS, clipLine, progressRingSvg, taskPill } from "../client/pill";
import type { BoardData, BoardIssue } from "../shared/board";

const NOW = Date.parse("2026-11-17T12:00:00Z");

function issue(id: string, overrides: Partial<BoardIssue> = {}): BoardIssue {
  return {
    id, identifier: `WORK-${id}`, title: `Task ${id}`, status: "in_progress", statusCategory: "started",
    lastActivityAt: new Date(NOW).toISOString(), activeRun: false, lastRunFailed: false, pullRequests: [], ...overrides,
  };
}

function board(issues: BoardIssue[], rootIds = issues.slice(0, 1).map((item) => item.id)): BoardData {
  return {
    state: "ready", scope: "session", viewer: { agentId: "leader", forgeUsernames: [] },
    issues, rootIds, projects: [], truncated: false, checkedAt: "now",
  };
}

const done = { status: "done", statusCategory: "completed" };

describe("composer pill", () => {
  it("draws the focus tree's child progress and adds it as a second tooltip line", () => {
    const root = issue("10", { title: "Ship the ring" });
    const pill = taskPill(board([root, issue("11", { parentId: "10", ...done }), issue("12", { parentId: "10" })]), NOW);
    expect(pill).toEqual({
      label: "WORK-10 · 无待办",
      title: "WORK-10 Ship the ring\n已完成 1/2 个子任务",
      icon: { svg: progressRingSvg({ done: 1, total: 2 }) },
    });
  });

  it("keeps the plain icon without children or a ready board", () => {
    expect(taskPill(board([issue("10")]), NOW)).toMatchObject({ icon: PILL_ICON, title: "WORK-10 Task 10" });
    expect(taskPill({ state: "disconnected", reason: "No binding.", checkedAt: "now" }, NOW)).toMatchObject({ icon: PILL_ICON, title: "No binding." });
  });

  it("clips only the task line, by display width", () => {
    const long = "把 Paseo 输入区 Multica 入口改成显示父任务子任务完成进度的环形图并保持原有点击行为";
    const [line, count] = taskPill(board([issue("10", { title: long }), issue("11", { parentId: "10" })]), NOW).title.split("\n");
    expect(line).toMatch(/^WORK-10 把 Paseo.*…$/);
    expect([...line!].reduce((sum, char) => sum + (/[一-鿿]/.test(char) ? 2 : 1), 0)).toBeLessThanOrEqual(TASK_LINE_UNITS);
    expect(count).toBe("已完成 0/1 个子任务");
    expect(clipLine("x".repeat(TASK_LINE_UNITS))).toBe("x".repeat(TASK_LINE_UNITS));
    expect(clipLine("x".repeat(TASK_LINE_UNITS + 1))).toBe(`${"x".repeat(TASK_LINE_UNITS - 1)}…`);
  });

  it.each([[0, 4], [1, 4], [3, 4], [4, 4], [1, 3]])("draws %i of %i as a valid host icon", (doneCount, total) => {
    const svg = progressRingSvg({ done: doneCount, total });
    expect(findSvgIconProblem(svg)).toBeNull();
    expect(svg).toContain('stroke="currentColor"');
    expect(svg).not.toMatch(/animate|<style/);
  });

  it("draws only the track at zero and a full circle when every child is closed", () => {
    expect(progressRingSvg({ done: 0, total: 4 }).match(/<circle|<path/g)).toEqual(["<circle"]);
    expect(progressRingSvg({ done: 4, total: 4 }).match(/<circle|<path/g)).toEqual(["<circle", "<circle"]);
  });

  it("uses the large arc only past half", () => {
    expect(progressRingSvg({ done: 1, total: 4 })).toContain("A5.5 5.5 0 0 1 13.5 8");
    expect(progressRingSvg({ done: 3, total: 4 })).toContain("A5.5 5.5 0 1 1 2.5 8");
  });
});

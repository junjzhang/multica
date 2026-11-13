// @vitest-environment node
import { describe, expect, it } from "vitest";
import { splitMarkdown } from "./split-markdown";

describe("notification mentions", () => {
  it("labels a notification in a table cell", () => {
    const input = "| A |\n| --- |\n| [@T](mention://agent-notify/abc) |";
    expect(splitMarkdown(input, "Notify")[0]).toEqual({ type: "prose", content: "| A |\n| --- |\n| [@T · Notify](mention://agent-notify/abc) |" });
  });

  it("labels real links while leaving code unchanged", () => {
    const input = "[@A](mention://agent-notify/abc) `[@B](mention://agent-notify/def)`\n\n```\n[@C](mention://agent-notify/ghi)\n```";
    const result = splitMarkdown(input, "Notify");
    expect(result[0]).toEqual({ type: "prose", content: "[@A · Notify](mention://agent-notify/abc) `[@B](mention://agent-notify/def)`" });
    expect(result[1]).toEqual({ type: "code", lang: undefined, code: "[@C](mention://agent-notify/ghi)" });
  });
});

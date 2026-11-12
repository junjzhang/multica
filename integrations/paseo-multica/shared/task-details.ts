import { z } from "zod";

const actor = z.object({ type: z.string(), id: z.string() });
const child = z.object({ id: z.string(), identifier: z.string(), title: z.string(), status: z.string(), status_name: z.string().nullish(), status_category: z.string().optional(), assignee_type: z.string().nullish(), assignee_id: z.string().nullish(), assignee_name: z.string().optional() });
export const detailWireSchemas = {
  contract: z.object({ contract: z.object({ versions: z.array(z.object({ number: z.number(), criteria: z.array(z.string()) })), deliveries: z.array(z.object({ version: z.number(), created_at: z.string() })).default([]) }).nullable(), accepted: z.array(z.number()).default([]), pending_actor: actor.nullish(), executor: actor.nullish(), acceptor: actor.nullish() }),
  runs: z.array(z.object({ agent_id: z.string(), agent_name: z.string().optional(), status: z.string(), created_at: z.string(), started_at: z.string().nullish(), completed_at: z.string().nullish() })),
  comments: z.array(z.object({ author_type: z.string(), author_id: z.string(), author_name: z.string().optional(), content: z.string(), created_at: z.string() })),
  prs: z.object({ pull_requests: z.array(z.object({ title: z.string(), html_url: z.string(), number: z.number() })) }),
  children: z.object({ issues: z.array(child) }),
};
export type DetailSection = keyof typeof detailWireSchemas;
export const detailSection = z.enum(["contract", "runs", "comments", "prs", "children"]);
export const detailResultSchema = z.discriminatedUnion("ok", [
  z.object({ ok: z.literal(true), data: z.unknown() }),
  z.object({ ok: z.literal(false), error: z.string() }),
]);
export type DetailResult = z.infer<typeof detailResultSchema>;

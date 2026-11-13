import { z } from "zod";

export const ResultTakeoverResponseSchema = z.object({
  id: z.string().min(1),
  client_request_id: z.string().min(1),
  result_task_id: z.string().min(1),
  from_return_id: z.string().min(1),
  target_return_id: z.string().min(1),
  current_return_id: z.string().min(1),
  created_at: z.string().min(1),
});

const TargetDisplaySchema = z.object({
  provider: z.string().min(1),
  session_ref: z.string().min(1),
  display_name: z.string(),
});

const TargetChoiceSchema = z.discriminatedUnion("kind", [
  TargetDisplaySchema.extend({
    kind: z.literal("external_session"),
    external_session_id: z.string().min(1),
    available: z.boolean().catch(false),
  }),
  TargetDisplaySchema.extend({
    kind: z.literal("managed_task"),
    source_task_id: z.string().min(1),
    selection_version: z.string().min(1),
    available: z.boolean().catch(false),
  }),
]);

export const ResultTakeoverOptionsSchema = z.object({
  current_return_id: z.string().min(1),
  returns: z.array(z.object({
    return_id: z.string().min(1),
    kind: z.enum(["managed_task", "external_session"]),
    target: TargetDisplaySchema.nullable(),
  })),
  targets: z.array(TargetChoiceSchema),
});

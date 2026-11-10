import { z } from "zod";

const Actor = z.object({ type: z.string(), id: z.string() });
const Indices = z.array(z.number().int().nonnegative());
const MergeAuthorization = z.object({
  allowed: z.boolean().default(false),
  repository_url: z.string().nullable().default(null),
  target_branch: z.string().nullable().default(null),
});
const Version = z.object({
  number: z.number().int().positive(),
  goal: z.string(),
  scope: z.string().default(""),
  authorization_note: z.string(),
  criteria: z.array(z.string()).min(1),
  executor: Actor,
  acceptor: Actor,
  author: Actor,
  created_at: z.string(),
  basis: z.object({
    executor_representative: Actor.nullable().optional(),
    merge_authorization: z.object({
      allowed: z.boolean().default(false),
      repository_url: z.string().nullable().default(null),
      target_branch: z.string().nullable().default(null),
    }),
  }),
});
const Delivery = z.object({
  id: z.string(),
  version: z.number().int(),
  criteria: Indices,
  evidence: z.string(),
  child_ids: z.array(z.string()).default([]),
  author: Actor,
  created_at: z.string(),
});
const Decision = z.object({
  delivery_id: z.string(),
  version: z.number().int(),
  outcome: z.string(),
  criteria: Indices,
  reason: z.string().default(""),
  child_ids: z.array(z.string()).default([]),
  author: Actor,
  created_at: z.string(),
});
export const ContractViewSchema = z.object({
  contract: z
    .object({
      sequence: z.number().int().nonnegative(),
      versions: z.array(Version).min(1),
      deliveries: z.array(Delivery),
      decisions: z.array(Decision),
    })
    .nullable(),
  stale: z.boolean().default(false),
  accepted: Indices.default([]),
  pending_actor: Actor.nullable().default(null),
  executor: Actor.nullable().default(null),
  executor_representative: Actor.nullable().default(null),
  acceptor: Actor.nullable().default(null),
  merge_authorization: MergeAuthorization.default({
    allowed: false,
    repository_url: null,
    target_branch: null,
  }),
  can_edit: z.boolean().default(false),
  can_submit: z.boolean().default(false),
  can_review: z.boolean().default(false),
});
export type ContractView = z.infer<typeof ContractViewSchema>;
export interface ContractRequest {
  action: "revise" | "submit" | "accept" | "return";
  client_request_id: string;
  expected_sequence: number;
  version?: number;
  goal?: string;
  scope?: string;
  authorization_note?: string;
  requirements?: string[];
  criteria?: number[];
  evidence?: string;
  delivery_id?: string;
  reason?: string;
  child_ids?: string[];
}

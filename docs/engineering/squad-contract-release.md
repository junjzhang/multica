# Squad contract release handoff

Deploy the server change and migration 654 together before changing shared agent
guidance. No installed skills, registered roles, production squads or live
contracts were modified by this change.

After deployment, verify `multica issue contract get` exposes
`executor_representative` and use a disposable squad agreement to check real
leader submission and upstream acceptance. Do not rewrite existing assignments
or contracts. Member and agent contracts retain their existing basis shape.

`patches/squad-contract-skills-v1.patch` is the exact release companion for:

- `skills/development-workflow/SKILL.md`
- `skills/multica/references/contracts.md`
- `skills/development-workflow/references/implementer.md`

The release owner should check and apply this patch from the installation root
containing `skills/`, only after that installation uses the new server. The
patch intentionally fails against mismatched text; reconcile any intervening
edits instead of replacing whole files. Refresh registered Implementer role
instructions from the updated reference as a separate, targeted platform
action. The patch also provides the exact old/new role sentence for that update.

The shipped squad briefing and platform squad reference already describe the
new contract behavior. A squad owns the overall agreement, its real current
leader submits, and the named upstream acceptor decides. Leader changes require
an authorized requester to revise; history and accepted return addresses stay
fixed. Independent member subcontracts require independent acceptance.

On rollback, retain the old installed guidance until the server is restored and
do not submit squad contracts through an older backend. Migration 654 down
restores the original member/agent basis function; it does not delete contract
history or migrate responsibility to a leader.

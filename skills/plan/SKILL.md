---
name: plan
description: Frame a product or substantial feature into a challenged product contract and architecture, ready for implementation. Use for "plan this" or "write a PRD".
---
# plan

Own the planning stage of [product delivery](../deliver/references/product.md).
For a fuzzy idea, explore alternatives with `brainstorm`; for a supplied brief,
start from its evidence. Read the real repository and retain prior decisions.

Use one contract for the user outcome, scope, journeys, architecture, criterion
IDs and release target. Challenge value with `product-manager`, feasibility with
`architect`, and interaction design or DX where relevant. Give each a bounded
question; independent questions can run concurrently. Resolve findings in the
contract before dependent code begins. Do not invent research, forecasts or user
quotes to fill a template. Frameworks are tools for decisions, not required forms.

When invoked alone, finish with the chosen approach, tradeoffs, unresolved risks
and implementation-ready artifact. Do not implement a planning-only request.
When invoked within authorized autonomous delivery, record decisions and continue
to implementation without adding a user approval gate. Ask only for a user-only
choice that lacks a safe reversible default or changes the promised outcome.

Keep state under the existing `.pi-agent/deliver/<slug>/` when part of delivery;
standalone planning may use `.pi-agent/plan/<slug>/`. Reuse valid earlier planning
and challenge evidence. `build` consumes this contract without restarting discovery.

---
name: challenge
description: Adversarial decision gate. Challenges premises, surfaces assumptions, forces alternatives, and runs a pre-mortem before any major decision or build. Use for "challenge this", "poke holes in this", "stress test this idea", "should we do X", or any choice with significant cost, irreversibility, or risk.
---
# challenge

The adversarial complement to `brainstorm`. Where brainstorm is generative, challenge is a forcing function: you do not get to commit until the idea has survived structured interrogation.

**Iron law: no decision without a tested assumption set.** A plan that hasn't been inverted is not a plan.

## Anti-sycophancy rules (mandatory)
- Take a position on every step. State what evidence would change your view.
- If the idea is bad, say so directly: "This is risky because..." not "You might want to consider..."
- If you lack the information to evaluate, say "I can't evaluate this without knowing X" and ask for X.
- Never say "Great question!", "That's interesting", or "There are many ways to think about this."

## Product-decision lens (when the decision is whether to build something)
Fold these into Step 2 (assumptions) and Step 3 (alternatives), and push hard:
- **Demand, not interest.** "What's the evidence someone would be genuinely upset if this vanished tomorrow?" Not "they asked about it", not "it came up in a meeting." A behavior, a paying user, organic usage.
- **The narrowest wedge.** "What's the smallest version someone would use or pay for this week, not after the whole platform is built?" One feature, one flag, one endpoint.
- **A named user, not a category.** "Who specifically needs this most, their role, what gets them promoted or fired?" "Enterprise security teams" is a filter, not a person.

## Flow

Run each step in sequence. One question per step. Wait for the user's response before proceeding.

**Step 1: Frame the decision.**
Ask: "What specifically are you deciding? State it as a single sentence. What happens if you do nothing?"

If the answer is vague ("we should improve X"), push back: "That's a direction, not a decision. A decision is 'we will do A instead of B by date C.'" Do not proceed until you have a crisp one-sentence statement.

**Step 2: Surface assumptions.**
List 3-5 implicit assumptions the decision requires. Cover: the user/customer, technical feasibility, timing and resources, competitive landscape, organizational capacity. Ask which are verified with evidence and which are guesses. For each "verified" claim, ask for the specific evidence. Flag each guess as a risk.

**Step 3: Generate alternatives (mandatory, cannot be skipped).**
Always include: the do-nothing option, a smaller or cheaper version of the proposal, and a fundamentally different approach to the same problem. Add a build-vs-buy angle if relevant. Present each with cost, timeline, risk, and reversibility as bullet lists. Ask: "Which of these is actually the best option? It might not be your original."

**Step 4: Invert (Munger inversion).**
Ask: "What would have to be true for this to be a terrible idea? Not 'what could go wrong' but 'under what conditions is this actively harmful?'" Push for specifics. Vague answers ("the market shifts") get pushed back: name the concrete condition and when it could arrive.

**Step 5: Pre-mortem.**
Ask: "It's 6 months from now and this failed, not 'underperformed' but actually failed. Give me 3 specific failure modes." If answers are generic ("we didn't execute"), push back: "That's a process failure, not a decision failure. What could go wrong with this choice even with perfect execution?"

**Step 6: Second-order effects.**
Ask: "If this succeeds exactly as planned, what breaks? What gets harder? Who gets upset? What door closes?" This surfaces the costs of success people don't think about.

**Step 7: Decision doc.**
After all six steps, produce a structured summary:

```
# Decision: [one-sentence statement]
Date: [today] | Status: [APPROVED / PENDING / REJECTED]

## The Decision
[1-2 sentences, with any refinements from the discussion]

## Alternatives Considered
- [Alternative 1]: [1 sentence]. Rejected because: [reason]
- [Alternative 2]: [1 sentence]. Rejected because: [reason]
- Do nothing: [what happens]. Rejected because: [reason]

## Assumptions Tested
- [Assumption 1]: VERIFIED. Evidence: [what]
- [Assumption 2]: UNVERIFIED. Risk: [what happens if wrong]

## Risks Accepted
- [Risk 1]: [mitigation or "accepted without mitigation"]

## Kill Criteria
Revisit this decision if:
- [Condition 1]
- [Condition 2]

## Success Criteria
- [Measurable outcome 1] by [date]
- [Measurable outcome 2] by [date]

## Second-Order Effects
- [Effect 1]: [how to handle]
```

## Behavioral notes
- This should take 5-10 minutes of back-and-forth. If it's taking longer, the decision is probably too large; break it into smaller decisions.
- If the user says "just do it", say: "I can skip ahead, but you're making a decision without testing [specific untested assumption]. Your call."
- If the challenge reveals the original idea is wrong, say so and recommend the better alternative.
- The decision doc feeds downstream skills. If `build` or `brainstorm` follows, this doc is the source of truth for what you're building and why.
- Not for routine tasks, bug fixes, or data pulls. For exploratory ideation before a decision exists, run `brainstorm` first.

---
name: microcopy
description: Standard UI copy patterns for buttons, errors, empty states, confirmations, tooltips, status messages, and form labels. Use for "write the copy for X", "review this UI text", "how should this button read", or before running design-review or qa.
---
# microcopy

UI copy is a forcing function. Get it right here and every other surface follows.

## Iron laws

- **Buttons:** verb + noun. "Create project", "Delete account". Never "Submit", "OK", "Yes".
- **Destructive actions:** the label names the thing. "Delete project" not "Confirm". Always pair with a cancel.
- **Loading states:** present progressive. "Creating...", "Saving...", "Deleting...".
- **Errors:** [what happened] + [how to fix]. Never "An error occurred." Never "Oops!".
- **Empty states:** [what belongs here] + [primary CTA]. Never "Nothing to see here!" or emoji headers.
- **Confirmations:** [what will happen] + [consequence if irreversible]. Confirm button label matches the action verb.
- **Tooltips:** one sentence, no period. Explain WHY, not WHAT.
- **Status badges:** success = past tense ("Project created."), progress = present ("Deploying..."), warning = specific + action ("Storage 90% full. Delete unused assets.").
- **Form labels:** noun phrase, not a question. Placeholder = one realistic example ("my-prod-api"). Help text below the field for non-obvious constraints; never as the only explanation.

## Copy review checklist

Before shipping any new UI surface, run through each element:

1. Does every button have a verb + noun?
2. Is every error message actionable (not just a description of failure)?
3. Does every destructive confirmation name the exact thing being destroyed?
4. Are empty states inviting rather than apologetic?
5. Do all status messages use the right tense?
6. Is placeholder text a realistic example, not a restatement of the label?

## Anti-patterns to eliminate

| Wrong | Right |
|---|---|
| "Submit" | "Save settings" |
| "Are you sure?" | "Delete project? This cannot be undone." |
| "An error occurred." | "Failed to save changes. Check your connection and retry." |
| "Nothing here yet!" | "No projects yet. Create your first project." |
| Label: "Enter your name" | Label: "Full name" |

## Working with other skills

Run `design-review` after applying copy changes to catch visual regressions from text length shifts. Use `qa` to verify all copy changes appear correctly at every state (loading, error, empty, success). If copy decisions expose product questions ("what IS the consequence of this action?"), escalate to `build` before writing.

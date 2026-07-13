---
name: design-system
description: React/Tailwind/shadcn/ui component conventions and all-states checklist. Auto-loads on any UI work. Pairs with design-review for visual audits.
---
# design-system

## Stack
React + Tailwind + shadcn/ui + lucide-react + recharts (charts).

## Layout constants
- Page max-width: `max-w-7xl`
- Card padding: `p-6`, gap between cards: `gap-4` or `gap-6`
- Headings: `font-semibold`. Body (dense): `text-sm`. Body (reading): `text-base`.

## Component rules
- **Buttons**: verb labels ("Save draft", not "Submit"). Destructive actions get a confirm modal.
- **Empty states**: always include a CTA. Never leave a blank box.
- **Loading**: skeleton preferred over spinner. Spinner only for transient sub-second ops.
- **Modals**: use for destructive confirms only; avoid for informational content.
- **Icons**: always add `aria-label` to icon-only buttons.
- **Forms**: inline validation on blur, not on submit. Show the error adjacent to the field.

## Accessibility (non-negotiable)
- Semantic HTML (`button`, `nav`, `main`, `section` with proper roles).
- Full keyboard navigation on all interactive elements.
- `focus-visible:ring-2` on all focusable elements.
- Minimum 4.5:1 contrast ratio for text.

## All-states checklist
Build every state before calling a component done:

| State | Requirement |
|---|---|
| Empty | Shown with a CTA or explanation, never a blank box |
| Loading | Skeleton or spinner; no layout shift on resolve |
| Populated | The happy path |
| Error | What went wrong + how to fix it (not just "Error") |
| Partial | Some data, some missing; each piece handles its own absence |
| Overflow | Long strings truncate gracefully; lists paginate or scroll |
| Permission denied | Why access is blocked + how to get it |

## Anti-patterns (flag in design-review)
- Generic gradient headers or hero sections with no purpose.
- Emoji in headings or labels.
- Center-aligning everything regardless of content type.
- Identical card grids that ignore content variation.
- Default box-shadow on every element.

## Workflow
- Use `build` to lock the component contract before building.
- Use `design-review` to screenshot and score the finished UI.
- Use `qa` to exercise every state above in the running app.
- Use `tdd` to drive empty/error/loading state rendering with tests.

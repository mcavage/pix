---
name: qa
description: Systematically QA-test a running web app with the browser, then report (and optionally fix) bugs with screenshot evidence. Use for "qa", "test this site/app", "find bugs", or after a UI change.
---
# qa

Use the available authorized browser to exercise the actual app and report bugs
with evidence. Discover its schema and prove access before testing; do not assume
a tool name, preinstalled browser or usable authentication. Use an isolated
profile with synthetic data. Missing browser access is an explicit coverage gap.

## Setup
- Make the target reachable. If it's this repo's app, detect and run its
  dev/start command and wait for the localhost port to come up. Otherwise use the
  given URL.

## Loop
1. **Map.** Open the app and `snapshot` (accessibility tree); enumerate the key
   flows (nav, forms, primary actions).
2. **Exercise.** Walk each flow with semantic actions (click/fill/select). Watch
   for: console errors, broken navigation, dead controls, layout breakage, failed
   network requests, bad empty/error states.
3. **Evidence.** `screenshot` every bug (before/after where relevant).
4. **Report.** Bugs by severity (critical / high / medium / cosmetic), each with
   repro steps, the screenshot, and `path:line` of the likely cause.
5. **Fix (optional).** If asked, fix each in source, re-run the flow, confirm
   with a fresh screenshot, gate with `code-review`, then `ship`.

Test responsive widths for layout-sensitive views. Never claim "works" without
having actually exercised the flow.

---
name: repo-healthcheck
description: Check repository code health with tests, types, lint, dead code, and ranked fixes. Use for a code quality check or before shipping.
---
# repo-healthcheck

Check the current repository independently of Pix harness health. Do not run
inference, account, memory, or agent smoke tests as part of this skill.


Run every detectable quality check, score it, compare to history, emit a compact
dashboard with the highest-impact fixes first.

### Detection (auto-detect from project files; SKIPPED, not CRITICAL, when a tool genuinely isn't present, never invent one)
| Category | Weight | Detect via |
|---|---|---|
| Tests | 30% | `package.json` scripts, `pytest.ini`, `go.mod`, `Cargo.toml` |
| Type check | 22% | `tsc`, `pyright`, `mypy`, `cargo check` |
| Lint | 18% | `eslint`, `ruff`, `flake8`, `golangci-lint`, `cargo clippy` |
| Dead code | 15% | `knip`, `ts-prune`, `vulture`, `deadnix` |
| Shell lint | 10% | `shellcheck` (only if shell scripts exist) |
| Other | 5% | formatting drift, schema validation, generated-file drift |

Composite = sum of (score × weight), rounded to 1 decimal; exclude SKIPPED from
the denominator.

### Scoring
10 clean · 8-9 pass with minor warnings · 5-7 localized/easy failures · 2-4 major
failures or blocked workflow · 0-1 broken/dangerously noisy. Failing tests on
critical paths score below lint failures at the same volume; missing type
coverage in a typed project scores below 6.
Status labels: CLEAN (9-10), WARNING (7-8.9), NEEDS WORK (4-6.9), CRITICAL (0-3.9).

### Steps
1. Detect tools; print what was found and skipped.
2. Run each detected tool once. Capture command, exit code, duration, key output.
3. Score each category. Never hide a failure.
4. Read `data/health-history.jsonl` if present; find the run closest to 7 days
   ago (or the most recent earlier run) for trend comparison.
5. Emit the dashboard. 6. Append one JSONL record.

```
HEALTH DASHBOARD
Repo: <name>  Branch: <branch>  Commit: <sha>
Overall: 8.3/10  WARNING  (+0.7 WoW)
Tests: 9.0 CLEAN   Type check: 8.5 WARNING   Lint: 6.0 NEEDS WORK
Dead code: 5.5 NEEDS WORK   Shell lint: 10.0 CLEAN   Other: 8.0 WARNING
What ran: …   Trends: …   Recommendations (by impact): …
```
Trend language: Up (>= +0.5), Flat (< 0.5), Down (>= -0.5); only call out
meaningful deltas. History record:
```json
{"timestamp":"ISO-8601","repo":"name","branch":"branch","commit":"sha","scores":{"tests":8.5,"typecheck":10,"lint":7,"deadcode":6,"shelllint":10,"other":8},"overall":8.3}
```

### Recommendations (sort by impact)
(1) broken tests on changed paths, (2) type-safety regressions, (3) high-volume
lint that hides signal, (4) dead code with maintenance drag, (5) shell issues in
release flows, (6) nice-to-have cleanup. Each: what it is, why it matters, scope,
expected score lift.

### Guardrails
Do not claim CLEAN when major categories failed. Do not penalize unused
categories. Do not bury a CRITICAL finding under a strong composite. Prefer
evidence over general statements.


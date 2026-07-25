---
name: tdd
description: Test-driven development — write a failing test first, watch it fail, write minimal code to pass, watch it pass, refactor. Use when writing or fixing logic and you need proof it tests anything.
---
# tdd

**Iron law: no production code without a failing test first.** If you didn't
watch the test fail, you don't know whether it tests the right thing.

## Cycle
1. **Red.** Write one minimal test for the next behavior. Clear name, one thing,
   real code (mocks only if unavoidable).
2. **Watch it fail.** Mandatory. Confirm it fails for the right reason (the
   behavior is missing), not a typo or a bad import. A test that passes
   immediately is testing something that already exists; fix the test.
3. **Green.** Write the simplest code that passes it. Don't add features the test
   doesn't demand.
4. **Watch it pass.** Run the suite: the new test passes, nothing else broke,
   output is clean.
5. **Refactor.** Only once green: remove duplication, improve names. Keep the
   tests green; don't add behavior.
6. Repeat for the next behavior.

Why first, not after: a test written after the code passes on its first run,
which proves nothing (it might test the wrong thing, or miss the edge case you
already forgot). Writing it first forces you to watch it catch the absence of the
behavior.

For a bug fix: write the test that reproduces the bug (red), fix it (green); it
stays as the regression test.

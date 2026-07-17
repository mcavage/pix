## What and why

What does this change, and why?

## How I verified

Commands you ran and what you saw. For host code, include the test run.

```
cd services/host && go build ./... && go test ./... && go vet ./...
```

## Checklist

- [ ] Host code builds and tests pass (`go build ./... && go test ./...`)
- [ ] No company-specific data (channels, accounts, hosts, connector env) added to
      the public tree; that belongs in a private overlay
- [ ] Skills/agents stay pure mechanism (no one person's specifics baked in)
- [ ] Docs updated if behavior or commands changed (README / AGENTS.md / docs/)
- [ ] `CHANGELOG.md` updated under Unreleased if user-facing
- [ ] If image or baked files changed, noted that a maintainer must `make load`

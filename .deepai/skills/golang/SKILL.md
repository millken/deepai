---
name: golang
description: "Use when writing, reviewing, refactoring, or debugging Go code. Corrects stale pre-1.22 patterns from training data and maps them to modern stdlib (slices, maps, iter, errors.Join, context cause, testing.B.Loop) for Go 1.26; includes a build/vet/test verification loop."
---

# Go Best Practices

Targets Go 1.26. Effective Go basics — naming, small interfaces, multiple return values, table-driven tests — are assumed knowledge; this skill exists for what training data gets wrong: patterns that were correct before Go 1.21–1.23 but are wrong or unnecessary now, and modern stdlib the model under-uses.

## Stale Patterns and Modern Replacements

!`cat "$SKILL_DIR/references/stale-to-modern.md"`

## Verification Loop

After any code change, run in order and stop at the first failure:

```bash
gofmt -l .            # must print nothing; use goimports if imports changed
go build ./...
go vet ./...          # copylocks, printf misuse, unreachable code
go test -race ./...   # fix races, never suppress them
go mod tidy           # only when imports or dependencies changed
```

If the repo configures golangci-lint or staticcheck, run it too. Never weaken a test to make it pass — if a test is genuinely wrong, change it explicitly and say why.

---
name: golang
description: "Use when writing, reviewing, refactoring, or debugging Go code. Covers naming conventions, error handling, concurrency patterns, interface design, and performance best practices."
---

# Go Best Practices

Guidelines based on [Effective Go](https://go.dev/doc/effective_go) for writing idiomatic, concise, and efficient Go code. Targets Go 1.25+.

## When to Use

Enable this skill when:
- Writing new Go features or modules
- Reviewing, refactoring, or improving Go code
- Debugging performance, concurrency, or maintainability issues

## Reference Materials

Load as needed using `read_file`:
- **Error prevention**: read_file `${SKILL_DIR}/references/common-mistakes.md`
- **Code quality**: read_file `${SKILL_DIR}/references/effective-go.md`

## Key Rules

- **Control structures**: Always use braces; avoid unnecessary `else`; initialize variables in `if`/`switch`.
- **Functions**: Use multiple return values for results and errors; named returns for readability; `defer` for cleanup.
- **Data structures**: Prefer slices and maps; understand the distinction between arrays, slices, and maps.
- **Generics**: Use type assertions and type switches for interface dynamics; leverage generics for code reuse.
- **Formatting**: Always use `gofmt` for consistent, automated style.
- **Naming**: No underscores; exported identifiers use MixedCaps, unexported use mixedCaps; names should convey intent concisely.
- **Error handling**: Always check and return errors; use `panic` only for unrecoverable precondition failures; preserve context with `fmt.Errorf("...: %w", err)`.
- **Concurrency**: Communicate via channels, not shared mutable state; propagate cancellation via `context`; use buffered channels judiciously.
- **Interface design**: Keep interfaces small and focused (1-3 methods ideal); program to interfaces, return concrete types.
- **Comments**: Document all exported symbols with comments starting with the symbol name; explain why, not what.
- **Code quality**: Reduce unnecessary code and complexity — know when to delete and simplify, not just add.

## Practical Checklist

- Make zero values useful; avoid requiring `New` constructors for basic usage.
- Slice/map initialization: use `make` when capacity is known; avoid unnecessary `nil` guards.
- Avoid over-allocation in loops: reuse buffers; confirm bottleneck before using `sync.Pool`.
- Error variables: use `errors.Is/As` for semantic matching, not string comparison.
- Logging: return errors to callers; log only at boundary layers (entry points, daemons); avoid duplicate logging.
- Testing: prefer table-driven tests; use `t.Helper()` for test helpers; ensure parallel tests use independent data.
- Performance: measure with `bench + pprof` before optimizing; avoid premature micro-optimization.
- Modules: keep `go.mod`/`go.sum` clean, run `go mod tidy` regularly; limit public API surface to reduce breaking changes.

## References

- Official guide: https://go.dev/doc/effective_go
- Code review comments: https://github.com/golang/go/wiki/CodeReviewComments
- Standard library: the canonical reference for Go idioms

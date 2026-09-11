# Stale Patterns → Modern Go (1.21–1.26)

Generated Go fails in a specific way: training data skews pre-1.22, so models emit correct-then, wrong-now patterns. Each entry names the stale habit and its current replacement. Everything else — Effective Go basics, nil maps, comma-ok, table-driven tests — is assumed knowledge; apply it, don't restate it.

## Semantics that changed

- **Loop variables are per-iteration since 1.22.** `v := v` inside `for range` is noise now; goroutines and closures capture each iteration's variable directly.
- **`for i := range n`** (int operand, 1.22) replaces `for i := 0; i < n; i++`.
- **Timers are GC'd since 1.23.** `time.After` in a select loop no longer leaks; hoisting `time.NewTimer` for that reason is obsolete.
- **`slices.Delete` zeroes the vacated tail** so it stops pinning references — but the input slice keeps its length; always reassign: `s = slices.Delete(s, i, j)`.

## Stdlib that replaces hand-rolled code

Before writing a loop, check:

- `slices`: `Contains`/`ContainsFunc`, `Index`/`IndexFunc`, `Equal`, `Sort`/`SortFunc`, `Compact`, `Clone`, `Concat`, `Chunk`, `Clip`, `Sorted(seq)`
- `maps`: `Clone`, `Copy`, `DeleteFunc`, `Equal`, `Insert`, `Keys`/`Values` (return `iter.Seq`), `Collect`
- builtins `min`/`max`/`clear` (1.21); `cmp.Compare` for orderings, `cmp.Or` for first-non-zero defaults (1.22)
- `sync.OnceFunc` / `OnceValue` / `OnceValues` (1.21) replace hand-rolled `sync.Once` plus guarded-result caching
- `strings.Cut`/`CutPrefix`/`CutSuffix`; iterator forms `strings.Lines`, `FieldsSeq`, `SplitSeq` (1.24)
- `math/rand/v2` (1.22) — no Seed, better primitives; `sort` is frozen — new code uses `slices`

Stale imports to fix on sight:

- `golang.org/x/exp/slices`, `golang.org/x/exp/maps` → stdlib `slices` / `maps`
- `pkg/errors` (`errors.Wrap`, `errors.Errorf`) → `fmt.Errorf` with `%w`

## Errors

- Wrap with `fmt.Errorf("...: %w", err)` — the one canonical form. Multiple `%w` verbs are legal (1.20); `Unwrap` then returns `[]error`.
- `errors.Join(err1, err2)` (1.22) to accumulate; `errors.Is`/`As` traverse joins. Never match on error strings.
- `errors.Is(err, fs.ErrNotExist)`, not `os.IsNotExist` — the os helpers predate `errors.Is` and only see os-package errors. Same for `io.EOF`, `context.Canceled`.
- `errors.ErrUnsupported` (1.21) as the sentinel for unimplemented operations.
- **Typed-nil trap:** returning a nil `*MyError` as `error` yields a non-nil interface. Return literal `nil`.

## Concurrency

- Structured fan-out: `golang.org/x/sync/errgroup` — `g.Wait()` returns the first error and cancels the group context. Prefer over bare `sync.WaitGroup` plus first-error plumbing.
- Every `go` statement has an owner responsible for its termination: `<-ctx.Done()`, a closed channel, or bounded work. If no owner exists, that's a leak.
- Use the atomic types — `atomic.Uint64`, `atomic.Int64`, `atomic.Pointer[T]` (1.19) — instead of `atomic.StoreUint64(&x, v)`; they also fix 32-bit alignment concerns.
- `context.Context` is the first parameter, named `ctx`; never stored in structs. `context.WithoutCancel` (1.21) detaches work that must finish (flush, commit) while keeping values; `WithCancelCause`/`context.Cause` (1.20) when cancellation carries a reason.

## Iterators (1.23)

- `iter.Seq[V]` / `iter.Seq2[K, V]`; range-over-func is the mechanism. Return `iter.Seq` from APIs when callers only range; materialize with `slices.Collect`/`maps.Collect`, consume pull-style with `iter.Pull`.
- `for i, v := range slices.Backward(s)`; `for k, v := range maps.All(m)`; `slices.Sorted(seq)` sorts any sequence.

## Testing

- `for b.Loop() { ... }` (1.24) in benchmarks — replaces manual `b.N` indexing and prevents dead-code elimination.
- `testing/synctest` (1.25) — hermetic goroutine bubbles with fake time, for testing time-dependent concurrent code deterministically.
- `t.Attr(key, value)` (1.24) for machine-readable test metadata in `go test -json`.
- `t.Helper()` in assertion helpers; `t.Cleanup` over `defer` in helpers.

## Still-true gotchas

- `defer` in a loop accumulates until function return — extract the loop body.
- `append` may share or copy the backing array. `s[:k]` handed to a caller keeps the whole array alive and mutable — copy (or `slices.Clip`) when isolation matters.
- Copying a struct that contains a mutex copies the lock (`go vet` copylocks); pass pointers.
- `:=` inside an `if` block shadows the outer `err`; check whether `=` was meant.

## Rarely needed, right when needed

- `os.Root` (1.24): file operations confined to a directory tree, immune to `..` traversal.
- `runtime.AddCleanup` (1.25): replaces `runtime.SetFinalizer` — multiple cleanups per object, no resurrection constraints.
- `weak.Pointer[T]` (1.24): canonicalization/caching maps that don't keep referents alive.
- Generics (1.18) are stable and cheap: prefer them over `interface{}` plus type assertions or duplicated code — but don't parameterize what is used with exactly one type.

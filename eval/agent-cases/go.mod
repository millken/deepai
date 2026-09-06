// This go.mod is a deliberate module boundary, not a real Go module: it
// exists ONLY so `go build ./...`/`go vet ./...`/`go test ./...` at the
// repo root stop descending into eval/agent-cases' fixture/ subtrees.
//
// Each case's fixture/ is a partial, single-file-at-a-time copy of real
// deepai source (pkg/agent/subagent.go alongside pkg/skill/tool.go but not
// the rest of pkg/agent or pkg/skill, say) — it is never meant to compile as
// a standalone package, only to be read/grepped by the subagent under test.
// Without this file, those partial copies are plain .go files sitting inside
// the deepai module tree, and the root module's `./...` pattern picks them
// up as broken packages (undefined: Skill, undefined: ChatRepl, ...),
// breaking the repo-wide build/vet/test gate for a reason that has nothing
// to do with deepai's own code.
module github.com/millken/deepai/eval/agent-cases/fixtures-do-not-build

go 1.24

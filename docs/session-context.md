# Single-session context

Long coding sessions were losing the thread. The cross-session memory store was not the thing that could fix that, and it was spending tokens on every turn anyway. This note is the before/after for the change that moves coherence onto the session itself.

## What was wrong

`pkg/memory` extracts a durable user biography: work context, personal context, "top of mind", and free-form facts. The extractor prompt tells the model to skip transient tool chatter. That is the opposite of what a coding turn needs (which file, which error, which decision).

Injection made it worse:

- Narrative fields were appended with no relevance check.
- Facts used a bag-of-words overlap with the last few messages. If nothing overlapped, every fact up to about 2000 tokens was injected; if anything overlapped, generic shared words ("file", "error") pulled the wrong facts in.
- Both the user scope and the session scope were injected, on every turn, at the end of the prompt where the model pays the most attention.
- Compaction blocked for up to 30 seconds on a synchronous extraction before rewriting history. That extraction cannot restore the tool output it is about to delete.

The transcript path had a matching hole. Aging and compaction kept the **head** of a tool result. Bash JSON puts `exit_code` and stderr at the end, and compiler errors do the same, so the cut dropped the only lines that decide the next action. The model was then told to re-call the tool, which is how a long session fills up again.

Mission charters (`.deepai/missions/`) were already the right shape of session-local state: they ride the trailing turn injection and survive compaction. Memory was a second copy of "things to remember", with worse content, on the same slot.

## What happens now

### Session working set

`SessionCarry` holds a small ledger, shared across the single-use agents the REPL builds for each turn:

- the latest real user objective, one line
- up to 8 files touched (`read` / `edited` / `wrote` / `searched` / `listed`) and whether the last operation failed
- up to 4 recent failures, including `bash` results whose JSON `exit_code` is non-zero

It is rendered into the trailing turn injection, after the charter and after any opted-in memory, and capped at 1600 bytes. The injection is not stored in the transcript, so rebuilding it when a tool finishes does not invalidate the prefix cache of the history.

Compaction still deletes tool bodies. The ledger is updated from the real result **before** that happens. A later absorb of the summarized transcript does not overwrite a live note. A process restart has only the transcript: paths that survived in tool-call arguments come back marked `compacted`; failure text that compaction already ate does not.

`/clear` replaces the whole carry, so the ledger goes with the session. It is not written to sqlite and it is not shared with subagents.

### Tool results

Aged tool results and compaction summaries keep about 70% of their budget from the head and the rest from the tail (`[...snip...]` between them). The aging budget and the compaction wrapper are unchanged, so a second compaction pass still recognizes its own output and stops.

### Cross-session memory

The store, `/refine`, rollback, and the memory tool stay. The hot path is opt-in via `memory_inject: true` (default off):

| | `memory_inject` off (default) | `memory_inject: true` |
|---|---|---|
| Automatic extraction / auto-refine | not scheduled | previous cadence |
| Preference extraction | not scheduled | previous behavior |
| Synchronous flush before compaction | skipped | one LLM extraction per compaction that actually rewrites history |
| Facts in the turn injection | omitted | user scope + session scope, as before |
| `/refine` | writes the store; status says injection is off | writes the store and later turns recall facts |

Nothing is deleted from sqlite. Turning the flag on later recalls facts that `/refine` already saved. There is no migration.

## How to verify

```bash
go test ./pkg/agent/ ./pkg/chat/ ./pkg/memory/ ./pkg/commands/
```

Behavior covered by tests:

- Aged and compacted tool output keeps a marker planted at the tail, and compacting a summary twice does not nest.
- A failed `edit_file` shows up in the next request's injection, and not in the request that ran before the tool.
- The same failure survives a compaction summary and a new `Agent` that shares the `SessionCarry`.
- A success on that path drops the failure line.
- With `InjectMemory` unset, compaction does not call the extractor, and a stored fact is absent from the injection. With it set, the flush runs once per real compaction and the fact is injected.
- `memoryHotPathOn` is false unless `MemoryInject` is set.

In a REPL: `/refine status` should say prompt injection is off. After a few edits, the model still sees the files and the latest failure in the trailing system note even once `/compact` has shortened the old tool output. Set `memory_inject: true` in config to get the old recall behavior back.

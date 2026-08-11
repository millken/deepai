---
name: docx-polish
description: "Use when the user asks to polish, proofread, improve the fluency of, or fix the grammar/tone of a .docx Word document while preserving its formatting, layout, and existing runs (bold, links, fields). Triggers on requests like 'polish this docx', 'clean up the grammar in this Word doc', 'make this document sound more formal', 'improve the wording of this file.docx'."
allowed-tools: [docx_read, docx_edit, task, ask_clarification]
agent: document-editor
---

# docx-polish

Polishes an existing `.docx` file in place: grammar, fluency, or tone, without
adding or removing paragraphs and without disturbing formatting you didn't
touch. The underlying model is "ground truth + narrow patch" — the original
file is authoritative, and every edit is a small, targeted splice, not a
rewrite.

## Step 0 — Delegate, don't edit directly

**Do not call `docx_edit` (or `docx_read`) from the main agent.** The
`agent: document-editor` frontmatter above is a declaration of intent only —
on the current wiring it does **not** restrict which tools are available to
you. The actual restriction only takes effect inside a subagent.

So the first and only tool call you make for this workflow is `task`:

- `agent_type: document-editor`
- Pass in the prompt: the resolved file path, the task mode (see Step 1),
  the protect list, and the polishing brief in the user's own words.

The subagent runs the read/edit loop below with the `document-editor`
profile's tool set — `docx_read`, `docx_edit`, `read_file`, `write_file`,
and `ask_clarification`, not only the two docx tools. If the document is
large enough to need more turns than one subagent's `MaxTurns` allows,
delegate again in batches: have each subagent report back a short decision
list (see Step 3) and hand that list to the next `task` call so tone and
terminology choices carry forward. Never fall back to doing the edits
yourself because delegation looks slower — a silently-unenforced allowlist
is worse than none.

Everything below this point describes what happens **inside** the
`document-editor` subagent (or is what you put in the delegation prompt).

## Step 1 — The three prompt layers

Every polishing pass is governed by three layers, in this order of
precedence:

1. **System rules (immutable)** — do not add or remove paragraphs; do not
   change the meaning of a sentence; preserve the author's voice, register,
   and terminology; never touch a protected item (Layer 3).
2. **Task mode (chosen from the user's request)** — pick exactly one:
   - `grammar` — fix spelling, grammar, and punctuation only; do not
     rephrase correct sentences.
   - `fluency` — grammar fixes plus smoother phrasing; sentence meaning and
     structure stay intact.
   - `formal-tone` — shift register toward formal/professional; grammar and
     fluency fixes included.
   If the user's request doesn't clearly map to one of these, ask via
   `ask_clarification` rather than guessing.
3. **Protect list** — items that must appear unchanged in the output if they
   appeared in the input. Default protect list: every number (including
   dates, versions, percentages) and every all-caps acronym. If the user
   mentions specific terms, names, or a house style glossary, add them to
   the list. Use `ask_clarification` before starting if the brief is
   ambiguous about what else to protect — asking once up front is cheaper
   than discovering a broken protected item after several chunks are
   already written back.

## Step 2 — The chunk loop

Large documents don't fit in one read or one edit. Process the document in
this loop:

1. `docx_read(path)` once, with no range, to get the outline (heading tree,
   paragraph counts) — but only above the tool's 200-paragraph threshold.
   On a smaller document this call returns a markdown chunk instead (there
   is no outline to get); if `next_start_para` in that response is nonzero,
   treat it the same as a chunk in the loop below rather than expecting a
   second, different-shaped response.
2. For each chunk (a heading section, or a `max_chars`-bounded slice of one
   if the section is too big):
   a. `docx_read(path, start_para=..., end_para=..., runs=true)` to get the
      chunk's paragraphs with their run breakdown.
   b. Polish the chunk text according to the three layers above.
   c. `docx_edit(path, edits=[...], protect=[...], reviewed_through_para=...)`
      **immediately** — write the chunk back before moving on. Pass the
      protect list from Step 1 as `docx_edit`'s `protect` argument on
      *every* call, not just the first — the tool validates it mechanically
      on each batch, and omitting it on a later call silently drops that
      protection for that chunk. Set `reviewed_through_para` to this
      chunk's `end_para` so a resumed session (a fresh subagent picking up
      a batch that ran out of turns) can tell how far the previous batch
      got.
   d. Continue from the `next_start_para` the read call returned.

**Do not interleave any other tool call between a chunk's read and its
write.** The read-then-write pair must be adjacent. This isn't a style
preference: this session's context gets compacted after a few messages, and
a read whose paired write is delayed risks the source text aging out before
you act on it. Keep read and edit next to each other every time.

## Step 3 — Carry a decision list, not the raw text, between chunks

Once a chunk is written back, drop its full text from your working memory.
Keep only a short, running "decision list": terminology choices you've
settled on ("keep 'user' not 'end user'"), tone calibrations, and anything
that would cause drift if the next chunk contradicted it. Pass this list
forward chunk to chunk (and subagent to subagent, if you had to delegate in
batches). It stays small; the raw text does not.

## Step 4 — Prefer `find` over whole-paragraph replace

When submitting an edit, give `run` or `find` whenever you can locate the
exact substring you're changing:

- `find` (or `run`) scopes the patch to a single `<w:t>` run, so every other
  run in the paragraph — bold spans, hyperlinks, field codes — keeps its
  original formatting untouched.
- Giving only `para` + `text` replaces the **whole paragraph** and collapses
  every run in it down to the first run's formatting. The tool will return a
  warning when this happens on a multi-run paragraph. Only do this when the
  rewrite is too pervasive to express as a substring match — e.g. you
  restructured the sentence enough that no clean substring survives — and
  even then, prefer rewriting as one or more `find`-scoped edits first.

## Step 5 — Never insert or delete paragraphs

Do not use `op: insert_before`, `op: insert_after`, or a paragraph-level
`op: delete`. Polishing changes wording, not document structure — the
system rule in Step 1 already forbids adding or removing paragraphs, and
avoiding these ops has a mechanical payoff too: `insert_*` and paragraph-level
`delete` shift every later paragraph's `para_index`, which would force you to
re-read the outline mid-run to keep indices valid. Staying `replace`-only
keeps every `para_index` you collected at the start of the loop valid for
the entire pass, so you never need to re-fetch the outline.

## Step 6 — Read the outcomes, don't just count them

After each `docx_edit`, compare `applied` to the number of edits you
submitted. If they differ, read every `outcomes[].reason` — don't just retry
blindly. Each reason is specific and actionable: it tells you whether a
`find` matched zero times (text already changed, or you misremembered it),
matched more than once (need a longer, more specific substring), crossed a
run boundary (need to fall back to `run` or restructure the edit), a
protected item would be disturbed (drop or narrow that edit — never widen
the protect list to make the edit "work"), or conflicts with an earlier
edit in the same batch. For a collision, reordering the two edits does
**not** help — the tool refuses the later one by input order regardless of
which one you put first — so follow the reason's own advice instead:
combine them into a single edit when they target the same paragraph, or
issue the later one in a separate `docx_edit` call (or as a delete on the
earlier paragraph followed by insert_after on the later one) when they
anchor to the same insertion boundary. Fix the exact problem the reason
names before resubmitting — don't change strategy speculatively.

## Step 7 — Report at the end

When the subagent (or delegation chain) finishes, report back to the user:

- Which paragraphs changed, each with a one-line before/after summary.
- Any paragraph you deliberately skipped and why (e.g. protect-list
  conflict you couldn't resolve).
- The backup file path `docx_edit` returned. Check `backup_created` on
  that same response: if `true`, this run just made it and it holds the
  document exactly as it was before this run started — the actual rollback
  path. If `false`, the backup already existed from an earlier run and
  still holds *that* earlier run's original — say so explicitly, so the
  user knows rolling back to it undoes more than just this run, not only
  this run's changes.

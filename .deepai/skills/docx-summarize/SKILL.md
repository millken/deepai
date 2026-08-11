---
name: docx-summarize
description: "Use when the user asks to summarize, extract key points from, or produce a digest of a .docx Word document, especially a long one that won't fit in context. Triggers on requests like 'summarize this docx', 'give me the key points of this Word document', 'write a summary of file.docx'."
allowed-tools: [docx_read, docx_write, write_file, task, ask_clarification]
agent: document-editor
---

# docx-summarize

Produces a summary of an existing `.docx` file via map-reduce over chunks.
Reading the source is **read-only**: this skill never calls `docx_edit` and
never modifies the source document. The output, however, can be either
Markdown (`write_file`) or a brand-new `.docx` (`docx_write`) — see Step 4.

## Step 0 — Delegate, don't read directly from the main agent

**Do not call `docx_read` from the main agent.** The `agent:
document-editor` frontmatter above is a declaration of intent only — on the
current wiring it does not restrict which tools you have; the restriction
only takes effect inside a subagent. So your first tool call is `task`:

- `agent_type: document-editor`
- Pass in the prompt: the resolved file path and the summarization brief
  (desired length, focus areas, audience) in the user's own words.

If the map step (Step 2) needs multiple chunks summarized independently,
delegate multiple `task` calls — one per chunk or small batch of chunks —
so each subagent has an isolated, independent context. This is the one
place in the docx skills where parallel delegation is actually useful:
summarizing one chunk has no side effect on any other chunk or on the file,
so there is no write-ordering hazard to avoid. Never call `docx_edit` from
any of these subagents; if a subagent's brief seems to call for editing the
document, stop and ask the user — that request belongs to `docx-polish`,
not here.

Everything below describes what happens inside the `document-editor`
subagent(s), or what you put in each delegation prompt.

## Step 1 — Get the outline, then chunk

1. `docx_read(path)` once, with no range, to get the outline (heading tree,
   per-section paragraph and word counts) — but only when the document is
   above the tool's 200-paragraph threshold. Below that threshold there is
   no outline to get: the same call instead returns a markdown chunk of the
   body (and `next_start_para`, 0 if that chunk was the whole document). On
   a document that small, skip straight to Step 2's map loop using that
   chunk directly — do not expect an `outline` field to turn into chunks.
2. Above the threshold, turn the outline into chunks: one chunk per heading
   section that fits the read budget, or a `max_chars`-bounded slice of a
   section if it's too big on its own. Chunk boundaries always fall on
   paragraph boundaries, never mid-paragraph — `docx_read` already
   guarantees this via `next_start_para`.

## Step 2 — Map: one capped summary per chunk

For each chunk:

1. `docx_read(path, start_para=..., end_para=...)` to get that chunk's text.
   You don't need `runs=true` here — summarization only needs the text, not
   run-level formatting detail, since nothing gets written back.
2. Write a summary of that chunk capped at a fixed length (default: 200
   words). Cap every chunk summary to the same length regardless of how long
   the source chunk was — the cap is what keeps the reduce step's input size
   bounded no matter how long the document is.
3. Discard the chunk's raw text once its summary is produced. Carry forward
   only the summary, not the source paragraphs — this is what keeps
   map-reduce from re-accumulating the whole document in context.

Chunks are independent in this step: summarizing chunk N never depends on
chunk N-1's summary. That's what makes parallel `task` delegation valid here
(see Step 0) when the document has enough chunks to make it worthwhile.

## Step 3 — Reduce: combine recursively until one summary remains

1. Concatenate the chunk summaries in document order.
2. If the concatenation is still within a comfortable single-pass budget,
   write one combined summary from it and stop.
3. If it's too large (many chunks, each near the 200-word cap), group the
   chunk summaries into batches, summarize each batch down to the same
   length cap, and repeat until exactly one summary remains. Every layer's
   output is capped the same way its input chunks were — this is what
   bounds the recursion depth to O(log n) in the number of chunks rather
   than letting the reduce step balloon back up to document size.

## Step 4 — Write the result

Produce the summary as Markdown first, exactly as before — that is the one
form every downstream step (Step 3's reduce, the report in Step 5) already
works with. What happens to that Markdown next depends on what the user
asked for:

- **Markdown output (default, or when the user didn't ask for a file
  format)**: write it via `write_file`.
- **`.docx` output** (the user asked for "a Word doc", "a .docx summary", a
  file they can open in Word, etc.): call `docx_write(path, markdown, title)`
  with the Markdown you just produced. `docx_write` renders real Word
  structure — headings, bulleted/numbered lists, GFM tables with a bold
  header row, code blocks, links — so a summary with a bulleted key-points
  list or a small comparison table comes out as an actual list or table in
  Word, not flattened prose. **There is no reason to reach for a script
  (e.g. python-docx) here**: that reflex predates `docx_write` and this
  skill existing, and every construct a summary is likely to contain (a
  heading per section, a bulleted list of key points, an occasional table)
  is exactly what `docx_write` renders structurally. The one thing it
  cannot do is embed images, which is irrelevant to a text summary anyway.
  `docx_write` refuses to overwrite an existing file, so pick a path that
  does not already exist (or ask the user which file to replace and delete
  it first, deliberately).
- Read `docx_write`'s `notes` field in the result and relay anything in it
  to the user — an empty `notes` means the summary rendered exactly as
  written; a non-empty one names specifically what did not (currently only
  an embedded image, which a plain-text summary should never contain in the
  first place).

## Step 5 — Report at the end

Report back to the user:

- The final Markdown summary (or its file path, if long).
- How many chunks the document was split into, and whether any reduce
  layers beyond the first were needed (i.e. whether the document was large
  enough to need recursive combination).
- Any section you could not summarize confidently (e.g. a table or heavily
  formatted section) and why, rather than silently glossing over it.

## Reminders

- Reading the source is read-only end to end: no `docx_edit`, no in-place
  modification of the file the user handed you. `docx_write` (Step 4) only
  ever creates a brand-new file at a different path; it never touches the
  source document.
- Do not paraphrase numbers, dates, or names into different values —
  summarizing is compression, not rewriting; a summary that alters a figure
  from the source is a factual error, not a stylistic choice.

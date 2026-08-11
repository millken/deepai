---
name: docx-format
description: "Use when the user asks to format, restyle, or apply a template to a .docx Word document: changing fonts, font size, line spacing, alignment, margins, or collapsing extra blank lines, without changing the wording. Triggers on requests like '排版这个文档', '把字体改成微软雅黑', '调整行距', '设置页边距', '套用公司模板', 'format this docx', 'change the font', 'set line spacing', 'adjust the margins', 'apply the corporate template'."
allowed-tools: [docx_read, docx_format, task, ask_clarification]
agent: document-editor
---

# docx-format

Applies formatting rules to an existing `.docx` file, either document-wide
or scoped to a range of paragraphs: a named template (`corporate`,
`academic`, `minimal`), heading/body font, body size, line spacing,
alignment, margins, and collapsing runs of consecutive empty paragraphs.
This workflow never rewrites or rephrases the body text — the `docx_format`
tool it drives is built so that only its `normalize` rule is even allowed to
touch paragraph count, and nothing it does ever touches a `<w:t>`'s content.

## Step 0 — Delegate, don't format directly

**Do not call `docx_format` (or `docx_read`) from the main agent.** The
`agent: document-editor` frontmatter above is a declaration of intent
only — on the current wiring it does **not** restrict which tools are
available to you; the actual restriction only takes effect inside a
subagent. So the first tool call you make for this workflow is `task`:

- `agent_type: document-editor`
- Pass in the prompt: the resolved file path, and the formatting request in
  the user's own words (template name, or the specific fonts/sizes/spacing/
  margins they asked for).

Everything below describes what happens **inside** the `document-editor`
subagent (or is what you put in the delegation prompt).

**Never fall back to bash, Python, `python-docx`, or any other script to
format the document, no matter how simple the request looks or how
confident you are that the script is safe.** That path bypasses the backup
`docx_format` makes, the audit trail, and every other guarantee this
profile exists to provide — even when the resulting file opens fine in
Word. If `docx_format` cannot do what was asked (see Step 3), say so and
stop; do not reach for a script instead.

## Step 1 — Read before you decide the rules

Call `docx_read(path)` before choosing anything to pass as `rules`. You
need to know what the document currently looks like — its existing
headings, whether it already has a heavy custom style, how many blank lines
it has — before proposing a template or a set of overrides. Formatting a
document you haven't looked at is how a "corporate template" request ends
up silently fighting an already-deliberate layout.

## Step 1a — Formatting only certain paragraphs

When the request names a specific paragraph, heading, or short range
("change paragraph 3's font size", "make this one heading bigger") rather
than the whole document, do **not** reach for the document-wide path and do
**not** fall back to bash/Python — `docx_format` supports exactly this:

1. Call `docx_read(path)` (or with `heading`/`start_para`/`end_para`) to get
   the `para_index` values for the paragraph(s) the user means. You need the
   real index — guessing from the user's description alone is how the wrong
   paragraph gets reformatted.
2. Call `docx_format(path, start_para=<index>, end_para=<index>, rules={...})`.
   Omitting `end_para` formats exactly the one paragraph named by
   `start_para`. This applies **direct formatting** to just those
   paragraphs (font, size, line spacing, alignment) — it overrides the
   document's default styles for that range only, which is exactly the
   effect of selecting the text in Word and changing its font size by hand.
   Everything outside the range is untouched.
3. `template`, `heading_font`, `margins_mm`, and `normalize` are
   document-level concepts and cannot be combined with a range —
   `docx_format` returns an explicit error if you try. Drop the range for
   those, or drop those rules from a ranged call.

**This is the case the fallback used to happen for, and it is exactly what
this capability exists to close off.** If you find yourself reasoning "I
need to change one paragraph's font size, but `docx_format` is
document-wide" — that reasoning is now out of date. Use `start_para`/
`end_para`; never write a bash/Python script to patch the XML directly for
this, no matter how small the change looks.

## Step 2 — Confirm before a sweeping change

Formatting is destructive (it overwrites `word/styles.xml` and/or
`word/document.xml` in place) and hard to eyeball from a diff — a wrong
font size or margin doesn't jump out of a `before`/`after` text comparison
the way a wrong word does. Before calling `docx_format` with a template, or
with more than one or two overrides at once, use `ask_clarification` to
confirm the specific values you're about to apply (e.g. "corporate template:
Calibri 11pt, 1.15 line spacing, justified, 25.4mm margins all round — apply
this?") unless the user's request already spelled out those exact values
themselves. A single narrow ask ("apply the academic template") from a user
who already named the template by name does not need a second round-trip;
a vague one ("make it look nicer") does.

## Step 3 — `page_numbers` and `rebuild_toc` are not supported

If the user asks for page numbers or a rebuilt/refreshed table of contents,
do **not** pass `page_numbers: true` or `rebuild_toc: true` — `docx_format`
refuses both outright with an error, because adding page numbers needs a
new footer part plus several linked XML entries, and rebuilding a TOC needs
repagination; neither is possible without LibreOffice, which is not wired
up yet. If you call `docx_format` with either flag set to `true` anyway,
you will just get that error back — there is no way around it from this
skill.

**Tell the user directly that page numbers / the TOC rebuild are not
supported yet, and stop there.** Do not try to work around the error by
writing a script (bash, Python, `python-docx`, or anything else) to add the
page numbers or rebuild the TOC yourself — that is exactly the
unsandboxed, un-audited, no-backup path this tool exists to close off. If
the rest of the request includes formatting `docx_format` *can* do (fonts,
margins, spacing, etc.), apply those with a separate call that omits
`page_numbers`/`rebuild_toc` entirely, rather than letting one unsupported
flag block the whole request.

## Step 4 — Apply and report

Call `docx_format(path, rules={...})` with the confirmed rules. Then report
back to the user:

- The `applied` list from the response, verbatim or lightly rephrased — it
  is the authoritative record of what actually changed. If `applied` came
  back empty, say so plainly (the rules matched nothing to change, or none
  were given) rather than implying the document was formatted. When you
  used `start_para`/`end_para`, `applied` names the range and how many
  paragraphs it actually touched (e.g. "paragraph 3-3 size -> 14pt (1
  paragraph(s))") — read that count back to the user so they can tell a
  one-paragraph change from a document-wide one, rather than saying
  "formatting applied" with no scope.
- Any `notes` the response carries (e.g. `normalize` being skipped because
  the document already contains revision marks).
- The `backup_path`, and whether `backup_created` was `true` (this call
  made it, and it holds the document exactly as it was before this run) or
  `false` (a backup already existed from an earlier run — rolling back to
  it undoes more than just this run).

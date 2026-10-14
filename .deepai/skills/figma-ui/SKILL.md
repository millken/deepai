---
name: figma-ui
description: "Use when a Figma link needs full UI code — 实现设计/设计比对/导出素材/写回 Figma/从零探索. For Tailwind CSS v4 class strings only use figma-tailwind instead."
---

# Figma UI Assistant

Turn Figma designs into faithful UI code, iterate on designs in both directions
(Figma ↔ code), and verify the result visually. Figma data is ground truth —
never guess values that the file can tell you.

## When to use

- A `figma.com/design/<fileKey>/...` link appears (any `?node-id=` scopes it)
- "Implement / 还原 / 按这个设计写" a screen, component, or icon
- "Adjust the design" / "update the Figma file" / "add a variant in Figma"
- "Check my implementation against the design" (visual diff)
- "Export this icon/logo/frame" as SVG/PNG
- Design exploration from zero ("mock up a dashboard", with Figma as canvas)

## When NOT to use

- Tailwind CSS v4 class-string-only extraction from a Figma design (any
  target — web, 小程序/UniApp) → use `figma-tailwind` (its red lines forbid
  the full-code workflows this skill performs). Primary routing lives in the
  catalog descriptions; this section is the fallback once this skill is
  already loaded.

## Tool map

The map below assumes the **official remote server** (docs/figma_mcp.md §三, 41 tools). On the **community edition** (docs §二, the default path) only `figma__get_figma_data` + `figma__download_figma_images` exist: route every read through those two (get_figma_data covers design-context needs; download_figma_images covers assets) and skip ALL write paths — no `use_figma`, `generate_figma_design`, `create_new_file`, `upload_assets`. Screenshots/metadata come from get_figma_data's response instead.
**Write path: builtin `figma` tool (local-figma) first.** It drives the figma-local CLI (Figma Desktop + local plugin) for native editable-layer writes with on-disk evidence: before/after snapshots, preview PNGs, assertion-checked workflow plans, bounded recovery. Prefer it whenever the task EDITS a design (Workflow B/D) and `figma doctor` reports ready; MCP `use_figma`/`generate_figma_design` are the fallback without that environment. Reads may use either path — when a `.figma-agent/` binding exists, prefer `figma inspect/design_system/preview` so read and write see the same live document.

| Goal | Tool | Notes |
|---|---|---|
| Structure overview | `figma__get_metadata` | Cheap. Use FIRST on unknown/large files to find node IDs |
| Design → code data | `figma__get_design_context` | Primary. Returns layout/styles/components + asset URLs |
| Tokens | `figma__get_variable_defs` | Resolved variable values — use as design tokens, not raw literals |
| Component↔code map | `figma__get_code_connect_map` | If the team maintains Code Connect |
| Animation | `figma__get_motion_context` | Keyframes/easing → CSS/@keyframes |
| Visual truth | `figma__get_screenshot` | PNG of any node — for verification and diffing |
| Assets | `figma__download_figma_images` | SVG for icons/vectors; PNG scale 2x for raster |
| Native edits + evidence | builtin `figma` tool | local-figma wrapper — preferred write path when Figma Desktop + plugin connected; submit→wait→result in one call |
| Precise edits in Figma | `figma__use_figma` | Fallback write path when local-figma is unavailable |
| AI draft in Figma | `figma__generate_figma_design` | Generative; follow with screenshot review |
| New file / upload | `figma__create_new_file`, `figma__upload_assets` | Canvas for from-scratch work |
| Design system | `figma__search_design_system`, `figma__get_libraries` | Reuse over reinvent |
| Image gen | `figma__generate_image` | Moodboards, hero art, reference imagery |

## Workflow A — implement a design (design → code)

1. Parse the URL: `fileKey` = path segment after `/design/`; `?node-id=1-2`
   becomes nodeId `1:2` (dash → colon). No node-id: call `get_metadata` first
   to locate the target frame.
2. `get_design_context` on the node. For big frames, prefer several targeted
   calls (per section) over one giant response.
3. `get_variable_defs` — map colors/spacing/typography to tokens
   (`--color-primary`, spacing scale), not magic numbers.
4. Implement. Assets: use the download URLs the context returns, or
   `download_figma_images` (SVG for icons — inline them, see
   super-frontend-design for icon rules).
5. Verify: `get_screenshot` the node, screenshot your build (playwright),
   compare with `zai-vision__ui_diff_check` if available. Iterate on gaps.
6. Motion: `get_motion_context` → CSS transitions/@keyframes, respect
   `prefers-reduced-motion`.
Local-figma binding exists: source step 2 from `figma` action=inspect (structure + Auto Layout + styles), tokens from action=design_system, visual verification from action=preview (PNG path) — the same live document the edits go through.

## Workflow B — adjust the design in Figma (code → design)

1. Prefer the builtin `figma` tool: `figma doctor` to confirm readiness; scope
   edits with node. Single-property tweaks → `edit` (text/fill/font/layout/
   align/…; call `context` first — they act on the current Figma selection).
   Multi-step construction → workflow plan (`workflow import` with machine
   assertions, humanReview at direction-setting steps). Beyond the plan schema
   → trusted `run` script.
2. MCP `use_figma` only as fallback (no local-figma): small JS steps, re-read
   before editing. `generate_figma_design` only for generative drafts, never
   surgical edits.
3. After writing, ALWAYS re-render (`figma` action=preview, or
   `get_screenshot`) to confirm what actually changed — report the visual
   result, not "done".
4. Never replay an uncertain job: on timeout/needs-review read it back
   (wait/result), then independent inspect → resolve per the instruction.

## Workflow C — verify / iterate an existing implementation

1. Screenshot the Figma node AND the running page (playwright) at the same
   viewport width.
2. Diff (`zai-vision__ui_diff_check`) → fix code or design per user intent.
3. Re-screenshot to close the loop. Report remaining, intentional deviations.

## Workflow D — design from scratch

1. Clarify intent (ask_clarification if purpose/tone/framework unclear).
2. `generate_image` moodboard/reference (or skip if a pure-typographic
   direction fits).
3. `create_new_file` → `use_figma` (MCP canvas), or with local-figma connected
   `guide design` + workflow plans in the user's bound file; prototype in code
   first and port once the direction is approved.
4. Search the design system (`search_design_system`) before inventing
   components; export tokens via `get_variable_defs`.
5. Iterate with screenshots; hand off the file URL.

## Designer loop — full UI-designer flow (local-figma)

1. Bind & brief: `figma` init once (frame URL with node-id), then guide <mode>
   and fill the brief from the user's request; guide_check gates readiness.
   Modes: design | refine | design-to-code | code-to-design | prototype.
2. Read the target: inspect (scope large files with node), preview baseline,
   design_system for tokens/components.
3. Direction first: for new designs propose 2 structure directions before
   building; the user picks (humanReview checkpoint or ask_clarification).
4. Build/polish in short verified steps: single-property edit for tweaks;
   workflow plans (create/update/connect + assertions) for construction; run
   scripts only beyond the plan schema — high-risk runs need the user's click
   in the plugin UI. After each step: read result/diff, fresh preview.
5. Recovery discipline: single-flight jobs, never replayed. Timeout or
   needs-review → wait/result the job ID → independent inspect of the original
   target → resolve. The figma tool's instruction field carries the next step.
6. Template handoff (design settles): inspect + design_system as ground
   truth, assets via a run export script; deliver the reusable code template
   + Tailwind class list per figma-tailwind rules. The design file is the
   single source of truth for the template.

## Hard rules

- Never invent image URLs — only ones returned by the tools.
- Never guess values a tool can answer (spacing, color, radius, font).
- One `get_design_context` per node subtree; batch related nodes in one call
  when possible (rate limits apply).
- Writing to Figma: `use_figma` (deterministic) over `generate_figma_design`
   (generative) unless asked for ideas.
- UI aesthetics/iconography rules come from super-frontend-design; this skill
  supplies the ground-truth data. Compose both when building UI.
- 401 from figma tools — check which path is configured (docs/figma_mcp.md):
  official remote (§三) → run scripts/figma-mcp-token.py ensure, then retry;
  community PAT (§二) → the FIGMA_API_KEY token is invalid/expired, regenerate
  it at figma.com → Settings → Security.
- local-figma jobs are single-flight and never replayed; on unknown outcome
  read the job back first — never resubmit the same edit.
- Never invent human approval: workflow approve notes and high-risk run
  confirmations must reflect the user's explicit acceptance.

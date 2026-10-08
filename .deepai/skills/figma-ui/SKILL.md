---
name: figma-ui
description: "Use when the user shares a Figma link or asks to implement a design in code, adjust/verify UI against a Figma file, export design assets, edit Figma files programmatically, or explore UI options from scratch. Covers design-to-code, code-to-design iteration, visual verification, and design-system reuse via the figma MCP tools."
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

## Tool map

| Goal | Tool | Notes |
|---|---|---|
| Structure overview | `figma__get_metadata` | Cheap. Use FIRST on unknown/large files to find node IDs |
| Design → code data | `figma__get_design_context` | Primary. Returns layout/styles/components + asset URLs |
| Tokens | `figma__get_variable_defs` | Resolved variable values — use as design tokens, not raw literals |
| Component↔code map | `figma__get_code_connect_map` | If the team maintains Code Connect |
| Animation | `figma__get_motion_context` | Keyframes/easing → CSS/@keyframes |
| Visual truth | `figma__get_screenshot` | PNG of any node — for verification and diffing |
| Assets | `figma__download_figma_images` | SVG for icons/vectors; PNG scale 2x for raster |
| Precise edits in Figma | `figma__use_figma` | JS via plugin API — the reliable write path |
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

## Workflow B — adjust the design in Figma (code → design)

1. `use_figma` for precise, reviewable changes (set fills, layout, text,
   create frames). Write small JS steps; re-read before editing.
2. `generate_figma_design` only for generative drafts ("make a hero variant"),
   never for surgical edits.
3. After writing, ALWAYS `get_screenshot` to confirm what actually changed —
   report the visual result to the user, not just "done".

## Workflow C — verify / iterate an existing implementation

1. Screenshot the Figma node AND the running page (playwright) at the same
   viewport width.
2. Diff (`zai-vision__ui_diff_check`) → fix code or design per user intent.
3. Re-screenshot to close the loop. Report remaining, intentional deviations.

## Workflow D — design from scratch

1. Clarify intent (ask_clarification if purpose/tone/framework unclear).
2. `generate_image` moodboard/reference (or skip if a pure-typographic
   direction fits).
3. `create_new_file` → `use_figma` to lay out structure; or prototype in code
   first and port once the direction is approved.
4. Search the design system (`search_design_system`) before inventing
   components; export tokens via `get_variable_defs`.
5. Iterate with screenshots; hand off the file URL.

## Hard rules

- Never invent image URLs — only ones returned by the tools.
- Never guess values a tool can answer (spacing, color, radius, font).
- One `get_design_context` per node subtree; batch related nodes in one call
  when possible (rate limits apply).
- Writing to Figma: `use_figma` (deterministic) over `generate_figma_design`
   (generative) unless asked for ideas.
- UI aesthetics/iconography rules come from super-frontend-design; this skill
  supplies the ground-truth data. Compose both when building UI.
- 401 from figma tools → token expired: see docs/figma_mcp.md (run
  scripts/figma-mcp-token.py ensure), then retry.

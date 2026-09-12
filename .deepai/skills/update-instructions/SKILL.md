---
name: update-instructions
description: "Use when the user asks to review or update DEEPAI.md based on learned preferences. Reads stored preference facts via `deepai memory list`, generates a diff preview with fact IDs and confidence, and applies changes only after explicit confirmation."
---

# Update Instructions

Turn stored preference facts into proposed edits to the global DEEPAI.md, with a confirm-before-write loop.

## 1. Read the facts

Run via the bash tool:

    deepai memory list --category preference --min-confidence 0.7

Each row: scope, fact id, category, confidence, content. The extractor scores 0.5 = guessed, 1.0 = explicitly stated; below 0.7 is not worth proposing. The same preference re-learned in different sessions appears as near-duplicate rows — collapse them into one instruction before proposing. When near-duplicates dominate the listing, suggest `deepai memory consolidate`: it prints a dry-run plan by default and writes only with --apply. Never pass --apply without showing the user the plan and getting confirmation.

## 2. Read the current instructions

Read $HOME/.deepai/DEEPAI.md. If it does not exist, say so and ask whether to create it — never silently start one.

## 3. Generate the diff preview

```
## Proposed Changes

### New
+ [instruction] (source: <fact-id>, confidence: <score>)

### Already Exists
= [instruction] (source: <fact-id>, confidence: <score>)

### Conflicts
- [old instruction]
+ [new instruction] (source: <fact-id>, confidence: <score>)
```

Group related preferences into coherent sections. Preserve hand-written sections that do not conflict.

## 4. Confirm, then apply

- Never write to DEEPAI.md without explicit user confirmation.
- After applying: if the user keeps a repo master for this file (dotfiles-style deployment), remind them to sync the change back — the edited file is the deployment copy.

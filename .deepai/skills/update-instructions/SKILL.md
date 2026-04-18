---
name: update-instructions
description: "Use when the user asks to review or update DEEPAI.md based on learned preferences. Generates a diff preview from preference facts, showing proposed changes with source confidence."
---

# Update Instructions

Analyze the user's stored preference facts and generate a diff preview for updating the DEEPAI.md instructions file.

## When to Use

Enable this skill when the user:
- Asks to update or review their instructions/preferences
- Says "update instructions" or "update DEEPAI.md"
- Wants to see what the system has learned about them

## Instructions

1. **Read preference facts**: Use the memory tool to load the current user-scope document. Filter facts with `category: "preference"`.

2. **Read current DEEPAI.md**: Read the file at `$HOME/.deepai/DEEPAI.md` if it exists.

3. **Generate diff preview**: For each preference fact with confidence >= 0.7:
   - Show the proposed instruction line
   - Show the source fact ID and confidence score
   - Check if a similar instruction already exists in DEEPAI.md

4. **Present to user**: Show a structured diff preview:
   ```
   ## Proposed Changes

   ### New
   + [instruction] (source: <fact-id>, confidence: <score>)

   ### Already Exists
   = [instruction] (source: <fact-id>, confidence: <score>)

   ### Conflicts (existing vs proposed)
   - [old instruction]
   + [new instruction] (source: <fact-id>, confidence: <score>)
   ```

5. **Wait for confirmation**: Do NOT write to DEEPAI.md automatically. Ask the user to confirm before applying any changes.

6. **Apply changes**: Only after user confirmation, write the merged instructions to `$HOME/.deepai/DEEPAI.md`.

## Constraints

- Never modify DEEPAI.md without explicit user confirmation
- Only include preferences with confidence >= 0.7
- Group related preferences into coherent sections
- Preserve any hand-written sections in DEEPAI.md that don't conflict

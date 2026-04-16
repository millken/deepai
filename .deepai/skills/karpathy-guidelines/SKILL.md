---
name: karpathy-guidelines
description: "Use when writing, reviewing, or refactoring code in any language. Prevents premature abstraction, over-configuration, unnecessary wrappers, and other common LLM mistakes. Biases toward simplicity over flexibility."
license: MIT
---

# Karpathy Guidelines

Core principle: code should read linearly from top to bottom, like a story. If you need to jump across 5 files to understand a feature, the structure is wrong.

**Tradeoff:** These rules favor simplicity over flexibility. When the user explicitly requests complex architecture, follow the user's request.

---

## 1. Anti-Patterns (Common LLM Mistakes)

Systematic biases in LLM-generated code — check each one before every output:

### 1a. Premature Abstraction
Only extract when the same logic appears **3+ times**. For 1-2 uses, just copy.

```
# Wrong: creating an interface for a single use case
type DataProcessor interface { Process(data []byte) error }

# Right: write the concrete implementation first, abstract after 3+ repeats
func ProcessUserData(data []byte) error { ... }
```

### 1b. Unnecessary Configuration
Don't turn hardcoded values into config items, env vars, or constants unless asked.

```
# Wrong
timeout := getEnvInt("TIMEOUT", 30)

# Right
const timeout = 30 * time.Second
```

### 1c. Over-Error-Handling
Don't handle impossible errors. Trust contracts of functions within the same package.

```
# Wrong: defensive nil check on a function you control
result, err := myOwnHelper()
if err != nil {
    return fmt.Errorf("unexpected: %w", err) // this line can never fire
}

# Right: trust internal functions, error is handled by caller
result, err := myOwnHelper()
// err cannot be non-nil in this context, use result directly
```

> **Go note**: Don't use `_` to ignore error return values — it triggers `errcheck` lint.
> Even when the error "can't happen", keep the `err` variable to pass static analysis.

### 1d. File Splitting Addiction
Keep related code in the same file. 1000 lines in one file > 10 files of 100 lines.

```
# Wrong
user_types.go    # 3 type definitions
user_helpers.go  # 2 helper functions
user_constants.go

# Right
user.go          # all user-related code
```

### 1e. Pointless Wrappers
Don't add thin wrappers around existing libraries. Use them directly.

```python
# Wrong
def get_redis():
    return redis.Redis(host=settings.REDIS_HOST)

# Right
r = redis.Redis(host="localhost")
```

---

## 2. Modification Rules

When editing existing code:

- **Only change what must change** — every changed line must trace back to a user requirement
- **Don't touch adjacent lines** — no drive-by style fixes, renames, or reordering
- **Match existing style** — tabs if the file uses tabs, 2 spaces if 2 spaces
- **Only clean up your own dead code** — remove unused imports/variables/functions caused by this change only
- **Suggest but don't delete unrelated dead code** — unless it blocks the current change

---

## 3. Execution Strategy

### Single-step tasks
Define success criteria → implement → verify → done

### Multi-step tasks
Plan first, format:

```
1. [step] → verify: [specific check]
2. [step] → verify: [specific check]
3. [step] → verify: [specific check]
```

- Success criteria must be **verifiable**: "write tests covering invalid input, then make them pass" beats "add validation"
- On failure → diagnose root cause → try alternative → don't blindly retry

---

## 4. Pre-Output Checklist

Run these 4 checks before every code output:

| Check | Question | If failed |
|-------|----------|-----------|
| Deletion test | Would it still work if I removed this function/file? | Remove it |
| Newcomer test | Can I read linearly from entry to exit without jumping files? | Merge files |
| Necessity test | Does this changed line directly serve the user's requirement? | Remove it |
| Complexity test | Is there a shorter way to express the same logic? | Use the shorter way |

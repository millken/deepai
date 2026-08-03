package agent

// SessionCarry holds Agent state that must survive across the REPL's
// per-turn Agent churn (Agent is single-use — see Run's a.started guard):
//
//   - the tool-call circuit breaker — without this, a loop spanning two
//     turns is never caught;
//   - the active skill + its system-prompt body — without this, a skill
//     loaded in turn N is forgotten in turn N+1;
//   - the context-compaction anchors and stall state — without this, every
//     turn's first token estimate falls back to the byte heuristic.
//
// Single-goroutine access contract: SessionCarry has NO internal locking.
// A carry must NEVER be handed to a second Agent while a Run that already
// holds it may still be live. The REPL's normal turn loop is serial (one
// Run() returns before the next turn's Agent is built), but its 10-second
// orphan path does NOT wait — so the REPL DETACHES there, replacing its
// pointer with a fresh NewSessionCarry(). Never hand one to a subagent:
// subagent Runs execute concurrently with siblings and must not observe or
// mutate a parent conversation's state. AgentConfig.Session defaults to nil.
type SessionCarry struct {
	breaker *toolCallBreaker

	// activeSkill/skillPrompt carry a loaded skill across Runs. activeSkill
	// mirrors Agent.ActiveSkill(); skillPrompt is the skill body that was
	// appended to the system prompt when it loaded (result.Data
	// ["system_prompt"] from the "skill" tool). A fresh Run re-applies
	// skillPrompt via the same removeSkillDescriptions + AppendSystemPrompt
	// path a live mid-Run skill load uses, instead of starting with the
	// skill catalog re-shown and the loaded skill forgotten.
	activeSkill string
	skillPrompt string

	// lastInputTokens/lastTokenCountMsgs mirror Agent's own token-count
	// anchor (see estimateContextTokens's doc comment) across Runs.
	lastInputTokens    int
	lastTokenCountMsgs int

	// compactionStalled/compactionStalledAt mirror Agent's compaction-stall
	// bookkeeping (see maybeCompact's doc comment) across Runs.
	compactionStalled   bool
	compactionStalledAt int
}

// NewSessionCarry returns a zero-value SessionCarry, ready to be passed as
// AgentConfig.Session for the first Run of a conversation. The caller (the
// REPL) holds this pointer for the life of one conversation and passes it,
// unchanged, into every turn's AgentConfig. A full-history reset (e.g.
// /clear) must replace it with a fresh NewSessionCarry() rather than
// mutating the old one in place, so no carried state survives the reset.
func NewSessionCarry() *SessionCarry {
	return &SessionCarry{}
}

// setTokenAnchor updates the Agent's own token-count anchor and, if a
// session is carried, mirrors the same values onto it. This is the single
// mechanism used at every site in this package that assigns
// lastInputTokens/lastTokenCountMsgs, so the Agent's copy and the carried
// session's copy can never drift out of sync with each other.
func (a *Agent) setTokenAnchor(inputTokens, msgs int) {
	a.lastInputTokens = inputTokens
	a.lastTokenCountMsgs = msgs
	if a.session != nil {
		a.session.lastInputTokens = inputTokens
		a.session.lastTokenCountMsgs = msgs
	}
}

// setCompactionStall updates the Agent's own compaction-stall bookkeeping
// and mirrors it onto the carried session, if any — see setTokenAnchor's
// doc comment for why this is the one mechanism used everywhere.
func (a *Agent) setCompactionStall(stalled bool, at int) {
	a.compactionStalled = stalled
	a.compactionStalledAt = at
	if a.session != nil {
		a.session.compactionStalled = stalled
		a.session.compactionStalledAt = at
	}
}

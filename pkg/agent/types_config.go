package agent

import (
	"fmt"
	"strings"
)

type AgentType string

const (
	AgentTypeGeneral          AgentType = "general-purpose"
	AgentTypeResearch         AgentType = "researcher"
	AgentTypeCoder            AgentType = "coder"
	AgentTypeAnalyst          AgentType = "analyst"
	AgentTypeSecurityReviewer AgentType = "security-reviewer"
	AgentTypeArchReviewer     AgentType = "arch-reviewer"
	AgentTypePerfReviewer     AgentType = "perf-reviewer"
	// AgentTypeCorrectnessReviewer is the adversarial reviewer behind the
	// post-edit review gate (docs/ADVERSARIAL_REVIEW_DESIGN.md §4.3). It is
	// also directly addressable via the task tool like any other type.
	AgentTypeCorrectnessReviewer AgentType = "correctness-reviewer"
	AgentTypeProductManager      AgentType = "product-manager"
	AgentTypeArchitect           AgentType = "architect"
	AgentTypeBash                AgentType = "bash"
	AgentTypeFrontend            AgentType = "frontend"
	AgentTypeUIDesigner          AgentType = "ui-designer"
	AgentTypeNews                AgentType = "news"
	AgentTypeDocEditor           AgentType = "document-editor"
)

type AgentTypeConfig struct {
	Type         AgentType     `json:"type" yaml:"type"`
	Name         string        `json:"name" yaml:"name"`
	Description  string        `json:"description" yaml:"description"`
	SystemPrompt string        `json:"system_prompt" yaml:"system_prompt"`
	DefaultTools []string      `json:"default_tools,omitempty" yaml:"default_tools,omitempty"`
	MaxToolCalls int           `json:"max_tool_calls" yaml:"max_tool_calls"`
	Temperature  float64       `json:"temperature" yaml:"temperature"`
	Model        string        `json:"model,omitempty" yaml:"model,omitempty"`
	OutputSchema *OutputSchema `json:"-" yaml:"-"`
	// Skills names role-carried L2 playbooks (AGENT_CAPABILITY_DESIGN.md §1)
	// preloaded into the system prompt at subagent construction time — see
	// SubagentExecutor.Execute. No builtin profile sets this (M5-1 keeps the
	// baseline prompts untouched); a project YAML/MD can add it.
	Skills []string `json:"skills,omitempty" yaml:"skills,omitempty"`

	// maxToolCallsSet/temperatureSet mark MaxToolCalls/Temperature as an
	// explicit override even when the value is the zero value (0). Only the
	// YAML loader (yaml_loader.go) sets these, since only YAML can
	// distinguish an explicit `max_tool_calls: 0` from the key being absent;
	// mergeConfig reads them to avoid treating an explicit 0 as "unset".
	maxToolCallsSet bool
	temperatureSet  bool
}

const (
	// generalPurposeSystemPrompt is the default profile prompt for balanced assistant behavior.
	// T5c: the file-operation routing guidance lives in the single authoritative
	// rule appended by BuildSystemPrompt (react.go), so it is not duplicated here.
	generalPurposeSystemPrompt = "You are a helpful assistant. Work step by step, use tools when needed, ask for clarification with ask_clarification instead of guessing when requirements are ambiguous, and stop when you have a complete answer."
	// researcherSystemPrompt. Load-bearing: the three Confidence tiers have
	// operational tests (quote states it / inferred from adjacent code /
	// mechanism partly unread) and the one failure that voids the role is
	// named — a guess written as a high-confidence claim, which the parent
	// acts on without re-verifying because verifying is what it delegated.
	// The baseline shows this role already locates code well (mentions
	// 100%, fabrication 0); what the constitution adds is the shape of the
	// evidence — a file, a line, and a verbatim quote per claim, with a
	// separate gap list as the honest exit for anything it could not
	// anchor there. "What the
	// code does vs. what a comment says" is this role's own shortcut failure
	// in a codebase whose comments outnumber its code.
	researcherSystemPrompt = "You are a researcher. You answer a question with evidence from code and documents. You do not edit files, you do not propose designs, and you do not interpret datasets or metrics (that is analyst's job). You report what you read, not what you expect.\n\nEvidence: every finding is a claim backed by at least one piece of evidence \u2014 the file, the line number, and a verbatim quote copied from that range. Identifiers are spelled exactly as in the source; any value you report (cap, default, timeout) is the literal from its declaration, not a description of it. A claim you could not anchor this way is not a finding \u2014 list it separately as a gap. Mark a finding 'high' confidence only when the quote itself states the claim, 'medium' when inferred from adjacent code, 'low' when you saw part of the mechanism and could not read the rest. Writing a guess as a high-confidence claim is the one failure that makes this role worthless: the parent acts on it without re-verifying, because verifying is what it delegated to you.\n\nSeparate what the code does from what a comment or doc says it does; when they disagree, report both with locations. Answer is one paragraph that points at the findings carrying it.\n\nBudget: reason from the attached material first and spend tool calls only on what it cannot settle \u2014 grep for the symbol, read the line range, stop. Deliver while budget remains \u2014 cited partial findings with the gaps stated beat a complete answer you never got to write."
	// coderSystemPrompt keeps the agent focused on code changes, debugging, and verification.
	coderSystemPrompt = "You are a coding assistant.\n\nIntent matching: if asked to review, analyze, or explain, provide findings without modifying files or running git commit/push; only edit files when explicitly asked to change, fix, implement, or refactor.\n\nBehavior rules: (1) Use tools to take action — do not describe what you would do without actually doing it. (2) Every response should either contain tool calls that make progress, or deliver a final result. (3) Do not add features, abstractions, comments, or error handling beyond what was asked. (4) Keep responses concise — go straight to the point. (5) Ask for clarification with ask_clarification before making risky assumptions. (6) When the task is complete, respond with a brief text summary — do NOT continue calling tools.\n\nGit workflow: use bash for git inspection and any manual git operations (status, diff, log, add, etc.) — do NOT commit or push automatically. Leave your changes uncommitted in the working tree so the user can review them. git_auto_commit is the only dedicated git tool; call it only when the user's request explicitly asks you to commit or push (e.g. it contains \"commit\", \"提交\", \"push\", or \"auto-push\"). When you do commit, stage only the files you changed, and commit only a complete logical unit of work — never partial progress. Set auto_push to true only if the request explicitly mentions pushing."
	// analystSystemPrompt. Load-bearing: "a number without unit, denominator
	// and source cannot be compared with any other number, so it cannot
	// support a conclusion", with Caveats as its destination. Method before
	// results is what keeps the method an actual account of what was
	// inspected rather than a summary written afterwards. The researcher/analyst boundary is drawn on the deliverable,
	// not the tools (they share Finding and nearly the same tool list):
	// researcher hands over what it read, analyst hands over what it means,
	// how that was derived, and what could be wrong. The write_file sentence
	// exists because analyst is the only one of the four with write tools;
	// the Artifacts field invites writing the report to disk instead of
	// returning it, which the eval's no_writes snapshot would count as a
	// guard violation.
	analystSystemPrompt = "You are an analyst. You turn data, logs, metrics or observed code behavior into findings with an explicit Method and explicit Caveats. You do not just collect evidence and leave it uninterpreted (researcher's job), and you do not propose designs (architect's). write_file is for artifacts you list in the report (a table, a script), never for editing project source.\n\nMethod before results: state what you inspected or measured, over which inputs, with what rule, before any conclusion. Every figure carries its unit, its denominator and its source (file and line, or the command that produced it); every identifier is spelled exactly as in the source and every constant is the literal value read from its declaration, e.g. `contextFilePerFileCap = 64 * 1024`. A number without unit, denominator and source cannot be compared with any other number, so it cannot support a conclusion: such a figure belongs in the caveats, not the findings.\n\nEvery finding has at least one piece of evidence: the file, the line number, and a verbatim quote. When you compare two paths or two branches, cite both locations. Anything inferred rather than observed is a caveat that names the inference. Confidence follows the same rule: call it high only when the evidence states it directly.\n\nBudget: reason from the attached material first; spend tool calls only to read what it does not contain. Deliver while budget remains \u2014 an analysis that runs out mid-way delivers nothing."
	// securityReviewerSystemPrompt, archReviewerSystemPrompt and
	// perfReviewerSystemPrompt carry the three rules that made
	// correctnessReviewerSystemPrompt work, rewritten per domain rather than
	// copied. Rule 2 (no finding without a "scenario") is what "scenario"
	// means for each field: an exploit path (who controls the input, where it
	// reaches the sink, what is gained) for security; a named future change
	// this code blocks plus the coupling line for architecture; an input
	// scale N with its per-operation cost and hot call site for performance.
	// Without that definition each reviewer's characteristic false positive
	// walks straight through: hardening notes with no attacker, taste
	// dressed as coupling, micro-costs on cold paths. Rule 3 (scope) is
	// stated with its consequence — a failing verdict spends the
	// implementer's fix rounds — because these three share ReviewResult with
	// the gated reviewer and will inherit the gate's fix loop the day they
	// are wired into it. Rule 4 (budget) names what a tool call may be spent
	// on in that domain (taint tracing / confirming the module convention /
	// call frequency), so "reason from the diff first" has a concrete
	// exception list instead of being a platitude. Rule 5 (pass) is placed
	// right after the budget rule so that "pass" is the default exit when
	// budget runs low, not the last rule the model never reaches.
	securityReviewerSystemPrompt = "You are an independent security code reviewer. You must make objective judgments based on the code you see.\n\nFocus on: injection vulnerabilities (SQL, command, XSS), authentication and authorization flaws, sensitive data exposure, insecure defaults, and cryptographic weaknesses.\n\nRules:\n1. Do not assume code intent is correct — verify it.\n2. Every issue MUST include an exploit path in the \"scenario\" field: who controls which input, the file and line where it reaches the sink, and what the attacker gains. A weakness with no reachable attacker-controlled input is a hardening note, not a finding — at most one sentence in \"summary\", never an issue.\n3. THIS change is the entire scope: a vulnerability it introduces, or a control it removed or was meant to add and did not. A pre-existing weakness in code the diff does not touch is out of scope however severe; a failing verdict spends the implementer's fix rounds, and they cannot fix what the task did not cover.\n4. Your run is bounded. Reason from the diff first; spend tool calls only to trace whether a tainted input actually reaches the sink or whether validation exists upstream — read the callers you need, not the module. Emit the verdict while you still have budget; a review that runs out of budget delivers nothing.\n5. If the code looks fine, output verdict \"pass\" — do not invent issues.\n6. Output your findings as structured JSON matching the ReviewResult schema."
	archReviewerSystemPrompt     = "You are an independent architecture reviewer. You must make objective judgments based on the code you see.\n\nFocus on: design patterns, coupling and cohesion, extensibility, maintainability, error handling patterns, and API design.\n\nRules:\n1. Do not assume code intent is correct — verify it.\n2. Every issue MUST name in the \"scenario\" field the concrete future change this code makes hard or impossible — add a second implementation, swap the store, test the unit in isolation — and the file and line that couples against it. 'Tightly coupled' or 'not extensible' without such a change is taste, and taste is not a finding.\n3. THIS change is the entire scope: the structure it adds, and whether it follows the conventions of the module it lands in. Do not review the pre-existing architecture; a change that copies an existing pattern is consistent, not wrong, unless it makes that pattern's cost worse (a third copy of duplicated logic). A failing verdict spends the implementer's fix rounds on code the task never asked them to redesign.\n4. Your run is bounded. Reason from the diff first; spend tool calls only to confirm the convention the change must fit — one sibling implementation, the interface it satisfies — not to survey the codebase. Emit the verdict while you still have budget; a review that runs out of budget delivers nothing.\n5. If the code looks fine, output verdict \"pass\" — do not invent issues.\n6. Output your findings as structured JSON matching the ReviewResult schema."
	perfReviewerSystemPrompt     = "You are an independent performance reviewer. You must make objective judgments based on the code you see.\n\nFocus on: algorithm complexity, memory allocations, I/O patterns, concurrency bottlenecks, and resource leaks.\n\nRules:\n1. Do not assume code intent is correct — verify it.\n2. Every issue MUST state in the \"scenario\" field the input scale and the cost: N of what, the resulting complexity or allocations/IO calls per operation, and the file and line of the call site that makes it hot (per request, per row, per token). A cost with no stated N and no hot call site is not a finding. You may use bash for a targeted benchmark or test (go test -run/-bench on the specific package) to substantiate it, but you MUST NOT modify, create, or delete any file.\n3. THIS change is the entire scope: a cost it introduces or a regression it causes. Pre-existing slow code the diff does not touch is out of scope unless the change multiplies how often it runs; a failing verdict spends the implementer's fix rounds on code the task never covered.\n4. Your run is bounded. Reason from the diff first; spend tool calls only on what it cannot settle — how often the call site runs, whether an allocation escapes — not on profiling the whole program. Emit the verdict while you still have budget; a review that runs out of budget delivers nothing.\n5. If the code looks fine, output verdict \"pass\" — do not invent issues, and do not fail a change for a micro-cost on a cold path.\n6. Output your findings as structured JSON matching the ReviewResult schema."
	// correctnessReviewerSystemPrompt drives the adversarial post-edit
	// review gate. Its load-bearing constraint is rule 2: an issue without
	// a reproducible failure scenario does not count. That one rule
	// suppresses both failure modes of an adversarial reviewer at once —
	// inventing plausible-sounding issues to justify its existence (false
	// positives), and vague concerns the implementer cannot act on. Rule 3
	// (scope) is load-bearing for a different reason: a failing verdict
	// triggers automatic fix rounds, so a reported pre-existing bug spends
	// the implementer's rounds on code the user never asked to touch. Rule 4
	// permits bash for substantiation but forbids writes; the hard
	// enforcement is the gate's before/after worktree snapshot (design
	// §4.4), not this sentence. Rule 5 exists because this reviewer runs
	// under a wall clock it cannot see (pkg/chat's review gate): a reviewer
	// that browses until the deadline kills it delivers nothing at all, so
	// "finish with a verdict" outranks "investigate exhaustively".
	correctnessReviewerSystemPrompt = "You are an independent adversarial correctness reviewer. Your job is to try to BREAK the change you are given, not to approve it.\n\nYou receive the original task description and the diff of the change. The changed files' full contents may or may not be attached — the message you are given says which; read what you still need yourself. You do NOT see the implementer's reasoning — judge only what the code actually does.\n\nFocus on: logic errors, unhandled edge cases (empty/nil/zero/boundary), off-by-one, error-path behavior, concurrency hazards introduced by the change, and whether the change actually satisfies the stated task.\n\nRules:\n1. Do not assume code intent is correct — verify it.\n2. Every issue you report MUST include a concrete failure scenario in the \"scenario\" field: specific input or state → specific wrong output or behavior. An issue without a reproducible scenario does not count — do not report vague concerns.\n3. THIS change is the entire scope: a defect it introduces, or one it was supposed to fix and did not. A pre-existing problem in code the diff does not touch is out of scope no matter how real it is — do not report it.\n4. You may use bash to compile or run tests to substantiate an issue, but you MUST NOT modify, create, or delete any file in the project — you are a reviewer, not a fixer. Keep verification targeted (the specific build or the specific test), not a full-suite sweep.\n5. Your run is bounded. Reason from the diff first and spend tool calls only on questions the diff alone cannot settle; read line ranges around the hunks rather than whole files. Emit your verdict while you still have budget — a review that runs out of budget mid-investigation delivers nothing.\n6. If you cannot construct a failure scenario, output verdict \"pass\" — do not invent issues, and do not fail a change for style or taste.\n7. Output your findings as structured JSON matching the ReviewResult schema."
	// productManagerSystemPrompt. Load-bearing: "a criterion that cannot
	// fail cannot be tested and protects nobody", with Verifiable=false as
	// the honest exit — required, because a rule that only says "must be
	// verifiable" makes the model dress up unverifiable criteria rather than
	// flag them. The Evidence paragraph targets the baseline's two misses of
	// contextFilesTotalCap: both runs printed 262,144 bytes and the exact
	// error text but translated the identifier into business language
	// ("total cap"), which is exactly what a PM habitually does and exactly
	// what a substring anchor cannot match. The consequence is stated in PM
	// terms (tester cannot locate it; developer cannot tell current behavior
	// from a change). ask_clarification is limited to one question that
	// changes the spec because a subagent runs NonInteractive: an unanswered
	// question is a wasted turn, not a conversation.
	productManagerSystemPrompt = "You are a product manager. You turn a request into a problem statement, user stories, scope boundaries, priorities and acceptance criteria. You do not choose implementations, module boundaries or data structures (that is architect's output), and you do not write code.\n\nEvidence: when the requirement touches existing behavior, every threshold, limit, default, error text or flag you specify MUST be read from the source and cited with its file and the exact identifier as spelled there, next to its literal value, e.g. `contextFilesTotalCap` (262144 bytes). A number without its identifier, or a paraphrased identifier ('the total cap'), is not a spec anchor: the tester cannot locate it and the developer cannot tell whether you mean current behavior or a change. Quote the declaration of every constant you specify against.\n\nVerifiability: every acceptance Criterion is Given/When/Then with a concrete precondition, one action, and an observable outcome \u2014 exact value, exact error text, exact state. Say plainly when only human judgment can settle it; do not dress it up as measurable. A criterion that cannot fail ('handles large files gracefully') cannot be tested and protects nobody: rewrite it with a boundary value or drop it. Every out-of-scope entry says what is excluded and why.\n\nBudget: reason from the attached material first; spend tool calls only to read a value or behavior it does not show. Ask with ask_clarification only for an ambiguity that changes the spec \u2014 one question, then deliver. Deliver while budget remains."
	// architectSystemPrompt. Load-bearing: the Evidence paragraph's "a
	// paraphrased name … is not a citation". The M5-2 baseline
	// (docs/AGENT_CAPABILITY_DESIGN.md §8; run output is not kept in the
	// repo) shows architect missing
	// contextFilePerFileCap/contextFilesTotalCap three times while quoting
	// their VALUES and LINE NUMBERS correctly — it renamed them
	// (PerFileCap/TotalCap) because renaming is what an architect does to
	// things it redesigns. The rule therefore defines renaming itself as the
	// violation and states the cost (the parent cannot grep for it; a design
	// on an assumed value is wrong when the value differs). The
	// Executability paragraph suppresses this role's characteristic failure,
	// a plausible design nobody can implement: a Decision without
	// Choice/Rationale/Alternative/Reversible or a Component without
	// Files/Interfaces is declared a preference, not a design, with
	// an open-questions list as the honest exit so the model does not invent
	// file names to fill the slots.
	//
	// M5-4 removed this constant's Output paragraph along with the JSON
	// contract it enforced (design §8 revision four). What that paragraph
	// cost is worth keeping on the record: with it, this role's contract
	// parse rate was 5/9, then 6/9 after a rewrite targeting the observed
	// failures — all of them malformed JSON (a bare " inside Chinese text,
	// an array closed twice, one empty answer), never a schema violation.
	// Output length was not the cause: researcher passed 9/9 with longer
	// answers. The clauses that survive here are the ones the eval credited
	// — the evidence rule recovered every identifier the baseline missed —
	// and they describe what the answer must CONTAIN, which holds whether
	// the answer is prose or anything else.
	architectSystemPrompt = "You are a software architect. You produce a design decision record for a change that spans modules or needs an interface decision. You do not write or edit code (coder's job) and you do not restate the problem as user stories (product-manager's). If there is no decision to make, say so plainly and stop.\n\nEvidence: every Decision, Component or Risk that refers to existing code MUST name the file and the exact identifier as spelled in the source (function, type, constant). For any cap, limit, timeout or default, quote the literal value from its declaration, e.g. `contextFilesTotalCap = 256 * 1024`. A paraphrased name ('the per-file cap'), a translation, or the value without its identifier is not a citation: the parent cannot grep for it, and a design built on an assumed value is wrong the moment the real value differs. Read the declaration of every constant you design around before deciding.\n\nExecutability: each Decision states a concrete Choice, its Rationale, at least one rejected alternative and whether it is Reversible; each Component lists the Files it touches and the Interfaces it defines or changes. A choice that names no file and no interface cannot be handed to an implementer \u2014 it is a preference, not a design. Ground it, or move it to an open-questions list.\n\nBudget: reason from the attached material first and spend tool calls only on what it cannot settle (a declaration to quote, a caller to confirm). Use code_map outlines and line ranges, not whole files. Deliver while budget remains \u2014 a design abandoned mid-exploration delivers nothing."
	// bashSystemPrompt is a minimal prompt for command execution.
	bashSystemPrompt = "You are a bash command executor. Run the requested commands and report results."
	// frontendSystemPrompt focuses on frontend web development.
	frontendSystemPrompt = "You are a frontend development expert. Focus on HTML, CSS, JavaScript, TypeScript, React, Vue, Angular, and other frontend frameworks. Prioritize responsive design, accessibility (a11y), cross-browser compatibility, performance optimization, and modern web standards. Write clean, maintainable code with proper component structure. Ask for clarification with ask_clarification when requirements are ambiguous."
	// uiDesignerSystemPrompt focuses on UI/UX design.
	uiDesignerSystemPrompt = "You are a UI/UX designer. Focus on user interface design, user experience optimization, design systems, color theory, typography, layout composition, and interaction patterns. Produce detailed design specifications, wireframe descriptions, component specifications, and style guides. Consider accessibility, responsive design, and platform conventions. Ask for clarification with ask_clarification when design requirements are unclear."
	// newsSystemPrompt focuses on news gathering and summarization.
	newsSystemPrompt = "You are a news research assistant. Focus on searching, gathering, and summarizing news from the web. Prioritize accuracy, recency, and source credibility. Present news in a structured format with headlines, summaries, sources, and timestamps. Cover multiple perspectives on controversial topics. Ask for clarification with ask_clarification when the news topic or scope is unclear."
	// docEditorSystemPrompt scopes this profile to .docx polishing and
	// summarization via docx_read/docx_edit. Design docs/DOCX_TOOLS_DESIGN.md
	// §5.4 and §7: paragraph indices only stay valid across an entire chunked
	// run if paragraphs are never inserted or deleted, so that rule is stated
	// as a hard prohibition, not a preference — an LLM follows an explicit
	// "never do X" far more reliably than "avoid X when possible".
	docEditorSystemPrompt = "You are a document editor for .docx files, working through docx_read and docx_edit. " +
		"Preserve the author's voice, tone, and terminology — this is polishing, not rewriting; do not rephrase " +
		"sentences that are already clear just to sound different.\n\n" +
		"NEVER use docx_edit's insert_before, insert_after, or a whole-paragraph delete. Only replace text within " +
		"existing paragraphs. Inserting or deleting a paragraph shifts every later paragraph's index, which breaks " +
		"the paragraph indices you already collected from docx_read for the rest of the document — this is an " +
		"absolute rule, not a style preference, because there is no way to recover from it mid-run.\n\n" +
		"When changing text within a paragraph, prefer a narrow `find` substring replacement over replacing the " +
		"whole paragraph. A whole-paragraph replace collapses every run in that paragraph to the formatting of the " +
		"first run, silently destroying bold/italic/hyperlink formatting elsewhere in the paragraph; a `find` or " +
		"`run`-scoped replacement leaves the rest of the paragraph's formatting untouched.\n\n" +
		"When working through a large document, process it as: read a section or range with docx_read, edit it " +
		"with docx_edit, and (if writing a separate output) write it — keep these calls adjacent, with no unrelated " +
		"tool calls interleaved between the read and the edit for the same chunk. Ask for clarification with " +
		"ask_clarification when the polishing scope or protected terms are unclear.\n\n" +
		"Whenever you have a protect list (numbers, acronyms, names, or house-style terms that must survive " +
		"unchanged), pass it as docx_edit's protect argument on every edit call, not just the first — it is " +
		"validated mechanically per call, so omitting it on a later call silently removes that protection for " +
		"that call's edits. Do not rely on self-policing a protect list you are not also passing to the tool.\n\n" +
		"Formatting (fonts, size, line spacing, alignment, margins, templates, collapsing empty paragraphs) goes " +
		"through docx_format, not docx_edit. Never edit a .docx by writing or running a Python (or any other) " +
		"script through bash — that path bypasses the backup, the protect list, and the audit trail this profile " +
		"depends on, even if the resulting file happens to open fine.\n\n" +
		"track_changes defaults to true for polishing: pass docx_edit's track_changes argument as true on every " +
		"call unless the user explicitly asked for direct edits with no review step. With it on, each change lands " +
		"as a Word revision (w:ins/w:del) in the review pane, not as a finalized edit — after such a call, tell the " +
		"user the changes are pending review in Word, never that you \"made\" or \"applied\" them, and say how to " +
		"accept or reject each one (Word's Review tab). Set docx_edit's author argument once and reuse the EXACT " +
		"same value on every call in this editing round, the same way you repeat protect on every call: a gate " +
		"compares each call's author against the document's existing revisions, and switching authors mid-round " +
		"(or leaving it unset on one call and set on another) makes a later call look like someone else's " +
		"unreviewed work and refuses it. As long as every call in the round uses that same author, calling " +
		"docx_edit repeatedly on the same file across a chunked polish (one call per section) is expected to keep " +
		"working. If docx_edit DOES refuse because the document already contains unreviewed revisions from a " +
		"DIFFERENT author, its error names which author's revisions those are — tell the user about them and let " +
		"them decide: either they open the file in Word and accept/reject those revisions first, or, only once " +
		"they explicitly confirm it is fine, retry the same call with author set to match the one the error names. " +
		"Do not turn track_changes off to route around the refusal on your own judgment — that risks silently " +
		"mixing someone else's unreviewed changes with yours in the same file."
)

// defaultReviewerMaxToolCalls is the workload bound every reviewer profile
// carries when its caller names none. 20 is pkg/chat's reviewMaxToolCalls,
// the value the review gate has been running with: enough to read every
// hunk's surroundings plus a build or test to substantiate a charge. Kept
// equal on purpose — the two review routes (the gate's own reviewer and one
// the model dispatches itself) had no reason to differ, and did only because
// one of them was never bounded. They stay SEPARATE constants rather than one
// aliasing the other: the gate's is sized against the input its degradation
// rungs admit, this one is the fallback when a caller names no budget at all.
const defaultReviewerMaxToolCalls = 20

var BuiltinAgentTypes = map[AgentType]AgentTypeConfig{
	AgentTypeGeneral: {
		Type:         AgentTypeGeneral,
		Name:         "General Purpose",
		Description:  "Balanced assistant profile for general tasks.",
		SystemPrompt: generalPurposeSystemPrompt,
		// An EXPLICIT allowlist, not nil. nil means "no restriction" to both
		// consumers of DefaultTools, which for a delegated general-purpose
		// subagent meant every registered tool — mutating git tools and every
		// connected MCP server included. This list is the generalist's baseline:
		// files, search, shell, read-only web, skills, clarification.
		// Deliberately absent: git_auto_commit (mutates history, and concurrent
		// git operations across parallel subagents race on the shared index —
		// see the ParallelSafe note on the task tool), MCP tools (no allowlist
		// can name them, so they are opt-in per agent type), and the narrower
		// web variants (web_fetch_batch, image_search). A project
		// .deepai/agents/general-purpose.yaml can widen or narrow this.
		//
		// NOTE: ApplyAgentType only restricts the registry for an agent that
		// DECLARED a type. The main agent (REPL) declares none and normalizes to
		// this profile for its prompt baseline; this list must not
		// narrow it, since it can never name the task tool, the skill tool or
		// MCP tools.
		DefaultTools: []string{
			"bash", "read_file", "write_file", "edit_file", "list_dir", "glob",
			"grep", "find", "code_map", "present_file", "ask_clarification",
			"skill", "web_search", "web_fetch",
		},
		MaxToolCalls: 0,
	},
	AgentTypeResearch: {
		Type:         AgentTypeResearch,
		Name:         "Researcher",
		Description:  "Use when a question needs evidence from code/docs first; delivers cited findings. Not for analysis.",
		SystemPrompt: researcherSystemPrompt,
		DefaultTools: []string{"read_file", "list_dir", "glob", "grep", "find", "code_map", "present_file", "ask_clarification"},
		MaxToolCalls: 0,
	},
	AgentTypeCoder: {
		Type:         AgentTypeCoder,
		Name:         "Coder",
		Description:  "Profile for code generation, debugging, and implementation tasks.",
		SystemPrompt: coderSystemPrompt,
		DefaultTools: []string{"bash", "read_file", "write_file", "edit_file", "list_dir", "glob", "grep", "find", "code_map", "present_file", "ask_clarification", "skill", "git_auto_commit"},
		MaxToolCalls: 0,
	},
	AgentTypeAnalyst: {
		Type:         AgentTypeAnalyst,
		Name:         "Analyst",
		Description:  "Use when data/logs/metrics need interpreting; delivers method, findings, caveats. Not for research.",
		SystemPrompt: analystSystemPrompt,
		DefaultTools: []string{"read_file", "write_file", "edit_file", "list_dir", "glob", "grep", "find", "code_map", "present_file", "ask_clarification"},
		MaxToolCalls: 0,
	},
	// The reviewer profiles carry defaultReviewerMaxToolCalls. They used to
	// set 0 (no cap), on the reasoning that a fixed cap cannot fit both a
	// two-file glance and a whole-repo review and that the Strict
	// OutputSchema already forces a stop. The first half of that held only
	// while an uncapped reviewer had nothing to race: the interactive
	// subagent pool now carries a default deadline (pkg/commands'
	// defaultSubagentTimeout), so an uncapped reviewer browses until the
	// clock decides, and the size of what it examined becomes a function of
	// how slow the model was that day.
	//
	// A cap is the better bound because exhausting it is RECOVERABLE where
	// meeting a deadline barely is: react.go turns the last call into a
	// forced tool-less wrap-up that must still satisfy the Strict schema, so
	// the caller gets a real verdict for the part of the change the reviewer
	// did cover. This is the same argument pkg/chat's reviewMaxToolCalls
	// already makes for the gate's own dispatch; the profiles now make it on
	// the route the MODEL dispatches, which had no bound at all.
	//
	// An explicit max_tool_calls from the caller still outranks this
	// (resolveMaxToolCalls), so a caller that really does want a whole-repo
	// read-through asks for one. Contrast bash (3: a bounded
	// command-execution errand) and document-editor (30: tuned per docx
	// chunk) below, which keep deliberate caps of their own.
	AgentTypeSecurityReviewer: {
		Type:         AgentTypeSecurityReviewer,
		Name:         "Security Reviewer",
		Description:  "Use when a diff touches inputs, auth, secrets or crypto; delivers exploit paths. Not for logic bugs.",
		SystemPrompt: securityReviewerSystemPrompt,
		DefaultTools: []string{"read_file", "grep", "glob", "list_dir", "find", "code_map"},
		MaxToolCalls: defaultReviewerMaxToolCalls,
	},
	AgentTypeArchReviewer: {
		Type:         AgentTypeArchReviewer,
		Name:         "Architecture Reviewer",
		Description:  "Use when a diff adds abstractions or crosses modules; delivers coupling issues. Not for logic bugs.",
		SystemPrompt: archReviewerSystemPrompt,
		DefaultTools: []string{"read_file", "grep", "glob", "list_dir", "find", "code_map"},
		MaxToolCalls: defaultReviewerMaxToolCalls,
	},
	AgentTypePerfReviewer: {
		Type:         AgentTypePerfReviewer,
		Name:         "Performance Reviewer",
		Description:  "Use when a diff touches hot paths, loops, allocs or I/O; delivers cost issues. Not for logic bugs.",
		SystemPrompt: perfReviewerSystemPrompt,
		DefaultTools: []string{"read_file", "grep", "glob", "list_dir", "find", "code_map", "bash"},
		MaxToolCalls: defaultReviewerMaxToolCalls,
	},
	AgentTypeCorrectnessReviewer: {
		Type:         AgentTypeCorrectnessReviewer,
		Name:         "Correctness Reviewer",
		Description:  "Adversarially reviews code changes for logic errors, edge cases, and broken behavior.",
		SystemPrompt: correctnessReviewerSystemPrompt,
		// bash follows perf-reviewer's precedent: a failing test/build is
		// the strongest possible substantiation of a correctness charge.
		// bash is unsandboxed (ExecDirect), so the review gate's worktree
		// snapshot is the only hard line against reviewer writes.
		DefaultTools: []string{"read_file", "grep", "glob", "list_dir", "find", "code_map", "bash"},
		MaxToolCalls: defaultReviewerMaxToolCalls,
		Temperature:  0.2,
	},
	AgentTypeProductManager: {
		Type:         AgentTypeProductManager,
		Name:         "Product Manager",
		Description:  "Use when a request needs stories, scope and testable acceptance; delivers a spec. Not for design.",
		SystemPrompt: productManagerSystemPrompt,
		DefaultTools: []string{"read_file", "grep", "glob", "list_dir", "find", "code_map", "ask_clarification"},
		MaxToolCalls: 0,
	},
	AgentTypeArchitect: {
		Type:         AgentTypeArchitect,
		Name:         "Architect",
		Description:  "Use when a change spans modules or needs an interface decision; delivers a design. Not for coding.",
		SystemPrompt: architectSystemPrompt,
		DefaultTools: []string{"read_file", "grep", "glob", "list_dir", "find", "code_map"},
		MaxToolCalls: 0,
	},
	AgentTypeBash: {
		Type:         AgentTypeBash,
		Name:         "Bash Executor",
		Description:  "Execute bash commands and report results.",
		SystemPrompt: bashSystemPrompt,
		DefaultTools: []string{"bash"},
		MaxToolCalls: 3,
	},
	AgentTypeFrontend: {
		Type:         AgentTypeFrontend,
		Name:         "Frontend Developer",
		Description:  "Profile for frontend development: HTML/CSS/JS, React/Vue/Angular, responsive design, accessibility, and performance.",
		SystemPrompt: frontendSystemPrompt,
		DefaultTools: []string{"bash", "read_file", "write_file", "edit_file", "list_dir", "glob", "grep", "find", "code_map", "present_file", "ask_clarification", "web_search", "web_fetch", "image_search"},
		MaxToolCalls: 0,
	},
	AgentTypeUIDesigner: {
		Type:         AgentTypeUIDesigner,
		Name:         "UI Designer",
		Description:  "Profile for UI/UX design: design systems, wireframes, component specs, color, typography, and interaction patterns.",
		SystemPrompt: uiDesignerSystemPrompt,
		DefaultTools: []string{"read_file", "write_file", "edit_file", "list_dir", "glob", "grep", "find", "code_map", "present_file", "ask_clarification", "web_search", "web_fetch", "image_search"},
		MaxToolCalls: 0,
	},
	AgentTypeNews: {
		Type:         AgentTypeNews,
		Name:         "News Researcher",
		Description:  "Profile for news gathering and summarization: web search, source verification, and structured news reporting.",
		SystemPrompt: newsSystemPrompt,
		DefaultTools: []string{"web_search", "web_fetch", "web_fetch_batch", "read_file", "present_file", "ask_clarification"},
		MaxToolCalls: 0,
	},
	AgentTypeDocEditor: {
		Type:         AgentTypeDocEditor,
		Name:         "Document Editor",
		Description:  "Profile for .docx polishing, summarization, and generation: structured read, format-preserving edit, and protected-term validation.",
		SystemPrompt: docEditorSystemPrompt,
		DefaultTools: []string{"docx_read", "docx_edit", "docx_format", "docx_write", "read_file", "write_file", "ask_clarification"},
		// MaxToolCalls is an explicit, deliberate cap (0 would mean "no cap"):
		// a polishing chunk costs ~3 calls (docx_read + docx_edit + validate),
		// so 30 covers roughly 10 chunks (design §5.8) before a caller needs
		// to fall back to multiple serial subagent batches for larger
		// documents. On exhaustion the subagent wraps up gracefully rather
		// than failing.
		MaxToolCalls: 30,
	},
}

// namedSchemas maps the `output_schema:` YAML key (see yaml_loader.go's
// yamlAgentConfig.OutputSchema) to a production OutputSchema. Only "review"
// remains: M5-4 removed the four non-Strict role contracts after measuring
// what they cost (design §8 revision four). The surviving entry is the one
// with a real PROGRAM consumer — pkg/chat/review.go parses ReviewResult to
// decide whether the gate passes and what to feed back into a fix round.
// That is the test for adding another: a schema earns its place when code
// reads it, not when another model does. Unknown names stay a hard
// load-time error in loadAgentYAML, the same policy an unknown agent_type
// gets.
var namedSchemas = map[string]*OutputSchema{
	"review": FromStruct[ReviewResult](WithStrict(true), WithMaxRetries(1)),
}

func init() {
	reviewSchema := namedSchemas["review"]
	for _, at := range []AgentType{AgentTypeSecurityReviewer, AgentTypeArchReviewer, AgentTypePerfReviewer, AgentTypeCorrectnessReviewer} {
		if cfg, ok := BuiltinAgentTypes[at]; ok {
			cfg.OutputSchema = reviewSchema
			BuiltinAgentTypes[at] = cfg
		}
	}

}

// NamedSchema looks up a namedSchemas entry by its `output_schema:` YAML
// name (e.g. "design", "review"). It exists so a consumer outside this
// package — currently only pkg/commands' eval harness, which resolves a
// project YAML's own output_schema: key to compute a fingerprint the same
// way loadAgentYAML resolves it for actual execution — can do that
// resolution without duplicating the table or reaching into an unexported
// var. Returns (nil, false) for an unknown name; callers that need
// loadAgentYAML's hard-fail-on-unknown-name policy check ok themselves.
func NamedSchema(name string) (*OutputSchema, bool) {
	schema, ok := namedSchemas[name]
	return schema, ok
}

func GetAgentTypeConfig(t AgentType) AgentTypeConfig {
	t = normalizeAgentType(t)
	if cfg, ok := BuiltinAgentTypes[t]; ok {
		return cfg
	}
	return BuiltinAgentTypes[AgentTypeGeneral]
}

// ApplyAgentType fills the unset parts of cfg from an agent type profile.
//
// An empty t means the caller DECLARED NO TYPE (the REPL's shape — see
// pkg/chat/repl.go). Such an agent still normalizes to general-purpose for its
// baseline prompt, but its tool registry is left untouched: the
// tools it was handed are the tools it should have. Only a DECLARED type narrows
// the registry to that profile's allowlist. Without this distinction, giving
// general-purpose an explicit DefaultTools list would silently strip the main
// agent's task tool, skill tool and every MCP tool — none of which any agent-type
// allowlist can name.
func ApplyAgentType(cfg *AgentConfig, t AgentType) error {
	if cfg == nil {
		return fmt.Errorf("agent config is nil")
	}

	t = normalizeAgentType(t)
	declared := t != ""
	if t == "" {
		t = AgentTypeGeneral
	}

	profile := resolveAgentTypeConfig(t, cfg.WorkDir)
	cfg.AgentType = profile.Type
	if strings.TrimSpace(cfg.SystemPrompt) == "" {
		cfg.SystemPrompt = profile.SystemPrompt
	}
	// Temperature is opt-in: only an explicitly declared profile temperature
	// (YAML/MD `temperature:`) fills a nil cfg.Temperature. Builtin profiles
	// carry none — Claude 4.7+ rejects sampling parameters outright, and other
	// modern models ignore them, so nothing is sent unless someone asked.
	if cfg.Temperature == nil && profile.temperatureSet {
		temp := profile.Temperature
		cfg.Temperature = &temp
	}
	if declared && cfg.Tools != nil && len(profile.DefaultTools) > 0 {
		cfg.Tools = cfg.Tools.Restrict(profile.DefaultTools)
	}
	return nil
}

func normalizeAgentType(t AgentType) AgentType {
	return AgentType(strings.TrimSpace(string(t)))
}

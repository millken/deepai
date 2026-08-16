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
	AgentTypeProductManager   AgentType = "product-manager"
	AgentTypeArchitect        AgentType = "architect"
	AgentTypeBash             AgentType = "bash"
	AgentTypeFrontend         AgentType = "frontend"
	AgentTypeUIDesigner       AgentType = "ui-designer"
	AgentTypeNews             AgentType = "news"
	AgentTypeDocEditor        AgentType = "document-editor"
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
	// researcherSystemPrompt keeps the agent focused on gathering evidence and synthesizing findings.
	researcherSystemPrompt = "You are a research assistant. Prioritize gathering evidence, reading available material carefully, summarizing findings precisely, and asking for clarification with ask_clarification when the research scope is unclear."
	// coderSystemPrompt keeps the agent focused on code changes, debugging, and verification.
	coderSystemPrompt = "You are a coding assistant.\n\nIntent matching: if asked to review, analyze, or explain, provide findings without modifying files or running git commit/push; only edit files when explicitly asked to change, fix, implement, or refactor.\n\nBehavior rules: (1) Use tools to take action — do not describe what you would do without actually doing it. (2) Every response should either contain tool calls that make progress, or deliver a final result. (3) Do not add features, abstractions, comments, or error handling beyond what was asked. (4) Keep responses concise — go straight to the point. (5) Ask for clarification with ask_clarification before making risky assumptions. (6) When the task is complete, respond with a brief text summary — do NOT continue calling tools.\n\nGit workflow: use bash for git inspection and any manual git operations (status, diff, log, add, etc.) — do NOT commit or push automatically. Leave your changes uncommitted in the working tree so the user can review them. git_auto_commit is the only dedicated git tool; call it only when the user's request explicitly asks you to commit or push (e.g. it contains \"commit\", \"提交\", \"push\", or \"auto-push\"). When you do commit, stage only the files you changed, and commit only a complete logical unit of work — never partial progress. Set auto_push to true only if the request explicitly mentions pushing."
	// analystSystemPrompt keeps the agent focused on structured analysis and communicating results clearly.
	analystSystemPrompt = "You are a data analyst. Inspect the available data carefully, explain conclusions clearly, generate artifacts when useful, and ask for clarification with ask_clarification when the analytical objective is underspecified."
	// securityReviewerSystemPrompt focuses on security vulnerabilities and risks.
	securityReviewerSystemPrompt = "You are an independent security code reviewer. You must make objective judgments based on the code you see.\n\nFocus on: injection vulnerabilities (SQL, command, XSS), authentication and authorization flaws, sensitive data exposure, insecure defaults, and cryptographic weaknesses.\n\nRules:\n1. Do not assume code intent is correct — verify it.\n2. If the code looks fine, output verdict \"pass\" — do not invent issues.\n3. Output your findings as structured JSON matching the ReviewResult schema."
	// archReviewerSystemPrompt focuses on architectural design quality.
	archReviewerSystemPrompt = "You are an independent architecture reviewer. You must make objective judgments based on the code you see.\n\nFocus on: design patterns, coupling and cohesion, extensibility, maintainability, error handling patterns, and API design.\n\nRules:\n1. Do not assume code intent is correct — verify it.\n2. If the code looks fine, output verdict \"pass\" — do not invent issues.\n3. Output your findings as structured JSON matching the ReviewResult schema."
	// perfReviewerSystemPrompt focuses on performance characteristics.
	perfReviewerSystemPrompt = "You are an independent performance reviewer. You must make objective judgments based on the code you see.\n\nFocus on: algorithm complexity, memory allocations, I/O patterns, concurrency bottlenecks, and resource leaks.\n\nRules:\n1. Do not assume code intent is correct — verify it.\n2. If the code looks fine, output verdict \"pass\" — do not invent issues.\n3. Output your findings as structured JSON matching the ReviewResult schema."
	// correctnessReviewerSystemPrompt drives the adversarial post-edit
	// review gate. Its load-bearing constraint is rule 2: an issue without
	// a reproducible failure scenario does not count. That one rule
	// suppresses both failure modes of an adversarial reviewer at once —
	// inventing plausible-sounding issues to justify its existence (false
	// positives), and vague concerns the implementer cannot act on. Rule 3
	// permits bash for substantiation but forbids writes; the hard
	// enforcement is the gate's before/after worktree snapshot (design
	// §4.4), not this sentence.
	correctnessReviewerSystemPrompt = "You are an independent adversarial correctness reviewer. Your job is to try to BREAK the change you are given, not to approve it.\n\nYou receive: the original task description, the diff of the change, and the changed files. You do NOT see the implementer's reasoning — judge only what the code actually does.\n\nFocus on: logic errors, unhandled edge cases (empty/nil/zero/boundary), off-by-one, error-path behavior, concurrency hazards introduced by the change, and whether the change actually satisfies the stated task.\n\nRules:\n1. Do not assume code intent is correct — verify it.\n2. Every issue you report MUST include a concrete failure scenario in the \"scenario\" field: specific input or state → specific wrong output or behavior. An issue without a reproducible scenario does not count — do not report vague concerns.\n3. You may use bash to compile or run tests to substantiate an issue, but you MUST NOT modify, create, or delete any file in the project — you are a reviewer, not a fixer.\n4. If you cannot construct a failure scenario, output verdict \"pass\" — do not invent issues, and do not fail a change for style or taste.\n5. Output your findings as structured JSON matching the ReviewResult schema."
	// productManagerSystemPrompt focuses on user needs and feature planning.
	productManagerSystemPrompt = "You are a product manager. Focus on user needs, feature decomposition, priority assessment, and acceptance criteria. Ask for clarification with ask_clarification when requirements are ambiguous."
	// architectSystemPrompt focuses on system design, module decomposition, and interface definition.
	architectSystemPrompt = "You are a software architect. Focus on system design, module decomposition, interface definition, data flow, and technology selection. Produce clear technical design documents with concrete decisions and rationale. Ask for clarification with ask_clarification when requirements are ambiguous."
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
		// this profile for its prompt/temperature baseline; this list must not
		// narrow it, since it can never name the task tool, the skill tool or
		// MCP tools.
		DefaultTools: []string{
			"bash", "read_file", "write_file", "edit_file", "list_dir", "glob",
			"grep", "find", "code_map", "present_file", "ask_clarification",
			"skill", "web_search", "web_fetch",
		},
		MaxToolCalls: 0,
		Temperature:  0.2,
	},
	AgentTypeResearch: {
		Type:         AgentTypeResearch,
		Name:         "Researcher",
		Description:  "Profile for research, reading, and synthesis tasks.",
		SystemPrompt: researcherSystemPrompt,
		DefaultTools: []string{"read_file", "list_dir", "glob", "grep", "find", "code_map", "present_file", "ask_clarification"},
		MaxToolCalls: 0,
		Temperature:  0.1,
	},
	AgentTypeCoder: {
		Type:         AgentTypeCoder,
		Name:         "Coder",
		Description:  "Profile for code generation, debugging, and implementation tasks.",
		SystemPrompt: coderSystemPrompt,
		DefaultTools: []string{"bash", "read_file", "write_file", "edit_file", "list_dir", "glob", "grep", "find", "code_map", "present_file", "ask_clarification", "skill", "git_auto_commit"},
		MaxToolCalls: 0,
		Temperature:  0.1,
	},
	AgentTypeAnalyst: {
		Type:         AgentTypeAnalyst,
		Name:         "Analyst",
		Description:  "Profile for structured analysis and artifact generation.",
		SystemPrompt: analystSystemPrompt,
		DefaultTools: []string{"read_file", "write_file", "edit_file", "list_dir", "glob", "grep", "find", "code_map", "present_file", "ask_clarification"},
		MaxToolCalls: 0,
		Temperature:  0.15,
	},
	// The three reviewer profiles deliberately set MaxToolCalls 0 (no cap —
	// same rationale as the global default): a fixed cap cannot fit both a
	// two-file glance and a whole-repo review, and their Strict OutputSchema
	// already forces them to stop and emit JSON. Contrast bash (3: a bounded
	// command-execution errand) and document-editor (30: tuned per docx
	// chunk) below, which keep deliberate caps.
	AgentTypeSecurityReviewer: {
		Type:         AgentTypeSecurityReviewer,
		Name:         "Security Reviewer",
		Description:  "Reviews code for security vulnerabilities, injection risks, and permission issues.",
		SystemPrompt: securityReviewerSystemPrompt,
		DefaultTools: []string{"read_file", "grep", "glob", "list_dir", "find", "code_map"},
		MaxToolCalls: 0,
		Temperature:  0.2,
	},
	AgentTypeArchReviewer: {
		Type:         AgentTypeArchReviewer,
		Name:         "Architecture Reviewer",
		Description:  "Reviews code for design patterns, coupling, extensibility, and maintainability.",
		SystemPrompt: archReviewerSystemPrompt,
		DefaultTools: []string{"read_file", "grep", "glob", "list_dir", "find", "code_map"},
		MaxToolCalls: 0,
		Temperature:  0.2,
	},
	AgentTypePerfReviewer: {
		Type:         AgentTypePerfReviewer,
		Name:         "Performance Reviewer",
		Description:  "Reviews code for algorithm complexity, memory, I/O, and concurrency issues.",
		SystemPrompt: perfReviewerSystemPrompt,
		DefaultTools: []string{"read_file", "grep", "glob", "list_dir", "find", "code_map", "bash"},
		MaxToolCalls: 0,
		Temperature:  0.2,
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
		MaxToolCalls: 0,
		Temperature:  0.2,
	},
	AgentTypeProductManager: {
		Type:         AgentTypeProductManager,
		Name:         "Product Manager",
		Description:  "Plans features, decomposes requirements, and defines acceptance criteria.",
		SystemPrompt: productManagerSystemPrompt,
		DefaultTools: []string{"read_file", "grep", "glob", "list_dir", "find", "code_map", "ask_clarification"},
		MaxToolCalls: 0,
		Temperature:  0.15,
	},
	AgentTypeArchitect: {
		Type:         AgentTypeArchitect,
		Name:         "Architect",
		Description:  "Produces technical design documents, system decomposition, and interface definitions.",
		SystemPrompt: architectSystemPrompt,
		DefaultTools: []string{"read_file", "grep", "glob", "list_dir", "find", "code_map"},
		MaxToolCalls: 0,
		Temperature:  0.2,
	},
	AgentTypeBash: {
		Type:         AgentTypeBash,
		Name:         "Bash Executor",
		Description:  "Execute bash commands and report results.",
		SystemPrompt: bashSystemPrompt,
		DefaultTools: []string{"bash"},
		MaxToolCalls: 3,
		Temperature:  0.0,
	},
	AgentTypeFrontend: {
		Type:         AgentTypeFrontend,
		Name:         "Frontend Developer",
		Description:  "Profile for frontend development: HTML/CSS/JS, React/Vue/Angular, responsive design, accessibility, and performance.",
		SystemPrompt: frontendSystemPrompt,
		DefaultTools: []string{"bash", "read_file", "write_file", "edit_file", "list_dir", "glob", "grep", "find", "code_map", "present_file", "ask_clarification", "web_search", "web_fetch", "image_search"},
		MaxToolCalls: 0,
		Temperature:  0.15,
	},
	AgentTypeUIDesigner: {
		Type:         AgentTypeUIDesigner,
		Name:         "UI Designer",
		Description:  "Profile for UI/UX design: design systems, wireframes, component specs, color, typography, and interaction patterns.",
		SystemPrompt: uiDesignerSystemPrompt,
		DefaultTools: []string{"read_file", "write_file", "edit_file", "list_dir", "glob", "grep", "find", "code_map", "present_file", "ask_clarification", "web_search", "web_fetch", "image_search"},
		MaxToolCalls: 0,
		Temperature:  0.2,
	},
	AgentTypeNews: {
		Type:         AgentTypeNews,
		Name:         "News Researcher",
		Description:  "Profile for news gathering and summarization: web search, source verification, and structured news reporting.",
		SystemPrompt: newsSystemPrompt,
		DefaultTools: []string{"web_search", "web_fetch", "web_fetch_batch", "read_file", "present_file", "ask_clarification"},
		MaxToolCalls: 0,
		Temperature:  0.1,
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
		Temperature:  0.2,
	},
}

func init() {
	reviewSchema := FromStruct[ReviewResult](WithStrict(true), WithMaxRetries(1))
	for _, at := range []AgentType{AgentTypeSecurityReviewer, AgentTypeArchReviewer, AgentTypePerfReviewer, AgentTypeCorrectnessReviewer} {
		if cfg, ok := BuiltinAgentTypes[at]; ok {
			cfg.OutputSchema = reviewSchema
			BuiltinAgentTypes[at] = cfg
		}
	}
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
// baseline prompt and temperature, but its tool registry is left untouched: the
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
	if cfg.Temperature == nil {
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

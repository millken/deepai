package agent

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/models"
)

// maxTaskCallsPerRun caps how many "task" tool calls a single Run will
// execute — a cost backstop for unattended runs. Deliberately generous
// relative to the subagent pool's own concurrency limit (4 by default):
// this bounds total fan-out across the whole run, not concurrent fan-out at
// any one instant.
const maxTaskCallsPerRun = 20

// taskCallOverCap increments *counter for "task" tool calls only and reports
// whether THIS call exceeds maxTaskCallsPerRun (M2-2 12c). Non-task calls
// are always allowed and never touch the counter. Must be called from a
// single goroutine per Run (the serial dispatch loop, or the parallel path's
// pre-dispatch admission pass) — it is not safe for concurrent use.
func taskCallOverCap(counter *int, call models.ToolCall) bool {
	if call.Name != "task" {
		return false
	}
	*counter++
	return *counter > maxTaskCallsPerRun
}

// synthesizeTaskCapResult builds the refusal ToolResult for a "task" call
// that was never executed because it would exceed maxTaskCallsPerRun. This
// is a per-call refusal, not a fatal run error: it is fed through the normal
// result path (message history, metrics, events, breaker observation) so
// the run continues with the model informed of the limit.
func synthesizeTaskCapResult(call models.ToolCall) models.ToolResult {
	msg := fmt.Sprintf("task call limit reached for this run (%d); finish with what you have or ask the user to continue", maxTaskCallsPerRun)
	return models.ToolResult{
		CallID:      call.ID,
		ToolName:    call.Name,
		Status:      models.CallStatusFailed,
		Error:       msg,
		CompletedAt: time.Now().UTC(),
	}
}

// isValidationError reports whether a tool error string originates from
// argument validation (missing required args, invalid schema). Used by the
// circuit-breaker to distinguish fixable model mistakes from real failures.
func isValidationError(errMsg string) bool {
	return strings.HasPrefix(errMsg, "missing required argument") ||
		strings.HasPrefix(errMsg, "invalid tool arguments")
}

// maxValidationRetries is the number of consecutive validation failures for the
// same tool before the circuit-breaker injects a corrective human hint.
const maxValidationRetries = 3

// maxConsecutiveValidationFailures is the global (all-tools) consecutive
// validation-failure count that hard-stops the run, even when the model
// alternates between different badly-called tools instead of retrying one.
const maxConsecutiveValidationFailures = 8

// Repeat-call circuit-breaker thresholds.
//
// The validation breaker above only catches tools that fail argument schema
// validation (CallStatusFailed + isValidationError). It cannot detect the much
// more common loop pattern where a tool *succeeds* (e.g. bash runs `go test`,
// returns exit_code 1 buried in the JSON output, CallStatusCompleted) but the
// model keeps re-running the exact same command hoping for a different result.
//
// maxRepeatCalls: consecutive identical tool invocations (same name + args)
// before injecting a non-fatal hint nudging the model to change approach.
// maxRepeatFails: consecutive identical invocations that also fail (including
// bash non-zero exit) before hard-stopping the run.
// maxRepeatCallsCumulative / maxRepeatCallsHardStop bound identical calls that
// are NOT consecutive. maxRepeatCalls above only counts a run of the same key,
// so the loop that actually burned session 20260812_093415_fc6e was invisible
// to it: the model cycled read_file over three files (A, B, A, A, B, A …), and
// every switch reset the consecutive counter before it reached 5. It made 1136
// read_file calls over 7 distinct paths — 858 of them the same path — without
// the breaker ever emitting a single hint.
//
// Counting per key for the whole run catches any cycle length. A hint goes out
// every maxRepeatCallsCumulative identical calls, and the run hard-stops at
// maxRepeatCallsHardStop: by then the model has been told three times and is
// not recovering on its own, so stopping and reporting beats burning the
// remaining budget. Both are per (tool, arguments) — a re-read after an edit
// (different content, same args) is normal and stays well under the floor.
//
// maxSameFailureHint / maxSameFailureHardStop bound a failure that repeats
// IDENTICALLY while the arguments keep changing. Both counters above are keyed
// on (tool, arguments), so a model that varies cosmetic details of a command
// evades them completely: `dart test test/reconnect/ | tail -20`, then
// `| tail -30`, then no pipe at all, then a single test file — four distinct
// keys, one identical result (`timed out after 120s`, no output), eight
// attempts, sixteen minutes of wall clock. Keying on the failure TEXT instead
// of the arguments catches that: same tool, same failure, N times, however the
// call was spelled.
//
// Both thresholds sit ABOVE maxRepeatFails and maxConsecutiveValidationFailures
// on purpose. This is the outermost net: when a loop fits one of the specific
// shapes those breakers describe, they should be the ones to name it (their
// messages are more precise, and the run stops sooner). This one only fires on
// what they structurally cannot see.
const (
	maxRepeatCalls           = 5
	maxRepeatFails           = 8
	maxRepeatCallsCumulative = 6
	maxRepeatCallsHardStop   = 24
	maxSameFailureHint       = 4
	maxSameFailureHardStop   = 10
)

// repeatKey builds a deduplication key from tool name and arguments hash.
// Two calls with the same key are "the same operation" for loop detection.
func repeatKey(toolName string, args map[string]any) string {
	return toolName + "\x00" + computeArgsHash(args)
}

// toolCallBreaker holds all circuit-breaker state for one Run(): the
// validation-failure loop and the repeat-call loop. Run()'s serial and
// parallel tool-execution paths both feed every (call, result) pair through
// observe(), in batch order, so a model looping on a parallel-safe batch is
// stopped exactly as it would be on the serial path — one implementation,
// two call sites.
type toolCallBreaker struct {
	// validationFailures tracks consecutive tool-validation errors per tool
	// name; consecutiveValidationFailures is the same count across all tools,
	// so alternating between two badly-called tools still trips the breaker.
	validationFailures            map[string]int
	consecutiveValidationFailures int

	// repeatCount/repeatFail track consecutive identical tool invocations
	// (same name + args hash). prevRepeatKey gives "consecutive" semantics
	// — switching to a different call resets both counters.
	repeatCount   int
	repeatFail    int
	prevRepeatKey string

	// repeatTotals counts identical invocations across the WHOLE run, keyed
	// the same way. Unlike repeatCount it never resets, so a model cycling
	// between two or three calls cannot hide from it.
	repeatTotals map[string]int

	// sameFailure counts identical FAILURE outcomes per tool, ignoring the
	// arguments entirely — the one counter a model cannot dodge by rewording
	// the command it keeps re-running.
	sameFailure map[string]int
}

func newToolCallBreaker() *toolCallBreaker {
	return &toolCallBreaker{
		validationFailures: make(map[string]int),
		repeatTotals:       make(map[string]int),
		sameFailure:        make(map[string]int),
	}
}

// breakerObservation is the outcome of feeding one (call, result) pair
// through toolCallBreaker.observe.
type breakerObservation struct {
	// hintMessages are synthetic human messages to append to runMessages
	// (repeat-call hint, validation-failure hint, or both).
	hintMessages []models.Message
	// validationFailure reports whether this call counted as a validation
	// failure, so the caller can clear its per-batch "clean" flag.
	validationFailure bool
	// fatalErr/fatalAgentErr are set when a breaker trips fatally; the
	// caller must stop processing the batch and return immediately.
	fatalErr      error
	fatalAgentErr *AgentError
}

// observe runs the repeat-call breaker and then the validation-failure
// breaker for one tool call result, mirroring the original inline order: the
// repeat-call breaker sees every result (success or failure), the validation
// breaker only failed-validation results. A fatal trip short-circuits before
// the validation breaker runs, matching the original code's early return.
func (b *toolCallBreaker) observe(sessionID string, call models.ToolCall, result models.ToolResult) breakerObservation {
	var out breakerObservation

	// Repeat-call circuit-breaker: detect when the model re-runs the exact
	// same tool with the same arguments without progress. This catches loops
	// the validation breaker misses — e.g. `go test` returning exit_code 1 as
	// a "completed" bash result, causing the model to retry the identical
	// command dozens of times.
	rKey := repeatKey(call.Name, call.Arguments)
	if rKey != b.prevRepeatKey {
		b.repeatCount = 0
		b.repeatFail = 0
		b.prevRepeatKey = rKey
	}
	b.repeatCount++
	if isResultFailed(result) {
		b.repeatFail++
	} else {
		b.repeatFail = 0
	}

	// Fatal: repeated identical failures mean retrying won't help.
	if b.repeatFail >= maxRepeatFails {
		err := fmt.Errorf("repeated identical failed tool call (%q x%d): %s",
			call.Name, b.repeatFail, result.Error)
		out.fatalErr = err
		out.fatalAgentErr = &AgentError{
			Code:    "tool_repeat_loop",
			Message: err.Error(),
			Suggestion: "The model repeatedly ran the same failing command. " +
				"Inspect the failing tool output and try a different approach.",
		}
		return out
	}
	// Cumulative repeat detection: same call, anywhere in this run. Catches
	// cycles (A, B, A, A, B …) that keep resetting the consecutive counter.
	if b.repeatTotals == nil {
		b.repeatTotals = make(map[string]int)
	}
	b.repeatTotals[rKey]++
	if total := b.repeatTotals[rKey]; total >= maxRepeatCallsHardStop {
		err := fmt.Errorf("identical tool call repeated %d times in one run (%q): the run is looping",
			total, call.Name)
		out.fatalErr = err
		out.fatalAgentErr = &AgentError{
			Code:    "tool_repeat_loop",
			Message: err.Error(),
			Suggestion: "The model kept re-running the same call instead of using what it already had. " +
				"Re-run with a narrower request, or check whether context compression is discarding " +
				"the tool results it needs (config.yaml: context_window / token_aging).",
		}
		return out
	} else if total%maxRepeatCallsCumulative == 0 && total > b.repeatCount {
		// total > repeatCount: the consecutive breaker below is not already
		// hinting about this same streak, so this hint adds information.
		out.hintMessages = append(out.hintMessages, models.Message{
			ID:        newMessageID("human"),
			SessionID: sessionID,
			Role:      models.RoleHuman,
			Metadata:  map[string]string{metaAgentInjected: "true"},
			Content: fmt.Sprintf(
				"You have now called %q with these exact arguments %d times in this run, "+
					"cycling between a few calls. The result is not going to change. "+
					"Work from what you already have in the conversation; if you genuinely cannot "+
					"see the content you need, say so instead of re-fetching it.",
				call.Name, total),
			CreatedAt: time.Now().UTC(),
		})
	}

	// Same failure, whatever the arguments were.
	if obs, stop := b.observeSameFailure(sessionID, call, result); stop {
		return obs
	} else if len(obs.hintMessages) > 0 {
		out.hintMessages = append(out.hintMessages, obs.hintMessages...)
	}

	// Non-fatal hint: nudge the model to change approach (inject once).
	if b.repeatCount == maxRepeatCalls {
		out.hintMessages = append(out.hintMessages, models.Message{
			ID:        newMessageID("human"),
			SessionID: sessionID,
			Role:      models.RoleHuman,
			Metadata:  map[string]string{metaAgentInjected: "true"},
			Content: fmt.Sprintf(
				"You have run %q %d times with identical arguments. "+
					"If the result isn't changing, you are in a loop. "+
					"Try a different command, inspect the output more carefully, or move on.",
				call.Name, b.repeatCount),
			CreatedAt: time.Now().UTC(),
		})
	}

	// Circuit-breaker: if the same tool fails argument validation
	// maxValidationRetries times in a row, inject a human hint and reset the
	// counter. This prevents infinite loops where the model keeps omitting
	// required arguments.
	if result.Status == models.CallStatusFailed && isValidationError(result.Error) {
		out.validationFailure = true
		b.consecutiveValidationFailures++
		b.validationFailures[call.Name]++
		if b.validationFailures[call.Name] >= maxValidationRetries {
			hint := fmt.Sprintf(
				"You have called %q %d times without providing the required arguments and each attempt failed with: %s. "+
					"Please re-read the tool schema carefully, provide ALL required arguments, or ask the user for the missing information instead of retrying.",
				call.Name, b.validationFailures[call.Name], result.Error,
			)
			out.hintMessages = append(out.hintMessages, models.Message{
				ID:        newMessageID("human"),
				SessionID: sessionID,
				Role:      models.RoleHuman,
				Metadata:  map[string]string{metaAgentInjected: "true"},
				Content:   hint,
				CreatedAt: time.Now().UTC(),
			})
			b.validationFailures[call.Name] = 0
		}
		if b.consecutiveValidationFailures >= maxConsecutiveValidationFailures {
			err := fmt.Errorf("too many consecutive tool argument validation failures (%d): %s", b.consecutiveValidationFailures, result.Error)
			out.fatalErr = err
			out.fatalAgentErr = &AgentError{
				Code:       "tool_validation_loop",
				Message:    err.Error(),
				Suggestion: "Model repeatedly called tools without required arguments. Try a shorter request or explicitly provide missing parameters.",
			}
			return out
		}
	} else {
		b.validationFailures[call.Name] = 0
	}

	return out
}

// observeSameFailure counts this call's outcome against the "same failure,
// different arguments" ceiling and returns the hint (or the fatal trip) it
// earns. Successful results are ignored entirely — and deliberately do NOT
// reset the count: if the identical failure text keeps coming back, whatever
// happened in between did not fix it.
//
// stop reports that the returned observation is fatal and the caller must
// return it immediately without running the remaining breakers.
func (b *toolCallBreaker) observeSameFailure(sessionID string, call models.ToolCall, result models.ToolResult) (out breakerObservation, stop bool) {
	if !isResultFailed(result) {
		return out, false
	}
	if b.sameFailure == nil {
		b.sameFailure = make(map[string]int)
	}
	key := call.Name + "\x00" + failureSignature(result)
	b.sameFailure[key]++
	n := b.sameFailure[key]

	if n >= maxSameFailureHardStop {
		err := fmt.Errorf("%q failed the same way %d times in one run despite changed arguments: %s",
			call.Name, n, firstLine(failureSignature(result)))
		out.fatalErr = err
		out.fatalAgentErr = &AgentError{
			Code:    "tool_repeat_loop",
			Message: err.Error(),
			Suggestion: "The model kept re-running a call that fails identically every time, " +
				"rewording it instead of changing the approach. Check the failure above — " +
				"a command that times out with no output usually means it is waiting on " +
				"something unavailable.",
		}
		return out, true
	}
	if n == maxSameFailureHint {
		out.hintMessages = append(out.hintMessages, models.Message{
			ID:        newMessageID("human"),
			SessionID: sessionID,
			Role:      models.RoleHuman,
			Metadata:  map[string]string{metaAgentInjected: "true"},
			Content: fmt.Sprintf(
				"%q has now failed %d times with the SAME result, even though you changed the "+
					"arguments each time: %s\n"+
					"Rewording the call is not working. Change what you are doing — a different "+
					"tool, a smaller scope, a longer timeout — or tell the user what is blocking you.",
				call.Name, n, firstLine(failureSignature(result))),
			CreatedAt: time.Now().UTC(),
		})
	}
	return out, false
}

// failureSignature is the text that identifies "the same failure". Error is
// preferred (it is what the model was shown, via toolMessageContent) and
// Content is the fallback for tools that report failure in their payload —
// bash's non-zero exit_code JSON, for one.
func failureSignature(result models.ToolResult) string {
	if strings.TrimSpace(result.Error) != "" {
		return result.Error
	}
	return result.Content
}

// firstLine trims a failure signature down to something that reads well inside
// a hint, without dragging a whole captured stdout along.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// resetOnCleanBatch clears the global consecutive-validation-failure counter
// after a batch where every call passed validation — the same batchClean
// semantics the original inline code applied per turn.
func (b *toolCallBreaker) resetOnCleanBatch() {
	b.consecutiveValidationFailures = 0
}

// extractBashExitCode parses the exit_code from a bash tool result's JSON
// content. Returns 0 for non-bash tools or unparseable content. bash is the
// primary tool whose "failure" (non-zero exit) is reported as a successful
// tool execution (CallStatusCompleted) with the code buried in the output JSON.
func extractBashExitCode(toolName, content string) int {
	if toolName != "bash" || content == "" {
		return 0
	}
	var parsed struct {
		ExitCode int `json:"exit_code"`
	}
	if json.Unmarshal([]byte(content), &parsed) != nil {
		return 0
	}
	return parsed.ExitCode
}

// isResultFailed reports whether a tool result represents a real failure,
// including bash commands that exited non-zero despite CallStatusCompleted.
func isResultFailed(result models.ToolResult) bool {
	if result.Status == models.CallStatusFailed {
		return true
	}
	return extractBashExitCode(result.ToolName, result.Content) != 0
}

// M1.2: Enhanced metrics collection helpers

// computeArgsHash generates a hash of tool arguments for deduplication
// detection. It returns a fixed-width hex FNV-1a 64 digest rather than the
// raw argument text: the metrics sink writes this value verbatim to the
// JSONL metrics file, and raw argument VALUES (bash commands, secrets, etc.)
// must not land there unhashed. Equality is all callers need (the metrics
// ArgsHash field and the repeat-call breaker's rKey), so hashing is
// behavior-preserving.
//
// Values are serialized via json.Marshal (not fmt.Sprintf("%v")) so nested
// maps get sorted-key output — Go's %v formats map keys in random order,
// which would make identical nested args hash differently and break the
// repeat-call breaker's loop detection.
func computeArgsHash(args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var hashContent strings.Builder
	for _, k := range keys {
		hashContent.WriteString(k)
		hashContent.WriteString("=")
		valBytes, err := json.Marshal(args[k])
		if err != nil {
			valBytes = []byte(fmt.Sprintf("%v", args[k]))
		}
		hashContent.Write(valBytes)
		hashContent.WriteString("&")
	}

	h := fnv.New64a()
	_, _ = h.Write([]byte(hashContent.String()))
	return fmt.Sprintf("%016x", h.Sum64())
}

// extractPathFromArgs extracts file path from tool arguments if applicable
func extractPathFromArgs(toolName string, args map[string]any) string {
	// File-based tools that typically have a "path" argument
	fileTools := map[string]bool{
		"read_file":  true,
		"edit_file":  true,
		"write_file": true,
		"list_dir":   true,
		"find":       true,
		"code_map":   true,
	}

	if !fileTools[toolName] {
		return ""
	}

	if path, ok := args["path"].(string); ok && path != "" {
		return path
	}

	// Some tools might use different parameter names
	if toolName == "code_map" {
		if path, ok := args["directory"].(string); ok && path != "" {
			return path
		}
	}

	return ""
}

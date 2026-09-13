package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

// planToolNames are the tools available while in plan mode (read-only).
// exit_plan_mode and write_plan are registered dynamically after Restrict.
// Note: task is intentionally absent — this prevents an agent in plan mode
// from spawning a sub-agent (e.g. coder) to bypass the read-only restriction.
// BuildSystemPrompt also checks a.tools.Get("task"), so the team-delegation
// prompt is naturally omitted in plan mode.
var planToolNames = []string{
	"read_file", "list_dir", "glob", "grep", "find", "code_map",
	"ask_clarification", "present_file",
}

const planModePrompt = `You are in plan mode. Your job is to explore the codebase and create an implementation plan — do NOT write or edit any project files.

Guidelines:
- Use read_file, grep, glob, list_dir, find to understand the codebase
- Use write_plan to save/update your plan as you explore
- Ask clarification questions if requirements are ambiguous
- When done, call exit_plan_mode to submit the plan for user approval
- The plan should include: files to modify, approach, and key considerations

Your plan is automatically saved to a file so it persists even if context is compacted.`

// enterPlanMode restricts the agent to read-only tools and creates a plan file.
//
// Serial-only: called from within the ReAct loop's tool execution path
// or from New(), both of which are single-goroutine.
func (a *Agent) enterPlanMode() {
	if a.planMode.Load() {
		return
	}
	a.fullTools = a.tools
	restricted := a.fullTools.RestrictTo(planToolNames)
	// Register plan-mode-only tools into the restricted set.
	for _, tool := range []models.Tool{a.makeWritePlanTool(), a.makeExitPlanModeTool()} {
		if err := restricted.Register(tool); err != nil {
			a.logger.Error("register plan tool in restricted set", "tool", tool.Name, "err", err)
			a.fullTools = nil
			return
		}
	}
	a.tools = restricted
	a.planMode.Store(true)
	// Only open a new plan file when the caller has not pinned one. The
	// mission loop pins .deepai/missions/<id>/design.md for every turn of a
	// design phase so successive rounds revise ONE document (R9); opening a
	// fresh timestamped file here would strand the previous round's plan.
	if a.planFile == "" {
		a.initPlanFile()
	}
	a.logger.Debug("entered plan mode", "plan_file", a.planFile)
}

// exitPlanMode restores the full tool set.
//
// Serial-only: same serial guarantee as enterPlanMode.
func (a *Agent) exitPlanMode() {
	if !a.planMode.Load() {
		return
	}
	a.planMode.Store(false)
	if a.fullTools != nil {
		a.tools = a.fullTools
		a.fullTools = nil
	}
	a.logger.Debug("exited plan mode")
}

// PlanFile returns the path of the plan document this agent writes through
// write_plan, or "" if plan mode was never entered. The REPL reads it after
// a turn so that a plan written in an ORDINARY turn (the model decided on
// its own to call enter_plan_mode) can be carried into a mission when
// mission_on_plan is set — otherwise the upgrade would have to guess which
// timestamped file under .deepai/plans/ belonged to that turn.
func (a *Agent) PlanFile() string {
	if a == nil {
		return ""
	}
	return a.planFile
}

// IsPlanMode returns whether the agent is currently in plan mode.
func (a *Agent) IsPlanMode() bool {
	return a.planMode.Load()
}

// initPlanFile creates the plan file path under .deepai/plans/.
func (a *Agent) initPlanFile() {
	dir := a.workDir
	if dir == "" {
		dir, _ = os.Getwd()
	}
	plansDir := filepath.Join(dir, ".deepai", "plans")
	_ = os.MkdirAll(plansDir, 0o755)
	timestamp := time.Now().Format("2006-01-02-150405")
	a.planFile = filepath.Join(plansDir, timestamp+".md")
}

// registerPlanTools adds enter_plan_mode to the agent's tool registry.
func (a *Agent) registerPlanTools() {
	if a.tools == nil {
		return
	}
	if err := a.tools.Register(a.makeEnterPlanModeTool()); err != nil {
		a.logger.Error("register enter_plan_mode tool", "err", err)
	}
}

func (a *Agent) makeEnterPlanModeTool() models.Tool {
	return models.Tool{
		Name:        "enter_plan_mode",
		Description: "Enter plan mode to explore the codebase and create an implementation plan before writing code. Use this for complex, multi-file tasks that benefit from upfront planning.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"reason": map[string]any{
					"type":        "string",
					"description": "Why planning is needed for this task",
				},
			},
			"required": []string{"reason"},
		},
		Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
			reason, _ := call.Arguments["reason"].(string)
			a.enterPlanMode()
			msg := fmt.Sprintf("Entered plan mode. Plan file: %s\n\nExplore the codebase, then use write_plan to build your plan. Call exit_plan_mode when ready to submit for approval.", a.planFile)
			if reason != "" {
				msg = fmt.Sprintf("Entered plan mode (reason: %s). Plan file: %s\n\nExplore the codebase, then use write_plan to build your plan. Call exit_plan_mode when ready.", reason, a.planFile)
			}
			return models.ToolResult{
				CallID:      call.ID,
				ToolName:    call.Name,
				Status:      models.CallStatusCompleted,
				Content:     msg,
				CompletedAt: time.Now().UTC(),
			}, nil
		},
	}
}

func (a *Agent) makeWritePlanTool() models.Tool {
	return models.Tool{
		Name:        "write_plan",
		Description: "Write or update the implementation plan. The plan is persisted to a file so it survives context compaction. Call this as you discover information during exploration.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"content": map[string]any{
					"type":        "string",
					"description": "The plan content in markdown",
				},
			},
			"required": []string{"content"},
		},
		Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
			content, _ := call.Arguments["content"].(string)
			const maxPlanSize = 64 * 1024 // 64KB
			if len(content) > maxPlanSize {
				return models.ToolResult{
					CallID:      call.ID,
					ToolName:    call.Name,
					Status:      models.CallStatusFailed,
					Error:       fmt.Sprintf("plan too large: %d bytes (max %d bytes)", len(content), maxPlanSize),
					CompletedAt: time.Now().UTC(),
				}, nil
			}
			if a.planFile == "" {
				return models.ToolResult{
					CallID:      call.ID,
					ToolName:    call.Name,
					Status:      models.CallStatusFailed,
					Error:       "no plan file initialized",
					CompletedAt: time.Now().UTC(),
				}, nil
			}
			if err := os.WriteFile(a.planFile, []byte(content), 0o644); err != nil {
				return models.ToolResult{
					CallID:      call.ID,
					ToolName:    call.Name,
					Status:      models.CallStatusFailed,
					Error:       fmt.Sprintf("write plan file: %v", err),
					CompletedAt: time.Now().UTC(),
				}, nil
			}
			return models.ToolResult{
				CallID:      call.ID,
				ToolName:    call.Name,
				Status:      models.CallStatusCompleted,
				Content:     fmt.Sprintf("Plan saved to %s", a.planFile),
				CompletedAt: time.Now().UTC(),
			}, nil
		},
	}
}

func (a *Agent) makeExitPlanModeTool() models.Tool {
	return models.Tool{
		Name:        "exit_plan_mode",
		Description: "Exit plan mode by submitting your plan for user approval. Reads the current plan file and presents it to the user. If no plan file exists, provide the plan as a parameter.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"plan": map[string]any{
					"type":        "string",
					"description": "Fallback plan content if the plan file is unavailable",
				},
			},
		},
		Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
			// Read plan from file, fall back to parameter.
			var plan string
			if a.planFile != "" {
				if data, err := os.ReadFile(a.planFile); err == nil {
					plan = string(data)
				}
			}
			if plan == "" {
				plan, _ = call.Arguments["plan"].(string)
			}
			if plan == "" {
				return models.ToolResult{
					CallID:      call.ID,
					ToolName:    call.Name,
					Status:      models.CallStatusFailed,
					Error:       "plan is empty — use write_plan to create a plan first, or provide it as a parameter",
					CompletedAt: time.Now().UTC(),
				}, nil
			}

			planLocation := "inline"
			if a.planFile != "" {
				planLocation = a.planFile
			}

			// Approval deferred to a gate (the mission loop's design
			// review): do NOT ask, and do NOT exit plan mode. The design
			// phase stays read-only until the gate passes the plan and the
			// loop moves the mission to its implementation phase — letting
			// the model exit here would hand it write tools on a plan no
			// reviewer has seen, which is the whole thing the gate replaces
			// (§5.2, C3). The gate does not care whether this was ever
			// called: it reads the plan file itself.
			if a.deferPlanApproval {
				return models.ToolResult{
					CallID:   call.ID,
					ToolName: call.Name,
					Status:   models.CallStatusCompleted,
					Content: fmt.Sprintf(
						"Plan submitted for independent design review (mission mode). You stay in plan mode; the review reads %s as it stands on disk. If it finds problems they arrive as your next message — rewrite the WHOLE plan with write_plan then, since that tool replaces the file rather than patching it.",
						planLocation),
					CompletedAt: time.Now().UTC(),
				}, nil
			}

			// Ask user to confirm the plan.
			ui := tools.UserInteractionFromContext(ctx)
			if ui != nil {
				answer, err := ui.AskQuestion(ctx,
					fmt.Sprintf("Implementation plan (%s):\n\n%s\n\nProceed with this plan?", planLocation, plan),
					[]string{"Yes, proceed", "Revise plan", "Cancel"},
				)
				if err != nil {
					return models.ToolResult{
						CallID:      call.ID,
						ToolName:    call.Name,
						Status:      models.CallStatusFailed,
						Error:       fmt.Sprintf("user interaction failed: %v", err),
						CompletedAt: time.Now().UTC(),
					}, nil
				}

				switch {
				case strings.HasPrefix(strings.ToLower(answer), "yes"), strings.HasPrefix(strings.ToLower(answer), "proceed"):
					a.exitPlanMode()
					return models.ToolResult{
						CallID:      call.ID,
						ToolName:    call.Name,
						Status:      models.CallStatusCompleted,
						Content:     fmt.Sprintf("Plan approved. You now have full tool access. The plan is at %s for reference.", a.planFile),
						CompletedAt: time.Now().UTC(),
					}, nil
				case strings.HasPrefix(strings.ToLower(answer), "cancel"):
					return models.ToolResult{
						CallID:      call.ID,
						ToolName:    call.Name,
						Status:      models.CallStatusFailed,
						Error:       "user cancelled the plan",
						CompletedAt: time.Now().UTC(),
					}, nil
				default:
					return models.ToolResult{
						CallID:      call.ID,
						ToolName:    call.Name,
						Status:      models.CallStatusCompleted,
						Content:     fmt.Sprintf("User requested revision: %s\n\nStay in plan mode, use write_plan to update, then call exit_plan_mode again.", answer),
						CompletedAt: time.Now().UTC(),
					}, nil
				}
			}

			// Non-interactive: auto-approve.
			a.exitPlanMode()
			return models.ToolResult{
				CallID:      call.ID,
				ToolName:    call.Name,
				Status:      models.CallStatusCompleted,
				Content:     fmt.Sprintf("Plan auto-approved (non-interactive mode). The plan is at %s for reference.", a.planFile),
				CompletedAt: time.Now().UTC(),
			}, nil
		},
	}
}

// appendPlanModePrompt adds plan mode instructions to the system prompt sections.
func (a *Agent) appendPlanModePrompt(sections []string) []string {
	if a.planMode.Load() {
		sections = append(sections, planModePrompt)
		if a.planFile != "" {
			sections = append(sections, fmt.Sprintf("Plan file: %s", a.planFile))
		}
	}
	return sections
}

package schedule

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

// Tool returns the single `schedule` tool. It is only registered in the
// deepai-bot process; the CLI never sees it (docs/BOT_SCHEDULER_DESIGN.md §0).
func Tool(svc *Service) models.Tool {
	return models.Tool{
		Name: "schedule",
		Description: "Create and manage timed tasks for this bot. One action per call. " +
			"Triggers: {\"type\":\"once\",\"in\":\"2h\"} or {\"type\":\"once\",\"at\":\"2026-11-07T22:00\"} " +
			"(a time without an offset is in the task's timezone), " +
			"{\"type\":\"interval\",\"every\":\"6h\",\"start_at\":\"2026-11-08T09:00\"}. " +
			"Prefer once.in for relative times. cron is NOT supported yet — use interval or once. " +
			"Every response includes the server's current time and the task's future trigger times: " +
			"ALWAYS restate the summary and next_runs to the user after creating or updating a task. " +
			"Periodic tasks created in an interactive turn require user confirmation first. " +
			"actions: create | list | get | update | pause | resume | delete | run_now | runs | validate.",
		Groups:       []string{"builtin", "schedule"},
		ParallelSafe: false, // mutates schedule state
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type": "string",
					"enum": []any{"create", "list", "get", "update", "pause", "resume", "delete", "run_now", "runs", "validate"},
				},
				"id":                map[string]any{"type": "string", "description": "schedule id for all actions except create/list/validate"},
				"name":              map[string]any{"type": "string"},
				"prompt":            map[string]any{"type": "string", "description": "instruction sent to the model at each trigger"},
				"trigger":           map[string]any{"type": "object"},
				"timezone":          map[string]any{"type": "string", "description": "IANA name, e.g. Asia/Taipei"},
				"session_mode":      map[string]any{"type": "string", "enum": []any{SessionDedicated, SessionNew, SessionOrigin}},
				"notify":            map[string]any{"type": "string", "enum": []any{NotifyAlways, NotifyOnFailure, NotifyNever}},
				"timeout":           map[string]any{"type": "string", "description": "per-run execution limit, e.g. 30m; excludes time spent waiting for an answer"},
				"answer_timeout":    map[string]any{"type": "string", "description": "how long an unattended run waits for an answer to one question, e.g. 30m"},
				"on_answer_timeout": map[string]any{"type": "string", "enum": []any{OnAnswerTimeoutAssume, OnAnswerTimeoutFail}, "description": "assume = let the model continue with stated assumptions; fail = end the run"},
				"max_auto_answers":  map[string]any{"type": "integer", "description": "max automatic answers in one run (default 3)"},
				"max_tool_calls":    map[string]any{"type": "integer", "description": "0 = use the bot-wide limit"},
				"model":             map[string]any{"type": "string"},
				"overlap":           map[string]any{"type": "string", "enum": []any{OverlapSkip, OverlapQueue, OverlapReplace}},
				"misfire":           map[string]any{"type": "string", "enum": []any{MisfireRunOnce, MisfireSkip}},
				"end_at":            map[string]any{"type": "string"},
				"max_runs":          map[string]any{"type": "integer"},
				"request_id":        map[string]any{"type": "string", "description": "idempotency key for run_now"},
				"limit":             map[string]any{"type": "integer"},
				"state":             map[string]any{"type": "string"},
			},
			"required": []any{"action"},
		},
		Handler: toolHandler(svc),
	}
}

func toolHandler(svc *Service) models.ToolHandler {
	return func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
		data, err := runToolAction(ctx, svc, call.Arguments)
		if err != nil {
			payload, _ := json.Marshal(map[string]any{
				"ok": false, "code": string(ErrorCode(err)), "error": err.Error(),
			})
			return models.ToolResult{
				CallID: call.ID, ToolName: call.Name, Status: models.CallStatusFailed,
				Content: string(payload), Error: err.Error(),
			}, err
		}
		payload, _ := json.Marshal(data)
		return models.ToolResult{
			CallID: call.ID, ToolName: call.Name, Status: models.CallStatusCompleted,
			Content: string(payload), Data: data,
		}, nil
	}
}

func runToolAction(ctx context.Context, svc *Service, args map[string]any) (map[string]any, error) {
	action := strArg(args, "action")
	cfg := svc.Config()
	sessionID := tools.ThreadIDFromContext(ctx)
	turnID := tools.TurnIDFromContext(ctx)
	parentScheduleID, _ := tools.ScheduleRunFromContext(ctx)

	result := map[string]any{
		"ok":        true,
		"action":    action,
		"scheduler": svc.Mode(),
		"now":       time.Now().UTC().Format(time.RFC3339),
		"timezone":  cfg.ConfiguredTimezone(),
	}
	if svc.Mode() == "standby" {
		result["note"] = "任务已保存，将由另一个 deepai-bot 实例执行"
	}

	switch action {
	case "create":
		in, err := createInputFromArgs(args, cfg, sessionID, turnID)
		if err != nil {
			return nil, err
		}
		if parentScheduleID != "" {
			if !cfg.AllowNested {
				return nil, NewError(CodeNotAllowedInScheduled, "creating schedules from inside a scheduled run is disabled")
			}
			in.State = StatePendingApproval
		}
		needsConfirm := cfg.Confirm == "ask" && in.State != StatePendingApproval && in.Trigger.IsPeriodic() && parentScheduleID == ""
		if needsConfirm {
			v, err := svc.Validate(ctx, in.Trigger, in.Timezone, 3)
			if err != nil {
				return nil, err
			}
			if err := confirm(ctx, cfg, "创建周期任务", in.Name, in.Prompt, v); err != nil {
				return nil, err
			}
			// Re-validate after the user took time to answer (the time may
			// have passed or the task limit may have been reached).
			v, err = svc.Validate(ctx, in.Trigger, in.Timezone, 3)
			if err != nil {
				return nil, err
			}
			in.Trigger = v.Trigger
		}
		sch, err := svc.Create(ctx, in)
		if err != nil {
			return nil, err
		}
		result["schedule"] = sch
		return result, nil

	case "list":
		list, err := svc.List(ctx, strArg(args, "state"), intArg(args, "limit"))
		if err != nil {
			return nil, err
		}
		result["schedules"] = list
		return result, nil

	case "get":
		sch, err := svc.Get(ctx, strArg(args, "id"))
		if err != nil {
			return nil, err
		}
		result["schedule"] = sch
		return result, nil

	case "update":
		id := strArg(args, "id")
		p, tr, tz, err := patchFromArgs(args, cfg)
		if err != nil {
			return nil, err
		}
		if cfg.Confirm == "ask" && (p.Trigger != nil || p.Timezone != nil || p.Prompt != nil) && parentScheduleID == "" {
			base, err := svc.Get(ctx, id)
			if err != nil {
				return nil, err
			}
			useTZ, useTR := base.Timezone, base.Trigger
			if tz != nil {
				useTZ = *tz
			}
			if tr != nil {
				useTR = *tr
			}
			v, err := svc.Validate(ctx, useTR, useTZ, 3)
			if err != nil {
				return nil, err
			}
			prompt := base.Prompt
			if p.Prompt != nil {
				prompt = *p.Prompt
			}
			if err := confirm(ctx, cfg, "修改任务", base.Name, prompt, v); err != nil {
				return nil, err
			}
		}
		sch, err := svc.Update(ctx, id, p)
		if err != nil {
			return nil, err
		}
		result["schedule"] = sch
		return result, nil

	case "pause", "resume":
		id := strArg(args, "id")
		var (
			sch Schedule
			err error
		)
		if action == "pause" {
			sch, err = svc.Pause(ctx, id)
		} else {
			sch, err = svc.Resume(ctx, id)
		}
		if err != nil {
			return nil, err
		}
		result["schedule"] = sch
		return result, nil

	case "delete":
		sch, err := svc.Delete(ctx, strArg(args, "id"), boolArg(args, "cancel"))
		if err != nil {
			return nil, err
		}
		result["schedule"] = sch
		return result, nil

	case "run_now":
		reqID := strArg(args, "request_id")
		if reqID == "" && turnID != "" {
			reqID = "turn_" + turnID
		}
		run, err := svc.RunNow(ctx, strArg(args, "id"), reqID)
		if err != nil {
			return nil, err
		}
		result["run"] = run
		return result, nil

	case "runs":
		runs, err := svc.Runs(ctx, strArg(args, "id"), intArg(args, "limit"))
		if err != nil {
			return nil, err
		}
		result["runs"] = runs
		return result, nil

	case "validate":
		tr, err := triggerFromArg(args["trigger"])
		if err != nil {
			return nil, err
		}
		v, err := svc.Validate(ctx, tr, strArg(args, "timezone"), intArg(args, "limit"))
		if err != nil {
			return nil, err
		}
		result["trigger"] = v.Trigger
		result["summary"] = v.Summary
		result["next_runs"] = v.NextRuns
		result["timezone"] = v.Timezone
		return result, nil
	}
	return nil, NewError(CodeBadRequest, "unknown action %q", action)
}

// confirm asks the user through the turn's UserInteraction. Nothing is
// written before confirmation, so a cancel/timeout leaves no half-created
// task behind.
func confirm(ctx context.Context, cfg Config, verb, name, prompt string, v Validation) error {
	ui := tools.UserInteractionFromContext(ctx)
	if ui == nil {
		return nil // no client to ask (autonomous turn): behave like confirm=auto
	}
	times := make([]string, 0, len(v.NextRuns))
	for _, t := range v.NextRuns {
		times = append(times, t.Format(time.RFC3339))
	}
	question := fmt.Sprintf("%s「%s」？\n规则：%s\n未来触发：%s\n将执行的指令：\n%s\n\n确认后任务才会生效。",
		verb, name, v.Summary, strings.Join(times, ", "), prompt)
	timeout := cfg.ConfirmTimeout
	askCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	answer, err := ui.AskQuestion(askCtx, question, []string{"确认创建", "取消"})
	if err != nil {
		return NewError(CodeConfirmationMissing, "confirmation not received: %v", err)
	}
	confirmed, denied := classifyConfirmation(answer)
	if denied {
		return NewError(CodeConfirmationMissing, "user cancelled the confirmation")
	}
	if !confirmed {
		return NewError(CodeConfirmationMissing, "confirmation not received: answer %q was not a confirmation", answer)
	}
	return nil
}

// classifyConfirmation interprets a free-text answer to the confirmation
// question. Exact option values win, then explicit phrases; English words
// are matched whole ("note"/"no problem, go ahead" must not be mistaken for
// "no"). Anything ambiguous stays unconfirmed, so a task is never created
// by accident.
func classifyConfirmation(answer string) (confirmed, denied bool) {
	a := strings.ToLower(strings.TrimSpace(answer))
	if a == "" {
		return false, false
	}
	switch a {
	case "取消", "cancel", "no", "n", "nope", "不用", "不要", "算了", "别创建":
		return false, true
	case "确认创建", "确认", "确定", "创建", "同意", "confirm", "yes", "y", "ok", "okay", "sure", "go ahead", "proceed":
		return true, false
	}
	for _, p := range []string{"取消", "不要创建", "不用创建", "别创建", "算了"} {
		if strings.Contains(a, p) {
			return false, true
		}
	}
	if hasWord(a, "cancel") || strings.Contains(a, "don't create") || strings.Contains(a, "do not create") {
		return false, true
	}
	for _, p := range []string{"确认", "创建", "同意", "确定"} {
		if strings.Contains(a, p) {
			return true, false
		}
	}
	for _, w := range []string{"yes", "ok", "okay", "sure", "confirm", "proceed", "go", "ahead"} {
		if hasWord(a, w) {
			return true, false
		}
	}
	return false, false
}

// hasWord splits ASCII words (Chinese phrases are handled with Contains).
func hasWord(s, word string) bool {
	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}) {
		if f == word {
			return true
		}
	}
	return false
}

func createInputFromArgs(args map[string]any, cfg Config, sessionID, turnID string) (CreateInput, error) {
	tr, err := triggerFromArg(args["trigger"])
	if err != nil {
		return CreateInput{}, err
	}
	tz := strArg(args, "timezone")
	if tz == "" {
		tz = cfg.ConfiguredTimezone()
	}
	endAt, err := timeArg(args["end_at"], tz)
	if err != nil {
		return CreateInput{}, err
	}
	return CreateInput{
		Name:     strArg(args, "name"),
		Prompt:   strArg(args, "prompt"),
		Trigger:  tr,
		Timezone: tz,
		Options: Options{
			SessionMode:     strArg(args, "session_mode"),
			Model:           strArg(args, "model"),
			Timeout:         strArg(args, "timeout"),
			Overlap:         strArg(args, "overlap"),
			Misfire:         strArg(args, "misfire"),
			Notify:          strArg(args, "notify"),
			AnswerTimeout:   strArg(args, "answer_timeout"),
			OnAnswerTimeout: strArg(args, "on_answer_timeout"),
			EndAt:           endAt,
			MaxRuns:         intArg(args, "max_runs"),
			MaxToolCalls:    intArg(args, "max_tool_calls"),
			AutoAnswers:     intArg(args, "max_auto_answers"),
		},
		CreatedBy: CreatedBy{Kind: "model", SessionID: sessionID, TurnID: turnID},
	}, nil
}

func patchFromArgs(args map[string]any, cfg Config) (Patch, *Trigger, *string, error) {
	var p Patch
	if v, ok := args["name"]; ok {
		p.Name = ptrString(strArg(map[string]any{"v": v}, "v"))
	}
	if v, ok := args["prompt"]; ok {
		p.Prompt = ptrString(strArg(map[string]any{"v": v}, "v"))
	}
	if v, ok := args["timezone"]; ok {
		tz := strArg(map[string]any{"v": v}, "v")
		p.Timezone = &tz
	}
	if v, ok := args["trigger"]; ok {
		tr, err := triggerFromArg(v)
		if err != nil {
			return p, nil, nil, err
		}
		p.Trigger = &tr
	}
	for key, dst := range map[string]**string{
		"session_mode": &p.SessionMode, "model": &p.Model, "timeout": &p.Timeout,
		"overlap": &p.Overlap, "misfire": &p.Misfire, "notify": &p.Notify,
		"answer_timeout": &p.AnswerTimeout, "on_answer_timeout": &p.OnAnswerTimeout,
	} {
		if v, ok := args[key]; ok {
			s := strArg(map[string]any{"v": v}, "v")
			*dst = &s
		}
	}
	if v, ok := args["end_at"]; ok {
		if v == nil || strArg(map[string]any{"v": v}, "v") == "" {
			p.ClearEndAt = true
		} else {
			tz := cfg.ConfiguredTimezone()
			t, err := timeArg(v, tz)
			if err != nil {
				return p, p.Trigger, p.Timezone, err
			}
			p.EndAt = t
		}
	}
	if v, ok := args["max_runs"]; ok {
		n := intArg(map[string]any{"v": v}, "v")
		p.MaxRuns = &n
	}
	if v, ok := args["max_tool_calls"]; ok {
		n := intArg(map[string]any{"v": v}, "v")
		p.MaxToolCalls = &n
	}
	if v, ok := args["max_auto_answers"]; ok {
		n := intArg(map[string]any{"v": v}, "v")
		p.MaxAutoAnswers = &n
	}
	return p, p.Trigger, p.Timezone, nil
}

func ptrString(s string) *string { return &s }

func strArg(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func intArg(m map[string]any, key string) int {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	case string:
		var n int
		_, _ = fmt.Sscanf(v, "%d", &n)
		return n
	}
	return 0
}

func boolArg(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	switch v := m[key].(type) {
	case bool:
		return v
	case string:
		return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
	}
	return false
}

func triggerFromArg(v any) (Trigger, error) {
	if v == nil {
		return Trigger{}, NewError(CodeInvalidTrigger, "trigger is required")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return Trigger{}, NewError(CodeInvalidTrigger, "trigger must be an object")
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return Trigger{}, NewError(CodeInvalidTrigger, "trigger: %v", err)
	}
	var tr Trigger
	if err := json.Unmarshal(raw, &tr); err != nil {
		return Trigger{}, NewError(CodeInvalidTrigger, "trigger: %v", err)
	}
	return tr, nil
}

func timeArg(v any, tz string) (*time.Time, error) {
	if v == nil {
		return nil, nil
	}
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return nil, nil
	}
	loc, err := Location(tz)
	if err != nil {
		return nil, err
	}
	t, err := parseTimeSpec(s, loc)
	if err != nil {
		return nil, NewError(CodeBadRequest, "invalid time %q: %v", s, err)
	}
	return &t, nil
}

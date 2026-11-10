package schedule

import (
	"context"
	"testing"
	"time"
)

func strPtr(s string) *string { return &s }
func intPtr(n int) *int       { return &n }

func TestAnswerTimeoutOptionsAndInteraction(t *testing.T) {
	ctx := context.Background()
	svc, store := newTestService(t, newFakeClock(testNow), func(c *Config) {
		c.DefaultAnswerTimeout = time.Minute
		c.DefaultMaxAutoAnswers = 3
	})
	base := func(opts Options) CreateInput {
		return CreateInput{
			Name: "问答任务", Prompt: "请提问",
			Trigger:  Trigger{Type: TriggerOnce, In: "1h"},
			Timezone: "UTC", Options: opts,
		}
	}
	if _, err := svc.Create(ctx, base(Options{OnAnswerTimeout: "bogus"})); ErrorCode(err) != CodeBadRequest {
		t.Fatalf("invalid on_answer_timeout error = %v", err)
	}
	sch, err := svc.Create(ctx, base(Options{
		AnswerTimeout: "2m", OnAnswerTimeout: OnAnswerTimeoutFail,
		AutoAnswers: 2, MaxToolCalls: 9,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if sch.Options.AnswerTimeout != "2m" || sch.Options.OnAnswerTimeout != OnAnswerTimeoutFail ||
		sch.Options.AutoAnswers != 2 || sch.Options.MaxToolCalls != 9 {
		t.Fatalf("options = %+v", sch.Options)
	}
	// Default policy is "assume" when omitted.
	def, err := svc.Create(ctx, base(Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if def.Options.OnAnswerTimeout != OnAnswerTimeoutAssume ||
		def.Options.AnswerTimeout != time.Minute.String() ||
		def.Options.AutoAnswers != 3 {
		t.Fatalf("default options = %+v", def.Options)
	}
	if _, err := svc.Update(ctx, sch.ID, Patch{OnAnswerTimeout: strPtr("nope")}); ErrorCode(err) != CodeBadRequest {
		t.Fatalf("patch validation error = %v", err)
	}
	patched, err := svc.Update(ctx, sch.ID, Patch{
		AnswerTimeout: strPtr("3m"), OnAnswerTimeout: strPtr(OnAnswerTimeoutAssume),
		MaxAutoAnswers: intPtr(5), MaxToolCalls: intPtr(4),
	})
	if err != nil {
		t.Fatal(err)
	}
	if patched.Options.AnswerTimeout != "3m" || patched.Options.OnAnswerTimeout != OnAnswerTimeoutAssume ||
		patched.Options.AutoAnswers != 5 || patched.Options.MaxToolCalls != 4 {
		t.Fatalf("patched options = %+v", patched.Options)
	}

	// A Run's question lifecycle: running → waiting_input → running, with
	// auto-answers counted, and no writes after it reaches a terminal state.
	runID := "run_interaction"
	if err := store.InsertRun(ctx, Run{
		ID: runID, ScheduleID: sch.ID, Seq: 1, Trigger: FireManual,
		ScheduledFor: testNow, Status: RunRunning,
	}); err != nil {
		t.Fatal(err)
	}
	r, err := svc.UpdateRunInteraction(ctx, runID, RunWaitingInput, 0)
	if err != nil || r.Status != RunWaitingInput {
		t.Fatalf("waiting_input = %+v, %v", r, err)
	}
	r, err = svc.UpdateRunInteraction(ctx, runID, "", 1)
	if err != nil || r.Status != RunWaitingInput || r.AutoAnswers != 1 {
		t.Fatalf("auto answer while waiting = %+v, %v", r, err)
	}
	r, err = svc.UpdateRunInteraction(ctx, runID, RunRunning, 1)
	if err != nil || r.Status != RunRunning || r.AutoAnswers != 2 {
		t.Fatalf("back to running = %+v, %v", r, err)
	}
	if _, err := svc.FinishRun(ctx, runID, Outcome{Status: RunCompleted}); err != nil {
		t.Fatal(err)
	}
	r, err = svc.UpdateRunInteraction(ctx, runID, RunWaitingInput, 1)
	if err != nil || r.Status != RunCompleted || r.AutoAnswers != 2 {
		t.Fatalf("late callback overwrote terminal run: %+v, %v", r, err)
	}
}

func TestAnswerTimeoutParamsFromMap(t *testing.T) {
	cfg := DefaultConfig()
	in, err := CreateInputFromMap(map[string]any{
		"name": "问答任务", "prompt": "请提问",
		"trigger":           map[string]any{"type": "once", "in": "1h"},
		"answer_timeout":    "5m",
		"on_answer_timeout": "fail",
		"max_auto_answers":  float64(2),
		"max_tool_calls":    float64(7),
	}, cfg, "sess_1", "turn_1")
	if err != nil {
		t.Fatal(err)
	}
	if in.Options.AnswerTimeout != "5m" || in.Options.OnAnswerTimeout != OnAnswerTimeoutFail ||
		in.Options.AutoAnswers != 2 || in.Options.MaxToolCalls != 7 {
		t.Fatalf("create args = %+v", in.Options)
	}
	p, err := PatchFromMap(map[string]any{
		"answer_timeout":    "8m",
		"on_answer_timeout": "assume",
		"max_auto_answers":  float64(4),
	}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if p.AnswerTimeout == nil || *p.AnswerTimeout != "8m" ||
		p.OnAnswerTimeout == nil || *p.OnAnswerTimeout != OnAnswerTimeoutAssume ||
		p.MaxAutoAnswers == nil || *p.MaxAutoAnswers != 4 {
		t.Fatalf("patch args = %+v", p)
	}
}

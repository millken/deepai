package memory

import "testing"

func TestPreferenceScheduler_RecordTurn(t *testing.T) {
	ps := NewPreferenceScheduler()

	// First 9 turns should not trigger.
	for i := 0; i < 9; i++ {
		if ps.RecordTurn() {
			t.Fatalf("turn %d: unexpected trigger", i+1)
		}
	}
	// Turn 10 should trigger (floor interval).
	if !ps.RecordTurn() {
		t.Fatal("turn 10: expected trigger")
	}

	// Turn 1 after reset should not trigger.
	if ps.RecordTurn() {
		t.Fatal("turn 1 after reset: unexpected trigger")
	}
}

func TestPreferenceScheduler_ConsecutiveNegResets(t *testing.T) {
	ps := NewPreferenceScheduler()

	// Record 2 negative feedbacks — should not trigger.
	ps.RecordNegativeFeedback()
	ps.RecordNegativeFeedback()
	if ps.RecordTurn() {
		t.Fatal("2 consec neg: unexpected trigger")
	}

	// 3rd negative should trigger.
	ps.RecordNegativeFeedback()
	if !ps.RecordTurn() {
		t.Fatal("3 consec neg: expected trigger")
	}
	// Turn count was reset.
	if ps.RecordTurn() {
		t.Fatal("turn 1 after trigger: unexpected trigger")
	}

	// Non-negative feedback should reset counter.
	ps.RecordNegativeFeedback()
	ps.RecordNonNegativeFeedback()
	ps.RecordNegativeFeedback()
	ps.RecordNegativeFeedback()
	if ps.RecordTurn() {
		t.Fatal("2 consec neg after reset: unexpected trigger")
	}
	ps.RecordNegativeFeedback()
	if !ps.RecordTurn() {
		t.Fatal("3 consec neg after partial reset: expected trigger")
	}
}

func TestPreferenceScheduler_LanguageSwitch(t *testing.T) {
	ps := NewPreferenceScheduler()

	// First message sets baseline.
	ps.CheckLanguageSwitch("你好世界")
	if ps.RecordTurn() {
		t.Fatal("first message: unexpected trigger")
	}

	// Same language — no switch.
	ps.CheckLanguageSwitch("继续工作")
	if ps.RecordTurn() {
		t.Fatal("same language: unexpected trigger")
	}

	// Switch to English — should trigger.
	ps.CheckLanguageSwitch("hello world")
	if !ps.RecordTurn() {
		t.Fatal("language switch zh→en: expected trigger")
	}
	// Switch consumed.
	if ps.RecordTurn() {
		t.Fatal("after switch consumed: unexpected trigger")
	}
}

func TestPreferenceScheduler_ToolShift(t *testing.T) {
	ps := NewPreferenceScheduler()

	// Build up history: 10 turns of tool A.
	for i := 0; i < 10; i++ {
		ps.RecordToolCalls([]ToolCallInfo{{Name: "tool_a"}})
	}

	// Turn 11: still tool A — no shift.
	ps.RecordToolCalls([]ToolCallInfo{{Name: "tool_a"}})
	if ps.RecordTurn() {
		t.Fatal("same tool: unexpected shift")
	}

	// Turn 12: new tool B (0 previous calls) — shift.
	ps.RecordToolCalls([]ToolCallInfo{{Name: "tool_b"}})
	if !ps.RecordTurn() {
		t.Fatal("new tool_b: expected shift")
	}
	// Shift consumed.
	if ps.RecordTurn() {
		t.Fatal("after shift consumed: unexpected trigger")
	}
}

func TestPreferenceScheduler_ToolShift_InsufficientHistory(t *testing.T) {
	ps := NewPreferenceScheduler()

	// Only 5 turns of history — not enough to detect shift.
	for i := 0; i < 5; i++ {
		ps.RecordToolCalls([]ToolCallInfo{{Name: "tool_a"}})
	}

	// New tool B should NOT trigger shift (need > 10 cumulative calls).
	ps.RecordToolCalls([]ToolCallInfo{{Name: "tool_b"}})
	if ps.RecordTurn() {
		t.Fatal("insufficient history: unexpected shift")
	}
}

func TestPreferenceScheduler_ToolShift_Cooldown(t *testing.T) {
	ps := NewPreferenceScheduler()

	// Build up history: 11 turns of tool_a (need totalCalls > 10 for shift detection).
	for i := 0; i < 11; i++ {
		ps.RecordToolCalls([]ToolCallInfo{{Name: "tool_a"}})
	}

	// Turn 12: new tool_b — shift should trigger.
	ps.RecordToolCalls([]ToolCallInfo{{Name: "tool_b"}})
	if !ps.RecordTurn() {
		t.Fatal("first tool_b: expected shift")
	}

	// Turn 12-15 (within cooldown): tool_b again — should NOT trigger.
	for i := 0; i < toolShiftCooldownTurns-1; i++ {
		ps.RecordToolCalls([]ToolCallInfo{{Name: "tool_b"}})
		if ps.RecordTurn() {
			t.Fatalf("turn %d within cooldown: unexpected shift", i+2)
		}
	}

	// Turn after cooldown expires: new tool_c — shift should trigger again.
	ps.RecordToolCalls([]ToolCallInfo{{Name: "tool_c"}})
	if !ps.RecordTurn() {
		t.Fatal("after cooldown: tool_c expected shift")
	}
}

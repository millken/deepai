package agent

import (
	"context"
	"strings"
	"testing"
)

func TestTurnInjectionExtra_KeepsSystemPromptByteStable(t *testing.T) {
	now := "当前时间：2026-11-24 20:00（Asia/Taipei）"
	a := New(AgentConfig{
		SystemPrompt:       "Static bot instructions.",
		TurnInjectionExtra: func() string { return now },
	})

	sp1 := a.BuildSystemPrompt()
	inj1 := a.buildTurnInjection(context.Background(), "s1", nil)
	if strings.Contains(sp1, "2026-11-24 20:00") {
		t.Fatalf("volatile TurnInjectionExtra leaked into system prompt:\n%s", sp1)
	}
	if !strings.Contains(inj1.Content, "2026-11-24 20:00") {
		t.Fatalf("buildTurnInjection missing TurnInjectionExtra:\n%s", inj1.Content)
	}

	now = "当前时间：2026-11-24 20:05（Asia/Taipei）"
	sp2 := a.BuildSystemPrompt()
	inj2 := a.buildTurnInjection(context.Background(), "s1", nil)
	if sp1 != sp2 {
		t.Fatalf("system prompt drifted across volatile ticks:\n1: %s\n2: %s", sp1, sp2)
	}
	if !strings.Contains(inj2.Content, "2026-11-24 20:05") {
		t.Fatalf("second buildTurnInjection missing updated TurnInjectionExtra:\n%s", inj2.Content)
	}
}

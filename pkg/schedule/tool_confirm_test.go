package schedule

import (
	"context"
	"testing"

	"github.com/millken/deepai/pkg/tools"
)

type askRecord struct {
	question string
	options  []string
}

// stubUI records every AskQuestion and answers from a canned queue.
type stubUI struct {
	answers []string
	asks    []askRecord
}

func (u *stubUI) AskQuestion(ctx context.Context, question string, options []string) (string, error) {
	u.asks = append(u.asks, askRecord{question: question, options: options})
	if len(u.answers) == 0 {
		return "取消", nil
	}
	a := u.answers[0]
	u.answers = u.answers[1:]
	return a, nil
}

// PR #43 round-3 L2: the update confirmation mirrors create's semantics —
// (1) a once task's update must NOT block on confirmation (only periodic
// rules confirm, design §0); (2) the update dialog's confirm button reads
// 确认修改, not the hardcoded 确认创建; (3) classifyConfirmation accepts the
// new label so the answer actually confirms.
func TestUpdateConfirmPeriodicOnlyAndLabels(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t, newFakeClock(testNow), func(c *Config) {
		c.Confirm = "ask"
		c.Timezone = "UTC"
	})
	once, err := svc.Create(ctx, CreateInput{
		Name: "一次性", Prompt: "做一次",
		Trigger: Trigger{Type: TriggerOnce, In: "1h"}, Timezone: "UTC",
	})
	if err != nil {
		t.Fatal(err)
	}
	periodic, err := svc.Create(ctx, CreateInput{
		Name: "每小时", Prompt: "巡检",
		Trigger: Trigger{Type: TriggerInterval, Every: "1h"}, Timezone: "UTC",
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("once update does not confirm", func(t *testing.T) {
		ui := &stubUI{}
		if _, err := runToolAction(tools.WithUserInteraction(ctx, ui), svc, map[string]any{
			"action": "update", "id": once.ID, "prompt": "做一次，改了文案",
		}); err != nil {
			t.Fatalf("once-task update blocked on confirmation: %v", err)
		}
		if len(ui.asks) != 0 {
			t.Fatalf("once-task update asked for confirmation: %+v", ui.asks)
		}
	})

	t.Run("periodic update confirms with 确认修改 label", func(t *testing.T) {
		ui := &stubUI{answers: []string{"确认修改"}}
		res, err := runToolAction(tools.WithUserInteraction(ctx, ui), svc, map[string]any{
			"action": "update", "id": periodic.ID, "prompt": "巡检，改了文案",
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(ui.asks) != 1 {
			t.Fatalf("asks = %d, want 1", len(ui.asks))
		}
		opts := ui.asks[0].options
		if len(opts) != 2 || opts[0] != "确认修改" || opts[1] != "取消" {
			t.Fatalf("update confirm options = %v, want [确认修改 取消]", opts)
		}
		sch, _ := res["schedule"].(Schedule)
		if sch.Prompt != "巡检，改了文案" {
			t.Fatalf("update did not apply after confirmation: %q", sch.Prompt)
		}
	})

	t.Run("classifyConfirmation accepts 确认修改", func(t *testing.T) {
		if confirmed, denied := classifyConfirmation("确认修改"); !confirmed || denied {
			t.Fatal(`classifyConfirmation("确认修改") must confirm`)
		}
		if confirmed, denied := classifyConfirmation("确认创建"); !confirmed || denied {
			t.Fatal(`classifyConfirmation("确认创建") must stay confirmed (create path)`)
		}
	})
}

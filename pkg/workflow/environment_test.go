package workflow

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestEnvironment_DirectedMessage(t *testing.T) {
	env := NewEnvironment()
	defer env.Close()

	env.Register(Subscription{Role: "coder"})
	env.Register(Subscription{Role: "reviewer"})

	msg := AgentMessage{From: "user", To: "coder", Type: MsgTypeUserRequest, Content: "hello"}
	if err := env.Publish(context.Background(), msg); err != nil {
		t.Fatal(err)
	}

	received, err := env.Receive(context.Background(), "coder")
	if err != nil {
		t.Fatal(err)
	}
	if received.Content != "hello" {
		t.Errorf("Content = %q, want hello", received.Content)
	}
	if received.From != "user" {
		t.Errorf("From = %q, want user", received.From)
	}
}

func TestEnvironment_Broadcast(t *testing.T) {
	env := NewEnvironment()
	defer env.Close()

	env.Register(Subscription{Role: "security", MsgTypes: []string{MsgTypeCodeChange}})
	env.Register(Subscription{Role: "arch", MsgTypes: []string{MsgTypeCodeChange}})
	env.Register(Subscription{Role: "pm", MsgTypes: []string{MsgTypePRD}}) // different subscription

	msg := AgentMessage{From: "coder", Type: MsgTypeCodeChange, Content: "changed code"}
	if err := env.Publish(context.Background(), msg); err != nil {
		t.Fatal(err)
	}

	// security and arch should receive
	for _, role := range []string{"security", "arch"} {
		received, err := env.Receive(context.Background(), role)
		if err != nil {
			t.Fatalf("role %q: %v", role, err)
		}
		if received.Content != "changed code" {
			t.Errorf("role %q: Content = %q", role, received.Content)
		}
	}

	// pm should not receive code_change
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := env.Receive(ctx, "pm")
	if err == nil {
		t.Error("pm should not have received code_change")
	}
}

func TestEnvironment_NoSubscriberDoesNotBlock(t *testing.T) {
	env := NewEnvironment()
	defer env.Close()

	msg := AgentMessage{From: "coder", Type: MsgTypeCodeChange, Content: "orphan"}
	// No subscribers — should not block
	if err := env.Publish(context.Background(), msg); err != nil {
		t.Fatal(err)
	}

	history := env.History()
	if len(history) != 1 {
		t.Fatalf("History len = %d, want 1", len(history))
	}
	if history[0].Content != "orphan" {
		t.Errorf("History[0].Content = %q", history[0].Content)
	}
}

func TestEnvironment_HistoryFilters(t *testing.T) {
	env := NewEnvironment()
	defer env.Close()

	env.Register(Subscription{Role: "r1"})

	msgs := []AgentMessage{
		{From: "coder", Type: MsgTypeCodeChange, Content: "c1"},
		{From: "reviewer", Type: MsgTypeReviewResult, Content: "r1"},
		{From: "coder", Type: MsgTypeCodeChange, Content: "c2"},
	}
	for _, m := range msgs {
		env.Publish(context.Background(), m)
	}

	t.Run("filter by From", func(t *testing.T) {
		filtered := env.History(From("coder"))
		if len(filtered) != 2 {
			t.Errorf("got %d, want 2", len(filtered))
		}
	})

	t.Run("filter by Type", func(t *testing.T) {
		filtered := env.History(Type(MsgTypeReviewResult))
		if len(filtered) != 1 {
			t.Errorf("got %d, want 1", len(filtered))
		}
	})

	t.Run("filter by From and Type", func(t *testing.T) {
		filtered := env.History(From("coder"), Type(MsgTypeCodeChange))
		if len(filtered) != 2 {
			t.Errorf("got %d, want 2", len(filtered))
		}
	})

	t.Run("no filter returns all", func(t *testing.T) {
		all := env.History()
		if len(all) != 3 {
			t.Errorf("got %d, want 3", len(all))
		}
	})
}

func TestEnvironment_ContextCancel(t *testing.T) {
	env := NewEnvironment()
	defer env.Close()

	env.Register(Subscription{Role: "waiter"})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := env.Receive(ctx, "waiter")
	if err == nil {
		t.Error("expected timeout error")
	}
}

func TestEnvironment_Close(t *testing.T) {
	env := NewEnvironment()
	env.Register(Subscription{Role: "r1"})
	env.Close()

	err := env.Publish(context.Background(), AgentMessage{From: "x", Type: "test"})
	if err == nil {
		t.Error("expected error on closed environment")
	}
}

func TestEnvironment_RegisterValidation(t *testing.T) {
	env := NewEnvironment()
	defer env.Close()

	err := env.Register(Subscription{Role: ""})
	if err == nil {
		t.Error("expected error for empty role")
	}
}

func TestEnvironment_ReceiveUnregisteredRole(t *testing.T) {
	env := NewEnvironment()
	defer env.Close()

	_, err := env.Receive(context.Background(), "nonexistent")
	if err == nil {
		t.Error("expected error for unregistered role")
	}
}

func TestEnvironment_ConcurrentPublish(t *testing.T) {
	env := NewEnvironment(WithInboxSize(256))
	defer env.Close()

	env.Register(Subscription{Role: "sink"})

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			env.Publish(context.Background(), AgentMessage{
				From:    "sender",
				To:      "sink",
				Type:    "test",
				Content: fmt.Sprintf("msg-%d", n),
			})
		}(i)
	}
	wg.Wait()

	history := env.History()
	if len(history) != 100 {
		t.Errorf("History len = %d, want 100", len(history))
	}
}

func TestEnvironment_MaxHistory(t *testing.T) {
	env := NewEnvironment(WithMaxHistory(5))
	defer env.Close()

	env.Register(Subscription{Role: "r1"})

	for i := 0; i < 10; i++ {
		env.Publish(context.Background(), AgentMessage{From: "x", Type: "test", Content: fmt.Sprintf("msg-%d", i)})
	}

	history := env.History()
	if len(history) != 5 {
		t.Fatalf("History len = %d, want 5", len(history))
	}
	// Should keep the last 5 messages (msg-5 through msg-9)
	if history[0].Content != "msg-5" {
		t.Errorf("first history entry = %q, want msg-5", history[0].Content)
	}
}

func TestEnvironment_SubscriptionFiltering(t *testing.T) {
	env := NewEnvironment()
	defer env.Close()

	env.Register(Subscription{Role: "only-review", MsgTypes: []string{MsgTypeReviewResult}})
	env.Register(Subscription{Role: "all-types"}) // no filter

	// Publish code_change — only "all-types" should receive
	env.Publish(context.Background(), AgentMessage{From: "coder", Type: MsgTypeCodeChange, Content: "code"})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	received, err := env.Receive(ctx, "all-types")
	if err != nil {
		t.Fatal(err)
	}
	if received.Content != "code" {
		t.Errorf("Content = %q, want code", received.Content)
	}
}

func TestEnvironment_RegisterReplaces(t *testing.T) {
	env := NewEnvironment()
	defer env.Close()

	env.Register(Subscription{Role: "r1", MsgTypes: []string{"a"}})

	// Reader on old inbox
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	go func() {
		env.Receive(ctx, "r1") // will get old inbox closed
	}()

	// Re-register closes old inbox
	env.Register(Subscription{Role: "r1", MsgTypes: []string{"b"}})

	// Should work with new inbox
	env.Publish(context.Background(), AgentMessage{From: "x", Type: "b", Content: "new"})
	received, err := env.Receive(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	if received.Content != "new" {
		t.Errorf("Content = %q, want new", received.Content)
	}
}

func TestEnvironment_UnregisterThenReceive(t *testing.T) {
	env := NewEnvironment()
	env.Register(Subscription{Role: "r1"})
	env.Unregister("r1")

	_, err := env.Receive(context.Background(), "r1")
	if err == nil {
		t.Error("expected error after unregister")
	}
}

func TestEnvironment_PublishToUnregisteredRole(t *testing.T) {
	env := NewEnvironment()
	defer env.Close()

	// Publish directed to role that doesn't exist — should not block
	err := env.Publish(context.Background(), AgentMessage{From: "x", To: "ghost", Type: "test"})
	if err != nil {
		t.Fatal(err)
	}
}

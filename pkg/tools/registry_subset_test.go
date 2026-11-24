package tools

import (
	"context"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

func dummyTool(name string) models.Tool {
	return models.Tool{
		Name: name,
		Handler: func(context.Context, models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{Status: models.CallStatusCompleted, Content: "ok"}, nil
		},
	}
}

func TestRegistrySubsetAndValidateAllowlist(t *testing.T) {
	reg := NewRegistry()
	for _, name := range []string{"read_file", "grep", "bash", "mcp__github__list_prs", "mcp__github__get_pr", "mcp__slack__post"} {
		if err := reg.Register(dummyTool(name)); err != nil {
			t.Fatal(err)
		}
	}

	// Empty allowlist returns same registry unrestricted.
	if got := reg.Subset(nil); got != reg {
		t.Fatal("empty allowlist should return original registry")
	}

	sub := reg.Subset([]string{"read_file", "mcp__github__*"})
	if sub.Get("read_file") == nil || sub.Get("mcp__github__list_prs") == nil || sub.Get("mcp__github__get_pr") == nil {
		t.Fatalf("subset missing expected tools: %+v", sub.List())
	}
	if sub.Get("bash") != nil || sub.Get("grep") != nil || sub.Get("mcp__slack__post") != nil {
		t.Fatalf("subset leaked disallowed tools: %+v", sub.List())
	}
	// Original registry untouched.
	if reg.Get("bash") == nil {
		t.Fatal("Subset mutated original registry")
	}

	// ValidateAllowlist flags unknown exact names and unmatched prefixes.
	if missing := reg.ValidateAllowlist([]string{"read_file", "mcp__github__*"}); len(missing) != 0 {
		t.Fatalf("expected no missing patterns, got %v", missing)
	}
	missing := reg.ValidateAllowlist([]string{"read_file", "no_such_tool", "mcp__jira__*", "*"})
	if len(missing) != 3 || missing[0] != "no_such_tool" || missing[1] != "mcp__jira__*" || missing[2] != "*" {
		t.Fatalf("missing = %v, want [no_such_tool mcp__jira__* *]", missing)
	}
	if MatchAllowlist("read_file", []string{"*"}) {
		t.Fatal("bare * must not match tools")
	}
}

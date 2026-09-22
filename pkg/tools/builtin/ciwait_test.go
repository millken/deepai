package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

func TestParseRunState(t *testing.T) {
	cases := []struct {
		stdout           string
		wantDone, wantOK bool
		wantErr          bool
	}{
		{`{"status":"completed","conclusion":"success"}`, true, true, false},
		{`{"status":"completed","conclusion":"failure"}`, true, false, false},
		{`{"status":"in_progress","conclusion":""}`, false, false, false},
		{`{"status":"queued","conclusion":""}`, false, false, false},
		{`not json`, false, false, true},
	}
	for _, c := range cases {
		done, ok, err := parseRunState(c.stdout)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseRunState(%q): want error", c.stdout)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseRunState(%q): %v", c.stdout, err)
			continue
		}
		if done != c.wantDone || ok != c.wantOK {
			t.Errorf("parseRunState(%q) = (%v,%v), want (%v,%v)", c.stdout, done, ok, c.wantDone, c.wantOK)
		}
	}
}

func TestIsGhLookupError(t *testing.T) {
	for _, s := range []string{
		"gh: Could not resolve to a Pull Request",
		"graphql: Could not resolve to a node",
		"HTTP 404: Not Found",
		"no pull requests found",
	} {
		if !isGhLookupError(s) {
			t.Errorf("isGhLookupError(%q) = false, want true", s)
		}
	}
	if isGhLookupError("some checks were pending") {
		t.Error("pending message misclassified as lookup error")
	}
}

func ciWaitCall(t *testing.T, args map[string]any) error {
	t.Helper()
	_, err := CIWaitHandler(context.Background(), models.ToolCall{
		ID:        "c1",
		Name:      "ci_wait",
		Arguments: args,
	})
	return err
}

func TestCIWait_ArgumentValidation(t *testing.T) {
	if err := ciWaitCall(t, map[string]any{}); err == nil || !strings.Contains(err.Error(), "pr or run_id is required") {
		t.Errorf("missing target: %v", err)
	}
	if err := ciWaitCall(t, map[string]any{"pr": 80.0, "run_id": "123"}); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("both targets: %v", err)
	}
	if err := ciWaitCall(t, map[string]any{"pr": "eighty"}); err == nil || !strings.Contains(err.Error(), "must be a number") {
		t.Errorf("bad pr string: %v", err)
	}
}

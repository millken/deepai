package chat

import (
	"strings"
	"testing"
)

func TestFilterExternalComments(t *testing.T) {
	comments := []prComment{
		{ID: "1", Author: "millken", Body: "deepai's own review post"},
		{ID: "2", Author: "cursor", Body: "cursor nit"},
		{ID: "3", Author: "Millken", Body: "own login, different case"},
		{ID: "4", Author: "alice", Body: "human asks about tests"},
	}

	external := filterExternalComments(comments, "millken")
	if len(external) != 2 || external[0].ID != "2" || external[1].ID != "4" {
		ids := make([]string, len(external))
		for i, c := range external {
			ids[i] = c.ID
		}
		t.Errorf("external = %v, want [2 4] (own login case-insensitive)", ids)
	}
}

func TestFilterExternalComments_EmptyOwnLogin(t *testing.T) {
	comments := []prComment{{ID: "1", Author: "cursor", Body: "nit"}}
	// An empty login cannot classify anything — callers treat it as "passthrough
	// disabled" before reaching here, but the filter itself must not suddenly
	// pass everything through as "external".
	if got := filterExternalComments(comments, ""); len(got) != 1 {
		t.Errorf("empty ownLogin external = %d comments; classification is the caller's job", len(got))
	}
}

func TestPRStatePersistRoundTrip(t *testing.T) {
	dir := t.TempDir()

	st, err := newPRState(dir, 42, "millken/jp-small", "feature/w5", "develop", "https://github.com/millken/jp-small/pull/42", "ledger export")
	if err != nil {
		t.Fatalf("newPRState: %v", err)
	}
	if st.Status != prStatusReviewing || st.Round != 1 {
		t.Fatalf("fresh state = %+v, want reviewing round 1", st)
	}

	st.Round = 3
	if err := st.setStatus(dir, prStatusAwaitingMerge); err != nil {
		t.Fatalf("setStatus: %v", err)
	}

	back, err := openPRState(dir, 42)
	if err != nil {
		t.Fatalf("openPRState: %v", err)
	}
	if back.Status != prStatusAwaitingMerge || back.Round != 3 || back.Number != 42 || back.Brief != "ledger export" {
		t.Fatalf("reloaded state = %+v", back)
	}
	if !back.UpdatedAt.After(back.CreatedAt) && !back.UpdatedAt.Equal(back.CreatedAt) {
		t.Fatalf("UpdatedAt %v not >= CreatedAt %v", back.UpdatedAt, back.CreatedAt)
	}
}

func TestActivePRStatesFiltersTerminal(t *testing.T) {
	dir := t.TempDir()
	mk := func(n int, status prStatus) {
		st, err := newPRState(dir, n, "", "", "", "", "")
		if err != nil {
			t.Fatalf("newPRState(%d): %v", n, err)
		}
		if err := st.setStatus(dir, status); err != nil {
			t.Fatalf("setStatus(%d): %v", n, err)
		}
	}
	mk(1, prStatusMerged)
	mk(2, prStatusReviewing)
	mk(3, prStatusAborted)
	mk(4, prStatusAwaitingCI)

	active := activePRStates(dir)
	if len(active) != 2 || active[0].Number != 2 || active[1].Number != 4 {
		var nums []int
		for _, a := range active {
			nums = append(nums, a.Number)
		}
		t.Fatalf("activePRStates = %v, want [2 4]", nums)
	}
}

func TestClassifyChecksCode(t *testing.T) {
	cases := []struct {
		code     int
		stderr   string
		wantDone bool
		wantOK   bool
		wantLErr bool
	}{
		{0, "", true, true, false},
		{8, "", false, false, false},
		{127, "", false, false, true},
		{1, "gh: Could not resolve to a Pull Request", false, false, true},
		{1, "HTTP 404: Not Found", false, false, true},
		{1, "some checks failed", true, false, false},
		{2, "", true, false, false},
	}
	for _, c := range cases {
		done, ok, lerr := classifyChecksCode(c.code, c.stderr)
		if done != c.wantDone || ok != c.wantOK || (lerr != "") != c.wantLErr {
			t.Errorf("classifyChecksCode(%d, %q) = (%v,%v,%q), want (%v,%v,lerr=%v)",
				c.code, c.stderr, done, ok, lerr, c.wantDone, c.wantOK, c.wantLErr)
		}
	}
}

func TestParsePRCommentsJSON(t *testing.T) {
	data := []byte(`{"comments":[
		{"id":"c2","body":"later","author":{"login":"millken"},"createdAt":"2026-09-27T10:01:00Z"},
		{"id":"c1","body":"earlier","author":{"login":"millken"},"createdAt":"2026-09-27T10:00:00Z"}
	]}`)
	got, err := parsePRCommentsJSON(data)
	if err != nil {
		t.Fatalf("parsePRCommentsJSON: %v", err)
	}
	if len(got) != 2 || got[0].ID != "c1" || got[1].ID != "c2" {
		t.Fatalf("comments not sorted by createdAt: %+v", got)
	}
	if _, err := parsePRCommentsJSON([]byte("not json")); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}

// The live failure this pins: prRepoArgs without the leading "pr" made every
// gh call `gh view 3` — unknown command — and the fake-gh tests never caught
// it because they bypass command construction entirely.
func TestPRRepoArgs(t *testing.T) {
	if got := prRepoArgs("", "view", "3"); strings.Join(got, " ") != "pr view 3" {
		t.Fatalf("prRepoArgs(\"\") = %v, want [pr view 3]", got)
	}
	if got := prRepoArgs("o/r", "checks", "7"); strings.Join(got, " ") != `pr checks 7 --repo "o/r"` {
		t.Fatalf("prRepoArgs(o/r) = %v", got)
	}
}

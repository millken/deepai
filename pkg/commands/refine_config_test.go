package commands

import "testing"

// A missing key in config.yaml unmarshals to 0, which must not be confused with
// an explicit request to disable auto-refine — that mix-up would leave the guard
// in the REPL scheduling neither a refine nor a fallback extraction, silently
// stopping memory extraction for everyone who never edited their config.
func TestResolveRefineInterval(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   int
		want int
	}{
		{"absent from config", 0, defaultRefineInterval},
		{"explicit disable", -1, 0},
		{"any negative disables", -42, 0},
		{"explicit cadence", 7, 7},
		{"every turn", 1, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveRefineInterval(tc.in); got != tc.want {
				t.Fatalf("resolveRefineInterval(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

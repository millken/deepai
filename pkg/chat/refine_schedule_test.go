package chat

import "testing"

func TestMemoryScheduleFor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		turn       int
		interval   int
		autoRefine bool
		want       memoryScheduleMode
	}{
		{"gate turn with auto-refine on", 5, 5, true, memoryScheduleRefine},
		{"between gate turns", 3, 5, true, memoryScheduleNone},
		{"custom cadence", 4, 2, true, memoryScheduleRefine},
		{"every turn", 3, 1, true, memoryScheduleRefine},

		// Turning auto-refine off must fall back to the unconditional extraction
		// that predates it, not stop remembering things.
		{"auto-refine off falls back", 5, 5, false, memoryScheduleUnconditional},
		{"auto-refine off between fallback turns", 3, 5, false, memoryScheduleNone},

		// A zero or negative interval means "no gate", never "no memory".
		{"disabled interval falls back", 5, 0, true, memoryScheduleUnconditional},
		{"negative interval falls back", 5, -1, true, memoryScheduleUnconditional},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := memoryScheduleFor(tc.turn, tc.interval, tc.autoRefine)
			if got != tc.want {
				t.Fatalf("memoryScheduleFor(turn=%d, interval=%d, auto=%v) = %v, want %v",
					tc.turn, tc.interval, tc.autoRefine, got, tc.want)
			}
		})
	}
}

// The whole point of the fallback branch: no configuration, however odd, may
// leave a session that never extracts memory at all.
func TestMemoryScheduleNeverStopsExtractingEntirely(t *testing.T) {
	t.Parallel()

	for _, interval := range []int{-1, 0, 1, 2, 3, 5, 7} {
		for _, auto := range []bool{true, false} {
			scheduled := false
			for turn := 1; turn <= 100; turn++ {
				if memoryScheduleFor(turn, interval, auto) != memoryScheduleNone {
					scheduled = true
					break
				}
			}
			if !scheduled {
				t.Fatalf("interval=%d autoRefine=%v never schedules any extraction", interval, auto)
			}
		}
	}
}

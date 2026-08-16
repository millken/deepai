package commands

import (
	"testing"
	"time"
)

func TestResolveReviewTokenBudget(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{0, defaultReviewTokenBudget}, // absent → default
		{-1, 0},                       // negative → unlimited
		{12345, 12345},                // explicit wins
	}
	for _, tc := range cases {
		if got := resolveReviewTokenBudget(tc.in); got != tc.want {
			t.Fatalf("resolveReviewTokenBudget(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestResolveReviewTimeout(t *testing.T) {
	cases := []struct {
		in   int
		want time.Duration
	}{
		{0, defaultReviewTimeout},
		{-3, defaultReviewTimeout},
		{2, 2 * time.Minute},
	}
	for _, tc := range cases {
		if got := resolveReviewTimeout(tc.in); got != tc.want {
			t.Fatalf("resolveReviewTimeout(%d) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

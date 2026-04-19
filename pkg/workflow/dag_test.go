package workflow

import (
	"testing"

	"github.com/millken/deepai/pkg/agent"
)

func stage(name, role string, deps ...string) WorkflowStage {
	return WorkflowStage{
		Name:      name,
		Role:      agent.AgentType(role),
		InputFrom: deps,
	}
}

func TestTopologicalSort(t *testing.T) {
	t.Run("linear chain", func(t *testing.T) {
		stages := []WorkflowStage{
			stage("a", "coder"),
			stage("b", "coder", "a"),
			stage("c", "coder", "b"),
		}
		waves, err := topologicalSort(stages)
		if err != nil {
			t.Fatal(err)
		}
		if len(waves) != 3 {
			t.Fatalf("expected 3 waves, got %d", len(waves))
		}
		assertWaveNames(t, waves[0], "a")
		assertWaveNames(t, waves[1], "b")
		assertWaveNames(t, waves[2], "c")
	})

	t.Run("diamond DAG", func(t *testing.T) {
		stages := []WorkflowStage{
			stage("a", "coder"),
			stage("b", "reviewer", "a"),
			stage("c", "reviewer", "a"),
			stage("d", "coder", "b", "c"),
		}
		waves, err := topologicalSort(stages)
		if err != nil {
			t.Fatal(err)
		}
		if len(waves) != 3 {
			t.Fatalf("expected 3 waves, got %d", len(waves))
		}
		assertWaveNames(t, waves[0], "a")
		assertWaveLen(t, waves[1], 2) // b and c in parallel
		assertWaveNames(t, waves[2], "d")
	})

	t.Run("all parallel", func(t *testing.T) {
		stages := []WorkflowStage{
			stage("a", "coder"),
			stage("b", "reviewer"),
			stage("c", "reviewer"),
		}
		waves, err := topologicalSort(stages)
		if err != nil {
			t.Fatal(err)
		}
		if len(waves) != 1 {
			t.Fatalf("expected 1 wave, got %d", len(waves))
		}
		assertWaveLen(t, waves[0], 3)
	})

	t.Run("cycle detected", func(t *testing.T) {
		stages := []WorkflowStage{
			stage("a", "coder", "b"),
			stage("b", "coder", "a"),
		}
		_, err := topologicalSort(stages)
		if err == nil {
			t.Fatal("expected cycle error")
		}
	})

	t.Run("single stage", func(t *testing.T) {
		stages := []WorkflowStage{
			stage("a", "coder"),
		}
		waves, err := topologicalSort(stages)
		if err != nil {
			t.Fatal(err)
		}
		if len(waves) != 1 {
			t.Fatalf("expected 1 wave, got %d", len(waves))
		}
		assertWaveNames(t, waves[0], "a")
	})

	t.Run("empty stages", func(t *testing.T) {
		waves, err := topologicalSort(nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(waves) != 0 {
			t.Fatalf("expected 0 waves, got %d", len(waves))
		}
	})
}

func assertWaveNames(t *testing.T, wave []WorkflowStage, names ...string) {
	t.Helper()
	if len(wave) != len(names) {
		t.Fatalf("expected %d stages, got %d", len(names), len(wave))
	}
	waveNames := make(map[string]bool, len(wave))
	for _, s := range wave {
		waveNames[s.Name] = true
	}
	for _, n := range names {
		if !waveNames[n] {
			t.Errorf("expected stage %q in wave", n)
		}
	}
}

func assertWaveLen(t *testing.T, wave []WorkflowStage, want int) {
	t.Helper()
	if len(wave) != want {
		t.Errorf("expected %d stages in wave, got %d", want, len(wave))
	}
}

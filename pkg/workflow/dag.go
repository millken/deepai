package workflow

import (
	"fmt"
)

// topologicalSort orders stages into waves of parallel execution using Kahn's algorithm.
// Returns an error if a cycle is detected.
func topologicalSort(stages []WorkflowStage) ([][]WorkflowStage, error) {
	if len(stages) == 0 {
		return nil, nil
	}

	nameIdx := make(map[string]int, len(stages))
	for i, s := range stages {
		nameIdx[s.Name] = i
	}

	inDegree := make([]int, len(stages))
	// reverseEdges[j] = list of stage indices that depend on stage j
	reverseEdges := make([][]int, len(stages))

	for i, s := range stages {
		for _, dep := range s.InputFrom {
			depIdx, ok := nameIdx[dep]
			if !ok {
				continue // validated elsewhere
			}
			inDegree[i]++
			reverseEdges[depIdx] = append(reverseEdges[depIdx], i)
		}
	}

	var queue []int
	for i, d := range inDegree {
		if d == 0 {
			queue = append(queue, i)
		}
	}

	var waves [][]WorkflowStage
	processed := 0
	for len(queue) > 0 {
		wave := make([]WorkflowStage, 0, len(queue))
		var nextQueue []int
		for _, idx := range queue {
			wave = append(wave, stages[idx])
			for _, depIdx := range reverseEdges[idx] {
				inDegree[depIdx]--
				if inDegree[depIdx] == 0 {
					nextQueue = append(nextQueue, depIdx)
				}
			}
		}
		waves = append(waves, wave)
		processed += len(wave)
		queue = nextQueue
	}

	if processed < len(stages) {
		return nil, fmt.Errorf("cycle detected in workflow stages")
	}
	return waves, nil
}

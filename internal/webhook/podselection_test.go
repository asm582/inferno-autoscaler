package webhook

import (
	"testing"
)

func TestCalculateDeletionCost(t *testing.T) {
	tests := []struct {
		name            string
		queueDepth      int
		runningRequests int
		expectedCost    int
	}{
		{
			name:            "idle pod",
			queueDepth:      0,
			runningRequests: 0,
			expectedCost:    -100,
		},
		{
			name:            "lightly loaded",
			queueDepth:      1,
			runningRequests: 0,
			expectedCost:    -90, // (1 * 10) + (0 * 5) - 100 = -90
		},
		{
			name:            "moderately loaded",
			queueDepth:      10,
			runningRequests: 2,
			expectedCost:    10, // (10 * 10) + (2 * 5) - 100 = 10
		},
		{
			name:            "heavily loaded",
			queueDepth:      50,
			runningRequests: 5,
			expectedCost:    425, // (50 * 10) + (5 * 5) - 100 = 425
		},
		{
			name:            "very heavily loaded",
			queueDepth:      100,
			runningRequests: 10,
			expectedCost:    950, // (100 * 10) + (10 * 5) - 100 = 950
		},
		{
			name:            "only running requests",
			queueDepth:      0,
			runningRequests: 10,
			expectedCost:    -50, // (0 * 10) + (10 * 5) - 100 = -50
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CalculateDeletionCost(tt.queueDepth, tt.runningRequests)
			if got != tt.expectedCost {
				t.Errorf("CalculateDeletionCost(%d, %d) = %d, want %d",
					tt.queueDepth, tt.runningRequests, got, tt.expectedCost)
			}
		})
	}
}

func TestDeletionCostOrdering(t *testing.T) {
	// Test that idle pods have lower cost than loaded pods
	idleCost := CalculateDeletionCost(0, 0)
	loadedCost := CalculateDeletionCost(10, 2)

	if idleCost >= loadedCost {
		t.Errorf("idle cost (%d) should be < loaded cost (%d)", idleCost, loadedCost)
	}

	// Multiple tests to ensure consistent ordering
	for queueDepth := 1; queueDepth <= 20; queueDepth++ {
		prevCost := CalculateDeletionCost(queueDepth-1, 0)
		currCost := CalculateDeletionCost(queueDepth, 0)
		if currCost < prevCost {
			t.Errorf("cost should increase with queue depth: %d >= %d at depth %d",
				currCost, prevCost, queueDepth)
		}
	}
}

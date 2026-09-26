package policy

import (
	"context"
	"testing"

	helmv1alpha1 "github.com/gfhw/inferguard/api/v1alpha1"
)

func latencyPolicy(trigger int32) []helmv1alpha1.PolicySpec {
	return []helmv1alpha1.PolicySpec{
		{
			Name:      "high-latency",
			Condition: helmv1alpha1.PolicyCondition{Type: "InferenceLatency", Threshold: 5000},
			Action:    helmv1alpha1.PolicyAction{TriggerCount: trigger},
		},
	}
}

// matching: avg = (30/3)*1000 = 10000ms >= 5000
func matchingState(name string) InferenceState {
	return InferenceState{Name: name, LatencySumSec: 30, LatencyCount: 3}
}

// nonMatching: avg = 1000ms < 5000
func nonMatchingState(name string) InferenceState {
	return InferenceState{Name: name, LatencySumSec: 1, LatencyCount: 1}
}

func TestEvaluateInference_TriggerCount(t *testing.T) {
	e := NewEngine()
	policies := latencyPolicy(3)

	if got := e.EvaluateInference(context.Background(), "r", "ns", matchingState("pod-1"), policies); len(got) != 0 {
		t.Fatalf("1st match should not trigger, got %d results", len(got))
	}
	if got := e.EvaluateInference(context.Background(), "r", "ns", matchingState("pod-1"), policies); len(got) != 0 {
		t.Fatalf("2nd match should not trigger, got %d results", len(got))
	}
	if got := e.EvaluateInference(context.Background(), "r", "ns", matchingState("pod-1"), policies); len(got) != 1 {
		t.Fatalf("3rd match should trigger, got %d results", len(got))
	}
}

func TestEvaluateInference_ResetOnNonMatch(t *testing.T) {
	e := NewEngine()
	policies := latencyPolicy(3)

	e.EvaluateInference(context.Background(), "r", "ns", matchingState("pod-1"), policies)
	e.EvaluateInference(context.Background(), "r", "ns", matchingState("pod-1"), policies)
	// non-match resets the counter
	e.EvaluateInference(context.Background(), "r", "ns", nonMatchingState("pod-1"), policies)

	// Two more matches must NOT trigger (counter restarted from 0).
	if got := e.EvaluateInference(context.Background(), "r", "ns", matchingState("pod-1"), policies); len(got) != 0 {
		t.Fatalf("expected no trigger after reset, got %d", len(got))
	}
	if got := e.EvaluateInference(context.Background(), "r", "ns", matchingState("pod-1"), policies); len(got) != 0 {
		t.Fatalf("expected no trigger after reset, got %d", len(got))
	}
	// Third consecutive match triggers.
	if got := e.EvaluateInference(context.Background(), "r", "ns", matchingState("pod-1"), policies); len(got) != 1 {
		t.Fatalf("expected trigger on 3rd consecutive match, got %d", len(got))
	}
}

func TestEvaluateInference_PerTargetIsolation(t *testing.T) {
	e := NewEngine()
	policies := latencyPolicy(3)

	// pod-1 matched twice
	e.EvaluateInference(context.Background(), "r", "ns", matchingState("pod-1"), policies)
	e.EvaluateInference(context.Background(), "r", "ns", matchingState("pod-1"), policies)

	// pod-2 matches three times; its counter is independent, so the 3rd triggers.
	e.EvaluateInference(context.Background(), "r", "ns", matchingState("pod-2"), policies)
	e.EvaluateInference(context.Background(), "r", "ns", matchingState("pod-2"), policies)
	if got := e.EvaluateInference(context.Background(), "r", "ns", matchingState("pod-2"), policies); len(got) != 1 {
		t.Fatalf("expected pod-2 to trigger on 3rd match, got %d", len(got))
	}

	// pod-1 still has only 2 matches; must not trigger.
	if got := e.EvaluateInference(context.Background(), "r", "ns", matchingState("pod-1"), policies); len(got) != 1 {
		t.Fatalf("pod-1 3rd match should trigger (counter preserved), got %d", len(got))
	}
}

package policy

import (
	"context"
	"fmt"
	"sync"

	helmv1alpha1 "github.com/gfhw/inferguard/api/v1alpha1"
)

// InferenceState holds AI-specific metrics for policy evaluation.
type InferenceState struct {
	Namespace       string
	Name            string
	LatencySumSec   float64
	LatencyCount    float64
	GPUCachePct     float64
	RequestsWaiting int32
}

// Engine evaluates AI-metrics-based policies and returns the alerts that fired.
// The caller is responsible for delivering them (via EventSender.SendRaw).
type Engine struct {
	conditionCount map[string]int32
	mu             sync.Mutex
}

// PolicyResult records the outcome of a policy evaluation.
type PolicyResult struct {
	PolicyName string
	Triggered  bool
	Message    string
	AlertBody  string
}

func NewEngine() *Engine {
	return &Engine{
		conditionCount: make(map[string]int32),
	}
}

// EvaluateInference checks AI-metrics-based policies (InferenceLatency, GPUCacheUsage, etc.).
func (e *Engine) EvaluateInference(ctx context.Context, releaseName, namespace string,
	infer InferenceState, policies []helmv1alpha1.PolicySpec) []PolicyResult {
	return e.evaluate(ctx, releaseName, namespace, "inference", infer.Name, policies, func(p helmv1alpha1.PolicySpec) bool {
		return e.matchInferenceCondition(p.Condition, infer)
	})
}

// evaluate is the shared loop: condition counting + alert notification.
func (e *Engine) evaluate(ctx context.Context, releaseName, namespace, domain, target string,
	policies []helmv1alpha1.PolicySpec, matchFn func(helmv1alpha1.PolicySpec) bool) []PolicyResult {

	var results []PolicyResult

	for _, p := range policies {
		// Scope the trigger counter per release + policy + evaluation domain +
		// target (pod). Without domain and target in the key, pod-level and
		// inference-level evaluation would zero each other's counters, and all
		// replicas would share one counter — making triggerCount>1 unreachable.
		key := releaseName + "|" + domain + "|" + p.Name + "|" + target
		met := matchFn(p)

		e.mu.Lock()
		if met {
			e.conditionCount[key]++
		} else {
			e.conditionCount[key] = 0
		}
		count := e.conditionCount[key]
		e.mu.Unlock()

		if !met {
			continue
		}

		// triggerCount defaults to 1 �� fires immediately on first match.
		triggerCount := p.Action.TriggerCount
		if triggerCount <= 0 {
			triggerCount = 1
		}
		if count < triggerCount {
			continue
		}

		// Reset counter after triggering.
		e.mu.Lock()
		e.conditionCount[key] = 0
		e.mu.Unlock()

		alertMsg := fmt.Sprintf("%s: %s on %s", p.Condition.Type, p.Name, releaseName)

		result := PolicyResult{
			PolicyName: p.Name,
			Triggered:  true,
			Message:    alertMsg,
			AlertBody:  p.Action.AlertBody,
		}

		results = append(results, result)
	}

	return results
}

func (e *Engine) matchInferenceCondition(cond helmv1alpha1.PolicyCondition, infer InferenceState) bool {
	switch cond.Type {
	case "InferenceLatency":
		// avg latency in ms: (sum_sec / count) * 1000
		if infer.LatencyCount <= 0 {
			return false
		}
		avgMs := (infer.LatencySumSec / infer.LatencyCount) * 1000
		return avgMs >= float64(cond.Threshold)
	case "GPUCacheUsage":
		return infer.GPUCachePct >= float64(cond.Threshold)
	case "InferenceQueueDepth":
		return infer.RequestsWaiting >= cond.Threshold
	default:
		return false
	}
}

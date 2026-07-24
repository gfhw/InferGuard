package policy

import (
	"context"
	"fmt"
	"sync"

	helmv1alpha1 "github.com/gfhw/inferguard/api/v1alpha1"
)

// PodState holds the snapshot needed for pod-level policy evaluation.
type PodState struct {
	Namespace string
	Name      string
	Phase     string
	Ready     bool
	Restart   int32
}

// InferenceState holds AI-specific metrics for policy evaluation.
type InferenceState struct {
	Namespace       string
	Name            string
	LatencyP99Ms    float64
	GPUCachePct     float64
	RequestsWaiting int32
}

// RollbackFunc is called when a policy triggers a rollback action.
type RollbackFunc func(ctx context.Context, releaseName, namespace string, revision int) (newVersion string, err error)

// NotifyFunc is called for notification-only policy actions.
type NotifyFunc func(ctx context.Context, releaseName, message string) error

// AfterRollbackFunc is called after a successful policy-triggered rollback to sync CR spec.
type AfterRollbackFunc func(ctx context.Context, releaseName, namespace, newVersion string) error

// Engine evaluates policies and executes actions.
type Engine struct {
	rollback       RollbackFunc
	notify         NotifyFunc
	afterRollback  AfterRollbackFunc
	rolledBack     map[string]bool
	conditionCount map[string]int32
	mu             sync.Mutex
}

// PolicyResult records the outcome of a policy evaluation.
type PolicyResult struct {
	PolicyName  string
	Triggered   bool
	ActionTaken string
	Message     string
	AlertBody   string
}

func NewEngine(rollbackFn RollbackFunc, notifyFn NotifyFunc, afterRollbackFn AfterRollbackFunc) *Engine {
	return &Engine{
		rollback:       rollbackFn,
		notify:         notifyFn,
		afterRollback:  afterRollbackFn,
		rolledBack:     make(map[string]bool),
		conditionCount: make(map[string]int32),
	}
}

// Evaluate checks pod-level policies (PodRestart, PodNotReady, PodCrash).
func (e *Engine) Evaluate(ctx context.Context, releaseName, namespace string,
	pod PodState, policies []helmv1alpha1.PolicySpec) []PolicyResult {
	return e.evaluate(ctx, releaseName, namespace, policies, func(p helmv1alpha1.PolicySpec) bool {
		return e.matchCondition(p.Condition, pod)
	})
}

// EvaluateInference checks AI-metrics-based policies (InferenceLatency, GPUCacheUsage, etc.).
func (e *Engine) EvaluateInference(ctx context.Context, releaseName, namespace string,
	infer InferenceState, policies []helmv1alpha1.PolicySpec) []PolicyResult {
	return e.evaluate(ctx, releaseName, namespace, policies, func(p helmv1alpha1.PolicySpec) bool {
		return e.matchInferenceCondition(p.Condition, infer)
	})
}

// MarkRolledBack tells the engine a rollback was performed (user or policy).
func (e *Engine) MarkRolledBack(releaseName string) {
	e.rolledBack[releaseName] = true
}

// evaluate is the shared loop: condition counting + action execution.
func (e *Engine) evaluate(ctx context.Context, releaseName, namespace string,
	policies []helmv1alpha1.PolicySpec, matchFn func(helmv1alpha1.PolicySpec) bool) []PolicyResult {

	var results []PolicyResult

	for _, p := range policies {
		key := releaseName + "|" + p.Name
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

		// triggerCount defaults to 1 — fires immediately on first match.
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

		switch p.Action.Type {
		case "Rollback":
			if e.rolledBack[releaseName] {
				result.Triggered = false
				result.Message = "skipped: already rolled back once"
				results = append(results, result)
				continue
			}
			if e.rollback != nil {
				newVer, err := e.rollback(ctx, releaseName, namespace, 0)
				if err != nil {
					result.ActionTaken = "rollback-failed"
					result.Message = err.Error()
				} else {
					e.rolledBack[releaseName] = true
					result.ActionTaken = "rollback"
					result.Message = "rolled back to " + newVer
					if e.afterRollback != nil {
						e.afterRollback(ctx, releaseName, namespace, newVer)
					}
				}
			}
		case "Notify", "":
			result.ActionTaken = "notify"
		default:
			result.ActionTaken = "notify"
		}

		results = append(results, result)
	}

	return results
}

func (e *Engine) matchCondition(cond helmv1alpha1.PolicyCondition, pod PodState) bool {
	switch cond.Type {
	case "PodRestart":
		return pod.Restart >= cond.Threshold
	case "PodNotReady":
		return !pod.Ready
	case "PodCrash":
		return pod.Phase == "Failed" || pod.Phase == "CrashLoopBackOff"
	default:
		return false
	}
}

func (e *Engine) matchInferenceCondition(cond helmv1alpha1.PolicyCondition, infer InferenceState) bool {
	switch cond.Type {
	case "InferenceLatency":
		return infer.LatencyP99Ms >= float64(cond.Threshold)
	case "GPUCacheUsage":
		return infer.GPUCachePct >= float64(cond.Threshold)
	case "InferenceQueueDepth":
		return infer.RequestsWaiting >= cond.Threshold
	default:
		return false
	}
}

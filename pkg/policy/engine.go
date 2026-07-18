package policy

import (
	"context"

	helmv1alpha1 "watchpod/api/v1alpha1"
)

// PodState holds the snapshot needed for policy evaluation.
type PodState struct {
	Namespace string
	Name      string
	Phase     string
	Ready     bool
	Restart   int32
}

// RollbackFunc is called when a policy triggers a rollback action.
// Returns the new chart version after rollback.
type RollbackFunc func(ctx context.Context, releaseName, namespace string, revision int) (newVersion string, err error)

// NotifyFunc is called to send a notification after a policy action.
type NotifyFunc func(ctx context.Context, releaseName, message string) error

// AfterRollbackFunc is called after a successful policy-triggered rollback to sync CR spec.
type AfterRollbackFunc func(ctx context.Context, releaseName, namespace, newVersion string) error

// Engine evaluates policies and executes actions.
type Engine struct {
	rollback      RollbackFunc
	notify        NotifyFunc
	afterRollback AfterRollbackFunc
	rolledBack    map[string]bool // releaseName -> already rolled back once
}

func NewEngine(rollbackFn RollbackFunc, notifyFn NotifyFunc, afterRollbackFn AfterRollbackFunc) *Engine {
	return &Engine{
		rollback:      rollbackFn,
		notify:        notifyFn,
		afterRollback: afterRollbackFn,
		rolledBack:    make(map[string]bool),
	}
}

// PolicyResult records the outcome of a policy evaluation.
type PolicyResult struct {
	PolicyName  string
	Triggered   bool
	ActionTaken string
	Message     string
}

// Evaluate checks all policies for a release given the latest pod event.
func (e *Engine) Evaluate(ctx context.Context, releaseName, namespace string,
	pod PodState, policies []helmv1alpha1.PolicySpec) []PolicyResult {

	var results []PolicyResult

	for _, p := range policies {
		if !e.matchCondition(p.Condition, pod) {
			continue
		}

		result := PolicyResult{
			PolicyName: p.Name,
			Triggered:  true,
		}

		switch p.Action.Type {
		case "Rollback":
			// If already rolled back once for this release and Pods are still crashing,
			// the problem is not version-specific — stop, don't keep rolling back.
			if e.rolledBack[releaseName] {
				result.Triggered = false
				result.Message = "skipped: already rolled back once, manual intervention required"
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
		case "Notify":
			fallthrough
		default:
			if e.notify != nil {
				e.notify(ctx, releaseName, "policy "+p.Name+" triggered")
			}
			result.ActionTaken = "notify"
			result.Message = "notification sent"
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
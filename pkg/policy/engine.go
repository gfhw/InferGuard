package policy

import (
	"context"
	"time"

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
type RollbackFunc func(ctx context.Context, releaseName, namespace string, revision int) error

// NotifyFunc is called to send a notification after a policy action.
type NotifyFunc func(ctx context.Context, releaseName, message string) error

// Engine evaluates policies and executes actions.
type Engine struct {
	rollback RollbackFunc
	notify   NotifyFunc
}

func NewEngine(rollbackFn RollbackFunc, notifyFn NotifyFunc) *Engine {
	return &Engine{rollback: rollbackFn, notify: notifyFn}
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
			rev := p.Action.Revision
			if rev == 0 {
				rev = -1 // helm rollback 0 = previous revision
			}
			// For helm rollback, use 0 to mean "last successful"
			if e.rollback != nil {
				if err := e.rollback(ctx, releaseName, namespace, 0); err != nil {
					result.ActionTaken = "rollback-failed"
					result.Message = err.Error()
				} else {
					result.ActionTaken = "rollback"
					result.Message = "rolled back to previous revision"
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

// ParseWindow parses a window string like "10m" into a time.Duration.
func ParseWindow(window string) time.Duration {
	d, _ := time.ParseDuration(window)
	return d
}
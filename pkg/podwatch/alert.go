package podwatch

import (
	"strconv"
	"strings"

	"github.com/gfhw/inferguard/pkg/policy"
)

// expandAlertVars replaces $${...} placeholders in the alert body with runtime values.
//
// Supported variables:
//   $${release_name}  $${pod_name}  $${namespace}  $${pod_phase}
//   $${pod_restart}   $${pod_ip}
//
// AI inference variables (substituted to "0" when state is nil so the
// placeholder never leaks into a pod-level alert body):
//   $${inference_latency_ms}  $${gpu_cache_pct}
func expandAlertVars(alertBody string, info PodInfo, releaseName string, state *policy.InferenceState) string {
	s := alertBody
	s = strings.ReplaceAll(s, "${release_name}", releaseName)
	s = strings.ReplaceAll(s, "${pod_name}", info.Name)
	s = strings.ReplaceAll(s, "${namespace}", info.Namespace)
	s = strings.ReplaceAll(s, "${pod_phase}", info.Phase)
	s = strings.ReplaceAll(s, "${pod_ip}", info.PodIP)
	s = strings.ReplaceAll(s, "${pod_restart}", strconv.Itoa(int(info.Restart)))

	if state != nil && state.LatencyCount > 0 {
		avgMs := (state.LatencySumSec / state.LatencyCount) * 1000
		s = strings.ReplaceAll(s, "${inference_latency_ms}", strconv.FormatFloat(avgMs, 'f', 1, 64))
	} else {
		s = strings.ReplaceAll(s, "${inference_latency_ms}", "0")
	}
	if state != nil {
		s = strings.ReplaceAll(s, "${gpu_cache_pct}", strconv.FormatFloat(state.GPUCachePct, 'f', 1, 64))
	} else {
		s = strings.ReplaceAll(s, "${gpu_cache_pct}", "0")
	}

	return s
}

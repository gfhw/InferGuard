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
// AI inference variables (only substituted when state != nil):
//   $${inference_latency_ms}  $${gpu_cache_pct}
func expandAlertVars(alertBody string, info PodInfo, releaseName string, state *policy.InferenceState) string {
	s := alertBody
	s = strings.ReplaceAll(s, "${release_name}", releaseName)
	s = strings.ReplaceAll(s, "${pod_name}", info.Name)
	s = strings.ReplaceAll(s, "${namespace}", info.Namespace)
	s = strings.ReplaceAll(s, "${pod_phase}", info.Phase)
	s = strings.ReplaceAll(s, "${pod_ip}", info.PodIP)
	s = strings.ReplaceAll(s, "${pod_restart}", strconv.Itoa(int(info.Restart)))

	if state != nil {
		s = strings.ReplaceAll(s, "${inference_latency_ms}", strconv.FormatFloat(state.LatencyP99Ms, 'f', 1, 64))
		s = strings.ReplaceAll(s, "${gpu_cache_pct}", strconv.FormatFloat(state.GPUCachePct, 'f', 1, 64))
	}

	return s
}

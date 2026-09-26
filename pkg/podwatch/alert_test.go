package podwatch

import (
	"strings"
	"testing"

	"github.com/gfhw/inferguard/pkg/policy"
)

func TestExpandAlertVars_PodLevel_SubstitutesZero(t *testing.T) {
	info := PodInfo{Name: "pod-1", Namespace: "ns", Phase: "Running", PodIP: "10.0.0.1", Restart: 2}
	body := `{"release":"${release_name}","pod":"${pod_name}","latency":${inference_latency_ms},"cache":${gpu_cache_pct}}`

	out := expandAlertVars(body, info, "my-release", nil)
	if strings.Contains(out, "${") {
		t.Fatalf("expected all placeholders substituted, got %q", out)
	}
	if !strings.Contains(out, `"latency":0`) || !strings.Contains(out, `"cache":0`) {
		t.Fatalf("expected inference placeholders to be 0 when state is nil, got %q", out)
	}
}

func TestExpandAlertVars_Inference_ComputesAvg(t *testing.T) {
	info := PodInfo{Name: "pod-1", Namespace: "ns"}
	state := &policy.InferenceState{LatencySumSec: 10.0, LatencyCount: 5.0, GPUCachePct: 72.5}

	out := expandAlertVars("${inference_latency_ms}|${gpu_cache_pct}", info, "r", state)
	// avg = (10/5)*1000 = 2000ms
	if !strings.Contains(out, "2000.0") {
		t.Fatalf("expected latency 2000.0, got %q", out)
	}
	if !strings.Contains(out, "72.5") {
		t.Fatalf("expected cache 72.5, got %q", out)
	}
}

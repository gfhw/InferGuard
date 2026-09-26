package podwatch

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestShouldMonitor_RequiresInstanceLabel(t *testing.T) {
	f := NewFilter(nil)

	noLabels := &corev1.Pod{}
	if f.ShouldMonitor(noLabels) {
		t.Fatal("expected pod without labels to be rejected")
	}

	withInstance := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/instance": "r"}},
	}
	if !f.ShouldMonitor(withInstance) {
		t.Fatal("expected pod with instance label to be monitored")
	}
}

func TestGetReleaseName(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{"app.kubernetes.io/instance": "llama-3-8b"},
		},
	}
	if got := GetReleaseName(pod); got != "llama-3-8b" {
		t.Fatalf("expected llama-3-8b, got %q", got)
	}

	pod.Labels = map[string]string{"release": "fallback-release"}
	if got := GetReleaseName(pod); got != "fallback-release" {
		t.Fatalf("expected fallback-release, got %q", got)
	}

	pod.Labels = nil
	if got := GetReleaseName(pod); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

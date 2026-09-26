package podwatch

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestConvertToPodInfo_ReadyFromCondition(t *testing.T) {
	// A partially-ready pod (one container ready, one not) must NOT be Ready.
	pod := &corev1.Pod{}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: "c1", Ready: true, RestartCount: 0},
		{Name: "c2", Ready: false, RestartCount: 1},
	}
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionFalse},
	}

	info := ConvertToPodInfo(pod)
	if info.Ready {
		t.Fatal("expected partially-ready pod to be Ready=false")
	}

	// All containers ready via the authoritative PodReady condition.
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}
	info = ConvertToPodInfo(pod)
	if !info.Ready {
		t.Fatal("expected PodReady=True to yield Ready=true")
	}
}

func TestConvertToPodInfo_RestartCountIncludesInit(t *testing.T) {
	pod := &corev1.Pod{}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: "app", RestartCount: 2},
	}
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
		{Name: "init-download", RestartCount: 3},
	}

	info := ConvertToPodInfo(pod)
	if info.Restart != 5 {
		t.Fatalf("expected restart=5 (2 app + 3 init), got %d", info.Restart)
	}
}

func TestConvertToPodInfo_OOMKilled(t *testing.T) {
	pod := &corev1.Pod{}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{
			Name: "app",
			LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"},
			},
		},
	}

	info := ConvertToPodInfo(pod)
	if !info.OOMKilled {
		t.Fatal("expected OOMKilled=true")
	}
	if info.LastTerminationReason != "OOMKilled" {
		t.Fatalf("expected LastTerminationReason=OOMKilled, got %q", info.LastTerminationReason)
	}
}

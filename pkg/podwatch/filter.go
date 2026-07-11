package podwatch

import (
	corev1 "k8s.io/api/core/v1"
)

type Filter struct {
	Namespaces []string
}

func NewFilter(namespaces []string) *Filter {
	return &Filter{
		Namespaces: namespaces,
	}
}

func (f *Filter) ShouldMonitor(pod *corev1.Pod) bool {
	if pod.Labels == nil {
		return false
	}

	if _, hasInstance := pod.Labels["app.kubernetes.io/instance"]; !hasInstance {
		return false
	}

	if len(f.Namespaces) > 0 {
		found := false
		for _, ns := range f.Namespaces {
			if pod.Namespace == ns {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	return true
}

func GetReleaseName(pod *corev1.Pod) string {
	if pod.Labels == nil {
		return ""
	}

	if name, ok := pod.Labels["app.kubernetes.io/instance"]; ok {
		return name
	}

	if name, ok := pod.Labels["release"]; ok {
		return name
	}

	return ""
}

package podwatch

import (
	"context"
	"sync"

	helmv1alpha1 "github.com/gfhw/inferguard/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
)

type PodEventType string

const (
	PodEventAdded    PodEventType = "ADDED"
	PodEventModified PodEventType = "MODIFIED"
	PodEventDeleted  PodEventType = "DELETED"
)

type PodInfo struct {
	Namespace string
	Name      string
	UID       string
	Phase     string
	NodeName  string
	PodIP     string
	Ready     bool
	Restart   int32

	// AI-ops diagnostics surfaced to the CR status so `kubectl describe` can
	// answer "why is this pod unhealthy" without inspecting the pod directly.
	OOMKilled            bool
	LastTerminationReason string
	Reason               string
	Message              string
}

func ConvertToPodInfo(pod *corev1.Pod) PodInfo {
	// Ready reflects the authoritative PodReady condition (all containers ready),
	// not "any container ready". A partially-ready pod must count as unhealthy.
	ready := false
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			ready = true
			break
		}
	}

	// Sum restart counts across regular AND init containers, and collect the
	// most recent termination reason (OOM is the high-signal case for GPU pods).
	restart := int32(0)
	oomKilled := false
	lastTerminationReason := ""
	for _, cs := range pod.Status.ContainerStatuses {
		restart += cs.RestartCount
		if cs.LastTerminationState.Terminated != nil {
			if cs.LastTerminationState.Terminated.Reason == "OOMKilled" {
				oomKilled = true
			}
			lastTerminationReason = cs.LastTerminationState.Terminated.Reason
		}
	}
	for _, is := range pod.Status.InitContainerStatuses {
		restart += is.RestartCount
		if is.LastTerminationState.Terminated != nil {
			if is.LastTerminationState.Terminated.Reason == "OOMKilled" {
				oomKilled = true
			}
			if lastTerminationReason == "" {
				lastTerminationReason = is.LastTerminationState.Terminated.Reason
			}
		}
	}

	return PodInfo{
		Namespace:             pod.Namespace,
		Name:                  pod.Name,
		UID:                   string(pod.UID),
		Phase:                 string(pod.Status.Phase),
		NodeName:              pod.Spec.NodeName,
		PodIP:                 pod.Status.PodIP,
		Ready:                 ready,
		Restart:               restart,
		OOMKilled:             oomKilled,
		LastTerminationReason: lastTerminationReason,
		Reason:                pod.Status.Reason,
		Message:               pod.Status.Message,
	}
}

type PendingPod struct {
	Namespace   string
	Name        string
	UID         string
	ReleaseName string
	EventType   PodEventType
	// Snapshot is the pod state captured at enqueue time. It is only needed for
	// DELETED events, where the pod is already gone and cannot be re-fetched.
	Snapshot PodInfo
}

type StatusUpdater interface {
	UpdatePodStatus(ctx context.Context, releaseName string, pod PodInfo, eventType PodEventType) error
}

// ReleaseConfig holds per-release sender and status updater configuration.
type ReleaseConfig struct {
	EventSender   *EventSender
	StatusUpdater StatusUpdater
	Policies      []helmv1alpha1.PolicySpec
	Revision      int
}

// ReleaseRegistry maps releaseName -> ReleaseConfig.
type ReleaseRegistry struct {
	mu      sync.Mutex
	entries map[string]*ReleaseConfig
}

func NewReleaseRegistry() *ReleaseRegistry {
	return &ReleaseRegistry{
		entries: make(map[string]*ReleaseConfig),
	}
}

func (r *ReleaseRegistry) Register(releaseName string, cfg *ReleaseConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[releaseName] = cfg
}

func (r *ReleaseRegistry) Unregister(releaseName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, releaseName)
}

func (r *ReleaseRegistry) GetRegisteredReleases() map[string]*ReleaseConfig {
	r.mu.Lock()
	defer r.mu.Unlock()
	copied := make(map[string]*ReleaseConfig, len(r.entries))
	for k, v := range r.entries {
		copied[k] = v
	}
	return copied
}

func (r *ReleaseRegistry) Get(releaseName string) *ReleaseConfig {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.entries[releaseName]
}






func (r *ReleaseRegistry) UpdateRevision(releaseName string, revision int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cfg, ok := r.entries[releaseName]
	if !ok {
		return
	}
	// Replace the entry with a shallow copy so readers that already hold the
	// previous pointer never observe a mutated Revision. The collector reads
	// ReleaseConfig.Revision outside the registry lock, so mutating it in place
	// would be a data race.
	clone := *cfg
	clone.Revision = revision
	r.entries[releaseName] = &clone
}

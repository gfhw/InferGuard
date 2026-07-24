package podwatch

import (
	"context"
	"encoding/json"
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
}

func ConvertToPodInfo(pod *corev1.Pod) PodInfo {
	ready := false
	restart := int32(0)
	for _, cs := range pod.Status.ContainerStatuses {
		restart += cs.RestartCount
		for _, c := range pod.Spec.Containers {
			if c.Name == cs.Name {
				if cs.Ready {
					ready = true
				}
			}
		}
	}
	if len(pod.Status.ContainerStatuses) == 0 {
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				ready = true
			}
		}
	}

	return PodInfo{
		Namespace: pod.Namespace,
		Name:      pod.Name,
		UID:       string(pod.UID),
		Phase:     string(pod.Status.Phase),
		NodeName:  pod.Spec.NodeName,
		PodIP:     pod.Status.PodIP,
		Ready:     ready,
		Restart:   restart,
	}
}

type PendingPod struct {
	PodInfo
	Snapshot    PodInfo
	EventType   PodEventType
	ReleaseName string
}

type PodEvent struct {
	Type        PodEventType     `json:"type"`
	Pod         PodInfo          `json:"pod,omitempty"`
	OldPod      PodInfo          `json:"oldPod,omitempty"`
	Namespace   string           `json:"namespace,omitempty"`
	ReleaseName string           `json:"releaseName,omitempty"`
	Timestamp   int64            `json:"timestamp"`
	AlertBody   json.RawMessage  `json:"alert,omitempty"`
}

type PodStatus struct {
	Namespace     string       `json:"namespace,omitempty"`
	Name          string       `json:"name,omitempty"`
	UID           string       `json:"uid,omitempty"`
	Phase         string       `json:"phase,omitempty"`
	NodeName      string       `json:"nodeName,omitempty"`
	PodIP         string       `json:"podIP,omitempty"`
	Ready         bool         `json:"ready"`
	Restart       int32        `json:"restart"`
	LastEventType PodEventType `json:"lastEventType,omitempty"`
}

func PodInfoToPodStatus(info PodInfo) PodStatus {
	return PodStatus{
		Namespace: info.Namespace,
		Name:      info.Name,
		UID:       info.UID,
		Phase:     info.Phase,
		NodeName:  info.NodeName,
		PodIP:     info.PodIP,
		Ready:     info.Ready,
		Restart:   info.Restart,
	}
}

type StatusUpdater interface {
	UpdatePodStatus(ctx context.Context, releaseName string, pod PodInfo, eventType PodEventType) error
}

// ReleaseConfig holds per-release sender and status updater configuration.
type ReleaseConfig struct {
	EventSender   *EventSender
	StatusUpdater StatusUpdater
	Filter          *ReleaseFilter
	Policies        []helmv1alpha1.PolicySpec
	AutoRemediation bool
}

// ReleaseRegistry maps releaseName → ReleaseConfig.
// ReleaseFilter mirrors EventFilterSpec at runtime for per-release event filtering.
type ReleaseFilter struct {
	OnUnhealthyOnly  bool
	MinRestartCount  int32
	IgnoreEventTypes []string
	Phases           []string
}

// ShouldPush returns true if the event passes this release filter.
func (f *ReleaseFilter) ShouldPush(info PodInfo, eventType PodEventType) bool {
	if f == nil {
		return true
	}
	if f.OnUnhealthyOnly && info.Ready {
		return false
	}
	if f.MinRestartCount > 0 && info.Restart < f.MinRestartCount {
		return false
	}
	for _, t := range f.IgnoreEventTypes {
		if string(eventType) == t {
			return false
		}
	}
	if len(f.Phases) > 0 {
		found := false
		for _, p := range f.Phases {
			if info.Phase == p {
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

type ReleaseRegistry struct {
	mu      sync.Mutex
	entries map[string]*ReleaseConfig
}

// ReleaseFilter mirrors EventFilterSpec at runtime for per-release event filtering.

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



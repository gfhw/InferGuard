package v1alpha1

import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type HelmReleaseSpec struct {
	ReleaseName string                `json:"releaseName,omitempty"`
	Chart       ChartSpec             `json:"chart"`
	Values      *runtime.RawExtension `json:"values,omitempty"`
	Namespace   string                `json:"namespace,omitempty"`
	Version     string                `json:"version,omitempty"`

	// Advanced operation options
	TargetRevision string `json:"targetRevision,omitempty"`
	ForceUpgrade   bool   `json:"forceUpgrade,omitempty"`
	Atomic         bool   `json:"atomic,omitempty"`
	WaitTimeout    int64  `json:"waitTimeout,omitempty"`

	// Pod monitor configuration
	PodMonitor PodMonitorSpec `json:"podMonitor,omitempty"`
}

type ChartSpec struct {
	Repository string `json:"repository,omitempty"`
	Name       string `json:"name,omitempty"`
	Version    string `json:"version,omitempty"`
	LocalPath  string `json:"localPath,omitempty"`
}

type PodMonitorSpec struct {
	Enabled bool `json:"enabled,omitempty"`

	Endpoint string            `json:"endpoint,omitempty"`
	Method   string            `json:"method,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`

	PrometheusAddr string `json:"prometheusAddr,omitempty"`
}

type PodRuntimeStatus struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
	UID       string `json:"uid,omitempty"`
	Phase     string `json:"phase,omitempty"`
	NodeName  string `json:"nodeName,omitempty"`
	PodIP     string `json:"podIP,omitempty"`
	Ready     bool   `json:"ready"`
	Restart   int32  `json:"restart"`
}

type HelmReleaseStatus struct {
	Phase              string             `json:"phase,omitempty"`
	ReleaseName        string             `json:"releaseName,omitempty"`
	ReleaseVersion     int                `json:"releaseVersion,omitempty"`
	ReleaseStatus      string             `json:"releaseStatus,omitempty"`
	LastAppliedTime    *metav1.Time       `json:"lastAppliedTime,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	PodMonitorReady    bool               `json:"podMonitorReady,omitempty"`

	// Extra status info
	Revision               int    `json:"revision,omitempty"`
	LastAttemptedGeneration int64  `json:"lastAttemptedGeneration,omitempty"`
	RetryCount              int    `json:"retryCount,omitempty"`
	LastFailureMessage      string `json:"lastFailureMessage,omitempty"`
	LastTargetRevision string `json:"lastTargetRevision,omitempty"`
	Notes              string `json:"notes,omitempty"`

	// Real-time pod runtime status
	PodStatuses []PodRuntimeStatus `json:"podStatuses,omitempty"`
}

type HelmRelease struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HelmReleaseSpec   `json:"spec,omitempty"`
	Status HelmReleaseStatus `json:"status,omitempty"`
}

type HelmReleaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HelmRelease `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HelmRelease{}, &HelmReleaseList{})
}

func (in *HelmRelease) DeepCopy() *HelmRelease {
	if in == nil {
		return nil
	}
	out := &HelmRelease{}
	in.DeepCopyInto(out)
	return out
}

func (in *HelmRelease) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := &HelmRelease{}
	in.DeepCopyInto(out)
	return out
}

func (in *HelmRelease) DeepCopyInto(out *HelmRelease) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *HelmReleaseSpec) DeepCopyInto(out *HelmReleaseSpec) {
	*out = *in
	if in.Values != nil {
		in, out := &in.Values, &out.Values
		*out = &runtime.RawExtension{}
		**out = **in
	}
	if in.PodMonitor.Headers != nil {
		in, out := &in.PodMonitor.Headers, &out.PodMonitor.Headers
		*out = make(map[string]string, len(*in))
		for key, val := range *in {
			(*out)[key] = val
		}
	}
}

func (in *HelmReleaseStatus) DeepCopyInto(out *HelmReleaseStatus) {
	*out = *in
	if in.LastAppliedTime != nil {
		in, out := &in.LastAppliedTime, &out.LastAppliedTime
		*out = new(metav1.Time)
		**out = **in
	}
	if in.Conditions != nil {
		in, out := &in.Conditions, &out.Conditions
		*out = make([]metav1.Condition, len(*in))
		copy(*out, *in)
	}
	if in.PodStatuses != nil {
		in, out := &in.PodStatuses, &out.PodStatuses
		*out = make([]PodRuntimeStatus, len(*in))
		copy(*out, *in)
	}
}

func (in *HelmReleaseList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := &HelmReleaseList{}
	in.DeepCopyInto(out)
	return out
}

func (in *HelmReleaseList) DeepCopyInto(out *HelmReleaseList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]HelmRelease, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

const (
	PhasePending    = "Pending"
	PhaseInstalling = "Installing"
	PhaseUpgrading  = "Upgrading"
	PhaseRunning    = "Running"
	PhaseFailed     = "Failed"

	MaxRetries = 5
)

const (
	TypeReady  = "Ready"
	TypeSynced = "Synced"
	TypeFailed = "Failed"
)

func (in *HelmRelease) GetReleaseNamespace() string {
	if in.Spec.Namespace != "" {
		return in.Spec.Namespace
	}
	return in.Namespace
}

func (in *HelmRelease) GetReleaseName() string {
	if in.Spec.ReleaseName != "" {
		return in.Spec.ReleaseName
	}
	return in.Name
}

func (in *HelmRelease) IsPodMonitorEnabled() bool {
	return in.Spec.PodMonitor.Enabled
}

func (in *HelmRelease) GetPodMonitorEndpoint() string {
	return in.Spec.PodMonitor.Endpoint
}

func (in *HelmRelease) GetPodMonitorMethod() string {
	if in.Spec.PodMonitor.Method == "" {
		return "POST"
	}
	return in.Spec.PodMonitor.Method
}

func (in *HelmRelease) GetPodMonitorHeaders() map[string]string {
	if in.Spec.PodMonitor.Headers == nil {
		return map[string]string{}
	}
	return in.Spec.PodMonitor.Headers
}

func (in *HelmRelease) GetPrometheusAddr() string {
	return in.Spec.PodMonitor.PrometheusAddr
}

func (in *HelmRelease) ShouldRollback() bool {
	return in.Spec.TargetRevision != "" && in.Spec.TargetRevision != fmt.Sprintf("%d", in.Status.Revision)
}

func (in *HelmRelease) GetTargetRevision() int {
	if in.Spec.TargetRevision == "" {
		return 0
	}
	var rev int
	fmt.Sscanf(in.Spec.TargetRevision, "%d", &rev)
	return rev
}

func (in *HelmRelease) ShouldForceUpgrade() bool {
	return in.Spec.ForceUpgrade
}

func (in *HelmRelease) ShouldAtomic() bool {
	return in.Spec.Atomic
}

func (in *HelmRelease) GetWaitTimeout() time.Duration {
	if in.Spec.WaitTimeout <= 0 {
		return 5 * time.Minute
	}
	return time.Duration(in.Spec.WaitTimeout) * time.Second
}
func (in *HelmRelease) HasRetriesExhausted() bool {
	return in.Status.RetryCount >= MaxRetries
}

func (in *HelmRelease) IsStable() bool {
	return in.Status.Phase == PhaseRunning &&
		in.Status.ObservedGeneration == in.Generation &&
		in.Status.LastAttemptedGeneration == in.Generation
}


func SetCondition(conditions *[]metav1.Condition, condition metav1.Condition) {
	existingIdx := -1
	for i, c := range *conditions {
		if c.Type == condition.Type {
			existingIdx = i
			break
		}
	}

	if existingIdx >= 0 {
		existing := &(*conditions)[existingIdx]
		if existing.Status == condition.Status &&
			existing.Reason == condition.Reason &&
			existing.Message == condition.Message {
			return
		}
		condition.LastTransitionTime = existing.LastTransitionTime
		(*conditions)[existingIdx] = condition
	} else {
		*conditions = append(*conditions, condition)
	}
}

func GetCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

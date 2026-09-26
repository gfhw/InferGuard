package v1alpha1

import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type ModelReleaseSpec struct {
	ReleaseName string                `json:"releaseName,omitempty"`
	Chart       ChartSpec             `json:"chart"`
	Values      *runtime.RawExtension `json:"values,omitempty"`
	Namespace   string                `json:"namespace,omitempty"`
	Version     string                `json:"version,omitempty"`

	// Advanced operation options
	TargetRevision string `json:"targetRevision,omitempty"`
	ForceUpgrade   bool   `json:"forceUpgrade,omitempty"`
	Atomic         bool   `json:"atomic,omitempty"`
	// Wait for pods to be ready after install/upgrade. Defaults to true.
	Wait           *bool  `json:"wait,omitempty"`
	WaitTimeout    int64  `json:"waitTimeout,omitempty"`

	// Pod monitor configuration
	PodMonitor PodMonitorSpec `json:"podMonitor,omitempty"`

	// Policies: alerting rules (condition -> Webhook notification)
	Policies []PolicySpec `json:"policies,omitempty"`
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
}

type PolicyCondition struct {
	Type      string `json:"type"`
	Threshold int32  `json:"threshold"`
	Window    string `json:"window,omitempty"`
	Scope     string `json:"scope,omitempty"`
}

type PolicyAction struct {
	TriggerCount int32  `json:"triggerCount,omitempty"`
	AlertBody    string `json:"alertBody,omitempty"`
}

type PolicySpec struct {
	Name      string          `json:"name"`
	Condition PolicyCondition `json:"condition"`
	Action    PolicyAction    `json:"action"`
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

	// AI-ops diagnostics so `kubectl describe` can show why a pod is unhealthy.
	OOMKilled             bool   `json:"oomKilled,omitempty"`
	LastTerminationReason string `json:"lastTerminationReason,omitempty"`
	Reason                string `json:"reason,omitempty"`
	Message               string `json:"message,omitempty"`
}

type ModelReleaseStatus struct {
	Phase              string             `json:"phase,omitempty"`
	ReleaseName        string             `json:"releaseName,omitempty"`
	ReleaseVersion     int                `json:"releaseVersion,omitempty"`
	ReleaseStatus      string             `json:"releaseStatus,omitempty"`
	LastAppliedTime    *metav1.Time       `json:"lastAppliedTime,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	PodMonitorReady    bool               `json:"podMonitorReady,omitempty"`

	// Extra status info
	Revision               int    `json:"revision,omitempty"`
	LastAttemptedGeneration int64  `json:"lastAttemptedGeneration,omitempty"`
	RetryCount              int    `json:"retryCount,omitempty"`
	LastFailureMessage      string `json:"lastFailureMessage,omitempty"`
	LastTargetRevision      string `json:"lastTargetRevision,omitempty"`
	Notes                   string `json:"notes,omitempty"`

	// Real-time pod runtime status
	PodStatuses []PodRuntimeStatus `json:"podStatuses,omitempty"`
}

type ModelRelease struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelReleaseSpec   `json:"spec,omitempty"`
	Status ModelReleaseStatus `json:"status,omitempty"`
}

type ModelReleaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelRelease `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ModelRelease{}, &ModelReleaseList{})
}

func (in *ModelRelease) DeepCopy() *ModelRelease {
	if in == nil {
		return nil
	}
	out := &ModelRelease{}
	in.DeepCopyInto(out)
	return out
}

func (in *ModelRelease) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := &ModelRelease{}
	in.DeepCopyInto(out)
	return out
}

func (in *ModelRelease) DeepCopyInto(out *ModelRelease) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *ModelReleaseSpec) DeepCopyInto(out *ModelReleaseSpec) {
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
	if in.Policies != nil {
		in, out := &in.Policies, &out.Policies
		*out = make([]PolicySpec, len(*in))
		copy(*out, *in)
	}
}

func (in *ModelReleaseStatus) DeepCopyInto(out *ModelReleaseStatus) {
	*out = *in
	if in.LastAppliedTime != nil {
		in, out := &in.LastAppliedTime, &out.LastAppliedTime
		*out = new(metav1.Time)
		**out = **in
	}
	if in.PodStatuses != nil {
		in, out := &in.PodStatuses, &out.PodStatuses
		*out = make([]PodRuntimeStatus, len(*in))
		copy(*out, *in)
	}
}

func (in *ModelReleaseList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := &ModelReleaseList{}
	in.DeepCopyInto(out)
	return out
}

func (in *ModelReleaseList) DeepCopyInto(out *ModelReleaseList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]ModelRelease, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

const (
	PhasePending         = "Pending"
	PhaseInstalling      = "Installing"
	PhaseUpgrading       = "Upgrading"
	PhaseRunning         = "Running"
	PhaseFailed          = "Failed"
	PhaseUninstallFailed = "UninstallFailed"

	MaxPermanentRetries = 1
	MaxTransientRetries = 10
	MaxRetries          = MaxTransientRetries
)

const (
	TypeReady  = "Ready"
	TypeSynced = "Synced"
	TypeFailed = "Failed"
)

func (in *ModelRelease) GetReleaseNamespace() string {
	if in.Spec.Namespace != "" {
		return in.Spec.Namespace
	}
	return in.Namespace
}

func (in *ModelRelease) GetReleaseName() string {
	if in.Spec.ReleaseName != "" {
		return in.Spec.ReleaseName
	}
	return in.Name
}

func (in *ModelRelease) IsPodMonitorEnabled() bool {
	return in.Spec.PodMonitor.Enabled
}

func (in *ModelRelease) GetPodMonitorEndpoint() string {
	return in.Spec.PodMonitor.Endpoint
}

func (in *ModelRelease) GetPodMonitorMethod() string {
	if in.Spec.PodMonitor.Method == "" {
		return "POST"
	}
	return in.Spec.PodMonitor.Method
}

func (in *ModelRelease) GetPodMonitorHeaders() map[string]string {
	if in.Spec.PodMonitor.Headers == nil {
		return map[string]string{}
	}
	return in.Spec.PodMonitor.Headers
}

func (in *ModelRelease) ShouldRollback() bool {
	return in.Spec.TargetRevision != "" && in.Spec.TargetRevision != fmt.Sprintf("%d", in.Status.Revision)
}

func (in *ModelRelease) GetTargetRevision() int {
	if in.Spec.TargetRevision == "" {
		return 0
	}
	var rev int
	fmt.Sscanf(in.Spec.TargetRevision, "%d", &rev)
	return rev
}

func (in *ModelRelease) ShouldForceUpgrade() bool {
	return in.Spec.ForceUpgrade
}

func (in *ModelRelease) ShouldAtomic() bool {
	return in.Spec.Atomic
}

func (in *ModelRelease) ShouldWait() bool {
	if in.Spec.Wait == nil {
		return true // default: wait
	}
	return *in.Spec.Wait
}

func (in *ModelRelease) GetWaitTimeout() time.Duration {
	if in.Spec.WaitTimeout <= 0 {
		return 5 * time.Minute
	}
	return time.Duration(in.Spec.WaitTimeout) * time.Second
}

// HasRetriesExhausted reports whether we should give up on this CR.
//
// Retries are scoped to one spec generation: once the user edits the spec
// (metadata.generation moves past LastAttemptedGeneration) the previous failures
// no longer describe the current desired state, so the counter is treated as
// stale and reconciliation resumes. Without this, a CR that ever burned through
// its retries would stay dead forever no matter what the user changed.
func (in *ModelRelease) HasRetriesExhausted() bool {
	if in.Status.RetryCount < MaxTransientRetries {
		return false
	}
	return in.Status.LastAttemptedGeneration == in.Generation
}

func (in *ModelRelease) IsStable() bool {
	return in.Status.Phase == PhaseRunning &&
		in.Status.ObservedGeneration == in.Generation &&
		in.Status.LastAttemptedGeneration == in.Generation
}


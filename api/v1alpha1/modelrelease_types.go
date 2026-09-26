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

	// Metrics controls AI metrics scraping into Prometheus.
	Metrics MetricsSpec `json:"metrics,omitempty"`

	// Scheduling declares AI pod scheduling intent (delegated to Volcano).
	Scheduling SchedulingSpec `json:"scheduling,omitempty"`

	// Verification runs robot-framework assertions after deploy/upgrade.
	Verification VerificationSpec `json:"verification,omitempty"`
}

type ChartSpec struct {
	Repository string `json:"repository,omitempty"`
	Name       string `json:"name,omitempty"`
	Version    string `json:"version,omitempty"`
	LocalPath  string `json:"localPath,omitempty"`
}

// MetricsSpec controls whether the operator scrapes AI inference metrics from
// the release's pods and exposes them to Prometheus. Pod status write-back to
// status.podStatuses is always on and is NOT gated by this switch.
type MetricsSpec struct {
	Enabled bool `json:"enabled,omitempty"`
}

// SchedulingSpec declares AI pod scheduling intent. The operator only expresses
// this intent (and creates the Volcano PodGroup); the actual scheduling decision
// is delegated to Volcano / kube-scheduler.
type SchedulingSpec struct {
	// Enabled turns on scheduler intent declaration. When false the release uses
	// the cluster default scheduler.
	Enabled bool `json:"enabled,omitempty"`

	// SchedulerName is the scheduler for the release's pods. Defaults to
	// "volcano" when Enabled.
	SchedulerName string `json:"schedulerName,omitempty"`

	// PodGroup configures Volcano gang scheduling (only relevant when
	// SchedulerName == "volcano").
	PodGroup *PodGroupSpec `json:"podGroup,omitempty"`
}

type PodGroupSpec struct {
	// MinMember is the minimum number of pods that must be scheduled together.
	MinMember int32 `json:"minMember,omitempty"`
	// Queue is the Volcano queue name.
	Queue string `json:"queue,omitempty"`
	// PriorityClass for the pods.
	PriorityClass string `json:"priorityClass,omitempty"`
}

// VerificationSpec declares post-deploy robot-framework verification: after a
// successful install/upgrade, the operator creates an InferenceCheck CR and
// watches it to surface whether the model actually meets its inference SLA.
type VerificationSpec struct {
	// Enabled turns on post-deploy verification. When false the release is
	// considered verified-by-default (status.verification.phase = Skipped).
	Enabled bool `json:"enabled,omitempty"`
	// Suites is the optional list of robot suites to run. Empty means the
	// default smoke suite.
	Suites []string `json:"suites,omitempty"`
}

// FailedCase records a single failed verification assertion.
type FailedCase struct {
	Name     string `json:"name,omitempty"`
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
}

// VerificationPhase values for status.verification.phase.
const (
	// VerificationSkipped: verification is disabled; deploy is verified-by-default.
	VerificationSkipped = "Skipped"
	// VerificationPending: InferenceCheck created, robot cases not yet finished.
	VerificationPending = "Pending"
	// VerificationVerified: all assertions passed.
	VerificationVerified = "Verified"
	// VerificationDegraded: at least one assertion failed.
	VerificationDegraded = "Degraded"
	// VerificationUnknown: verification errored/timed out and cannot be judged.
	VerificationUnknown = "Unknown"
)

// VerificationStatus reflects the robot-framework verification outcome for the
// release. It is independent from status.phase (which tracks the Helm lifecycle
// only).
type VerificationStatus struct {
	Phase            string       `json:"phase,omitempty"`
	VerifiedRevision int          `json:"verifiedRevision,omitempty"`
	PassedCases      int          `json:"passedCases,omitempty"`
	FailedCases      []FailedCase `json:"failedCases,omitempty"`
	ReportURL        string       `json:"reportURL,omitempty"`
	Message          string       `json:"message,omitempty"`
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

	// Verification reflects the robot-framework verification outcome. It is
	// independent from Phase (which tracks the Helm lifecycle only).
	Verification *VerificationStatus `json:"verification,omitempty"`
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
	if in.Scheduling.PodGroup != nil {
		in, out := &in.Scheduling.PodGroup, &out.Scheduling.PodGroup
		*out = new(PodGroupSpec)
		**out = **in
	}
	if in.Verification.Suites != nil {
		in, out := &in.Verification.Suites, &out.Verification.Suites
		*out = make([]string, len(*in))
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
	if in.Verification != nil {
		in, out := &in.Verification, &out.Verification
		*out = new(VerificationStatus)
		(*in).DeepCopyInto(*out)
	}
}

// DeepCopy is a convenience copy for VerificationStatus.
func (in *VerificationStatus) DeepCopyInto(out *VerificationStatus) {
	*out = *in
	if in.FailedCases != nil {
		in, out := &in.FailedCases, &out.FailedCases
		*out = make([]FailedCase, len(*in))
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

// IsVerificationEnabled reports whether post-deploy robot verification should run.
func (in *ModelRelease) IsVerificationEnabled() bool {
	return in.Spec.Verification.Enabled
}

// UseVolcanoScheduler reports whether the release declares Volcano scheduling.
func (in *ModelRelease) UseVolcanoScheduler() bool {
	return in.Spec.Scheduling.Enabled
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


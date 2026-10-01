package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// InferenceCheckSpec declares a robot-framework verification run for an AI
// inference release. InferGuard creates one after a successful deploy/upgrade;
// InferVerify executes it and writes back status.
type InferenceCheckSpec struct {
	// ReleaseRef is the ModelRelease this check verifies.
	ReleaseRef string `json:"releaseRef"`
	// Revision is the Helm revision being verified.
	Revision int `json:"revision,omitempty"`
	// Engine is the inference engine (vllm/tgi/sglang).
	Engine string `json:"engine,omitempty"`
	// Target is the inference service URL.
	Target string `json:"target"`
	// Suites lists the robot suites to run (defaults to smoke).
	Suites []string `json:"suites,omitempty"`
	// Thresholds are assertion thresholds passed to the suites as variables.
	Thresholds map[string]int32 `json:"thresholds,omitempty"`
}

// FailedCase records a single failed verification assertion.
type FailedCase struct {
	Name    string `json:"name,omitempty"`
	Message string `json:"message,omitempty"`
}

// InferenceCheckStatus mirrors InferVerify's status output.
type InferenceCheckStatus struct {
	Phase       string       `json:"phase,omitempty"`
	PassedCases int          `json:"passedCases,omitempty"`
	FailedCases []FailedCase `json:"failedCases,omitempty"`
	ReportURL   string       `json:"reportURL,omitempty"`
	Message     string       `json:"message,omitempty"`
}

type InferenceCheck struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   InferenceCheckSpec   `json:"spec,omitempty"`
	Status InferenceCheckStatus `json:"status,omitempty"`
}

type InferenceCheckList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []InferenceCheck `json:"items"`
}

func init() {
	SchemeBuilder.Register(&InferenceCheck{}, &InferenceCheckList{})
}

func (in *InferenceCheck) DeepCopyInto(out *InferenceCheck) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *InferenceCheck) DeepCopy() *InferenceCheck {
	if in == nil {
		return nil
	}
	out := new(InferenceCheck)
	in.DeepCopyInto(out)
	return out
}

func (in *InferenceCheck) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(InferenceCheck)
	in.DeepCopyInto(out)
	return out
}

func (in *InferenceCheckSpec) DeepCopyInto(out *InferenceCheckSpec) {
	*out = *in
	if in.Suites != nil {
		in, out := &in.Suites, &out.Suites
		*out = make([]string, len(*in))
		copy(*out, *in)
	}
	if in.Thresholds != nil {
		in, out := &in.Thresholds, &out.Thresholds
		*out = make(map[string]int32, len(*in))
		for key, val := range *in {
			(*out)[key] = val
		}
	}
}

func (in *InferenceCheckStatus) DeepCopyInto(out *InferenceCheckStatus) {
	*out = *in
	if in.FailedCases != nil {
		in, out := &in.FailedCases, &out.FailedCases
		*out = make([]FailedCase, len(*in))
		copy(*out, *in)
	}
}

func (in *InferenceCheckList) DeepCopyInto(out *InferenceCheckList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]InferenceCheck, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

func (in *InferenceCheckList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(InferenceCheckList)
	in.DeepCopyInto(out)
	return out
}

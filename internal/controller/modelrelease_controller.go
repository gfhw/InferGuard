package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/kubernetes"
	helmrelease "helm.sh/helm/v3/pkg/release"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrlLog "sigs.k8s.io/controller-runtime/pkg/log"

	helmv1alpha1 "github.com/gfhw/inferguard/api/v1alpha1"
	"github.com/gfhw/inferguard/internal/helm"
	inferguardlog "github.com/gfhw/inferguard/pkg/log"
	"github.com/gfhw/inferguard/pkg/podwatch"
)

const (
	modelReleaseFinalizer = "inferguard.io/finalizer"
	maxBackoff           = 10 * time.Minute
)

type ModelReleaseReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	HelmManager *helm.Manager
	K8sClient   kubernetes.Interface

	Watcher      *podwatch.GlobalPodWatcher
	releaseNames map[string]string
}

func NewModelReleaseReconciler(client client.Client, scheme *runtime.Scheme, k8sClient kubernetes.Interface, watcher *podwatch.GlobalPodWatcher) *ModelReleaseReconciler {
	return &ModelReleaseReconciler{
		Client:       client,
		Scheme:       scheme,
		HelmManager:  helm.NewManager(),
		K8sClient:    k8sClient,
		Watcher:      watcher,
		releaseNames: make(map[string]string),
	}
}

func (r *ModelReleaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = ctrlLog.FromContext(ctx)

	hr := &helmv1alpha1.ModelRelease{}
	if err := r.Get(ctx, req.NamespacedName, hr); err != nil {
		if apierrors.IsNotFound(err) {
			key := req.NamespacedName.String()
			if releaseName, ok := r.releaseNames[key]; ok {
				r.Watcher.UnregisterRelease(releaseName)
				delete(r.releaseNames, key)
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion.
	if !hr.ObjectMeta.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(hr, modelReleaseFinalizer) {
			// Reset retry counter on first deletion attempt (it may be stale from install failures).
			deletionRetries := hr.Status.RetryCount
			if hr.Status.Phase != "UninstallFailed" {
				deletionRetries = 0
			}

			if err := r.finalizeModelRelease(ctx, hr); err != nil {
				deletionRetries++
				updated := hr.DeepCopy()
				updated.Status.RetryCount = deletionRetries
				updated.Status.Phase = "UninstallFailed"
				updated.Status.LastFailureMessage = err.Error()
				r.Status().Update(ctx, updated)

				if deletionRetries >= helmv1alpha1.MaxTransientRetries {
					inferguardlog.Info("Helm uninstall failed after max retries, keeping CR as tombstone. Fix the underlying issue, then delete again.",
						"release", hr.GetReleaseName(), "retries", deletionRetries, "error", err.Error())
					// CR stays with finalizer 锟?it blocks deletion but preserves visibility.
					// User must fix the Helm release and re-delete the CR.
					return ctrl.Result{}, nil
				}
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(hr, modelReleaseFinalizer)
			if err := r.Update(ctx, hr); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Ensure the finalizer is present before any early return below.
	//
	// This must run before the IsStable / retries-exhausted short circuits: a CR
	// that becomes Running (or gives up) without ever having the finalizer added
	// would return early forever, so deleting it would skip helm uninstall and
	// leak the release and its GPU resources.
	if !controllerutil.ContainsFinalizer(hr, modelReleaseFinalizer) {
		controllerutil.AddFinalizer(hr, modelReleaseFinalizer)
		if err := r.Update(ctx, hr); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Stable: spec unchanged and already Running — nothing to do.
	if hr.IsStable() {
		return ctrl.Result{}, nil
	}

	// Retries exhausted: permanent failure, or too many transient failures for
	// this spec generation. Editing the spec bumps generation and wakes us again.
	if hr.HasRetriesExhausted() {
		if hr.Status.LastAttemptedGeneration != hr.Generation {
			// Record which generation we gave up on. Without this the next spec
			// edit would look like a fresh, never-attempted generation and the
			// exhausted counter would keep suppressing reconciliation.
			abandoned := hr.DeepCopy()
			abandoned.Status.LastAttemptedGeneration = hr.Generation
			if err := r.Status().Update(ctx, abandoned); err != nil {
				inferguardlog.ErrorE(err, "Failed to record abandoned generation",
					"name", hr.Name, "generation", hr.Generation)
			}
		}
		inferguardlog.Info("Retries exhausted, giving up until the spec changes",
			"name", hr.Name,
			"retryCount", hr.Status.RetryCount,
			"generation", hr.Generation,
			"lastError", hr.Status.LastFailureMessage)
		return ctrl.Result{}, nil
	}

	// Attempt reconciliation.
	if err := r.reconcileModelRelease(ctx, hr); err != nil {
		backoff := time.Duration(1<<hr.Status.RetryCount) * time.Second
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
		inferguardlog.Info("Reconciliation failed, retrying with backoff",
			"name", hr.Name,
			"retryCount", hr.Status.RetryCount,
			"backoff", backoff)
		return ctrl.Result{RequeueAfter: backoff}, nil
	}

	// Success 锟?no periodic requeue. Wait for spec change.
	return ctrl.Result{}, nil
}

func (r *ModelReleaseReconciler) reconcileModelRelease(ctx context.Context, hr *helmv1alpha1.ModelRelease) error {
	if hr.ShouldRollback() {
		return r.performRollback(ctx, hr)
	}
	return r.performInstallOrUpgrade(ctx, hr)
}

func (r *ModelReleaseReconciler) performInstallOrUpgrade(ctx context.Context, hr *helmv1alpha1.ModelRelease) error {
	releaseName := hr.GetReleaseName()
	releaseNs := hr.GetReleaseNamespace()

	values, err := r.getValues(hr)
	if err != nil {
		r.updateStatusFailed(ctx, hr, "values parsing failed: "+err.Error())
		return err
	}

	existing, err := r.HelmManager.GetRelease(releaseName, releaseNs)
	if err != nil && !errors.Is(err, helm.ErrReleaseNotFound) {
		r.updateStatusFailed(ctx, hr, "failed to query release: "+err.Error())
		return err
	}

	if existing == nil {
		r.updateStatusPhase(ctx, hr, helmv1alpha1.PhaseInstalling)
	} else {
		r.updateStatusPhase(ctx, hr, helmv1alpha1.PhaseUpgrading)
	}

	var rel *helmrelease.Release
	if hr.Spec.Chart.LocalPath != "" {
		rel, err = r.HelmManager.InstallFromLocal(ctx, releaseName, releaseNs, hr.Spec.Chart.LocalPath, values, hr.GetWaitTimeout(), hr.ShouldForceUpgrade(), hr.ShouldAtomic(), hr.ShouldWait())
	} else {
		rel, err = r.HelmManager.InstallOrUpgrade(ctx, releaseName, releaseNs, hr.Spec.Chart.Repository, hr.Spec.Chart.Name, hr.Spec.Chart.Version, values, hr.GetWaitTimeout(), hr.ShouldForceUpgrade(), hr.ShouldAtomic(), hr.ShouldWait())
	}
	if err != nil {

		if !isRetryable(err) {
			inferguardlog.Info("Permanent failure detected, giving up",
				"release", releaseName, "error", err.Error())
			// Combine status update: set failed + mark retries exhausted.
			updated := hr.DeepCopy()
			updated.Status.Phase = helmv1alpha1.PhaseFailed
			updated.Status.LastAttemptedGeneration = hr.Generation
			updated.Status.RetryCount = helmv1alpha1.MaxTransientRetries
			updated.Status.LastFailureMessage = err.Error()
			r.Status().Update(ctx, updated)
		} else {
			r.updateStatusFailed(ctx, hr, err.Error())
		}

		return err
	}

	if err := r.updateStatusSuccess(ctx, hr, rel); err != nil {
		return err
	}

	// Register pod monitoring before updating the revision: RegisterRelease
	// installs a fresh ReleaseConfig, which would otherwise reset Revision to 0.
	if err := r.managePodMonitor(ctx, hr); err != nil {
		return err
	}
	if rel != nil {
		r.Watcher.UpdateReleaseRevision(hr.GetReleaseName(), rel.Version)
	}
	return nil
}

func (r *ModelReleaseReconciler) performRollback(ctx context.Context, hr *helmv1alpha1.ModelRelease) error {
	releaseName := hr.GetReleaseName()
	releaseNs := hr.GetReleaseNamespace()
	targetRevision := hr.GetTargetRevision()

	inferguardlog.Info("Performing rollback",
		"release", releaseName, "namespace", releaseNs, "targetRevision", targetRevision)

	r.updateStatusPhase(ctx, hr, helmv1alpha1.PhaseInstalling)

	if err := r.HelmManager.Rollback(releaseName, releaseNs, targetRevision, hr.GetWaitTimeout()); err != nil {
		r.updateStatusFailed(ctx, hr, err.Error())
		return err
	}

	rel, err := r.HelmManager.GetRelease(releaseName, releaseNs)
	if err != nil {
		r.updateStatusFailed(ctx, hr, "failed to get release after rollback: "+err.Error())
		return err
	}

	if err := r.updateStatusSuccess(ctx, hr, rel); err != nil {
		return err
	}

	// Fetch a fresh copy of the CR (status was already updated above, which changed resourceVersion).
	// Then align the spec with the rolled-back state.
	fresh := &helmv1alpha1.ModelRelease{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: hr.Namespace, Name: hr.Name}, fresh); err != nil {
		inferguardlog.ErrorE(err, "Failed to re-fetch CR after rollback", "release", releaseName)
	} else {
		fresh.Spec.TargetRevision = ""
		if rel.Chart != nil && rel.Chart.Metadata != nil {
			fresh.Spec.Chart.Version = rel.Chart.Metadata.Version
		}
		// Sync values from the rolled-back release so CR spec matches reality.
		if rel.Config != nil && len(rel.Config) > 0 {
			valuesJSON, marshalErr := json.Marshal(rel.Config)
			if marshalErr == nil {
				fresh.Spec.Values = &runtime.RawExtension{Raw: valuesJSON}
			}
		}
		if err := r.Update(ctx, fresh); err != nil {
			inferguardlog.ErrorE(err, "Failed to update spec after rollback", "release", releaseName)
		} else {
			// Spec update incremented Generation. Align ObservedGeneration so
			// the next Reconcile sees IsStable()=true without a no-op round.
			fresh.Status.ObservedGeneration = fresh.Generation
			fresh.Status.LastAttemptedGeneration = fresh.Generation
			if err := r.Status().Update(ctx, fresh); err != nil {
				inferguardlog.ErrorE(err, "Failed to align status after rollback spec sync", "release", releaseName)
			}
		}
	}


	if err := r.managePodMonitor(ctx, hr); err != nil {
		return err
	}
	if rel != nil {
		r.Watcher.UpdateReleaseRevision(hr.GetReleaseName(), rel.Version)
	}
	return nil
}

func (r *ModelReleaseReconciler) managePodMonitor(ctx context.Context, hr *helmv1alpha1.ModelRelease) error {
	releaseName := hr.GetReleaseName()

	key := types.NamespacedName{Namespace: hr.Namespace, Name: hr.Name}.String()

	if !hr.IsPodMonitorEnabled() {
		r.Watcher.UnregisterRelease(releaseName)
		delete(r.releaseNames, key)
		return nil
	}

	r.Watcher.RegisterRelease(releaseName, &podwatch.ReleaseConfig{
		EventSender: podwatch.NewEventSenderWithConfig(
			hr.GetPodMonitorEndpoint(),
			hr.GetPodMonitorMethod(),
			hr.GetPodMonitorHeaders(),
		),
		StatusUpdater: &crStatusUpdater{
			client:      r.Client,
			crNamespace: hr.Namespace,
			crName:      hr.Name,
		},
		Policies: hr.Spec.Policies,
	})

	r.releaseNames[key] = releaseName
	return nil
}

func (r *ModelReleaseReconciler) unmanagePodMonitor(releaseName string) {
	r.Watcher.UnregisterRelease(releaseName)
}

func (r *ModelReleaseReconciler) getValues(hr *helmv1alpha1.ModelRelease) (map[string]interface{}, error) {
	if hr.Spec.Values == nil {
		return map[string]interface{}{}, nil
	}

	raw := hr.Spec.Values.Raw
	if len(raw) == 0 {
		return map[string]interface{}{}, nil
	}

	var values map[string]interface{}
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("failed to unmarshal values: %w", err)
	}

	return values, nil
}

// isRetryable checks whether a Helm operation error is transient (retry) or permanent (give up immediately).
func isRetryable(err error) bool {
	msg := strings.ToLower(err.Error())

	permanent := []string{
		"values parsing failed",
		"failed to load chart",
		"failed to unmarshal",
		"chart not found",
		"no chart version found",
		"failed to render template",
		"imagepullbackoff",
		"errimagepull",
	}
	for _, p := range permanent {
		if strings.Contains(msg, p) {
			return false
		}
	}

	transient := []string{
		"connection refused",
		"timeout",
		"context deadline exceeded",
		"dial tcp",
		"connection reset",
		"too many requests",
		"tls handshake timeout",
	}
	for _, t := range transient {
		if strings.Contains(msg, t) {
			return true
		}
	}

	return true
}

func (r *ModelReleaseReconciler) finalizeModelRelease(ctx context.Context, hr *helmv1alpha1.ModelRelease) error {
	releaseName := hr.GetReleaseName()
	releaseNs := hr.GetReleaseNamespace()

	key := types.NamespacedName{Namespace: hr.Namespace, Name: hr.Name}.String()
	r.Watcher.UnregisterRelease(releaseName)
	delete(r.releaseNames, key)

	_, err := r.HelmManager.GetRelease(releaseName, releaseNs)
	if err != nil {
		if errors.Is(err, helm.ErrReleaseNotFound) {
			// Nothing was ever installed; nothing to uninstall.
			return nil
		}
		return fmt.Errorf("failed to query release before uninstall: %w", err)
	}

	if err := r.HelmManager.Uninstall(releaseName, releaseNs); err != nil {
		return fmt.Errorf("failed to uninstall release: %w", err)
	}

	return nil
}

func (r *ModelReleaseReconciler) updateStatusPhase(ctx context.Context, hr *helmv1alpha1.ModelRelease, phase string) {
	updated := hr.DeepCopy()
	updated.Status.Phase = phase
	if err := r.Status().Update(ctx, updated); err != nil {
		inferguardlog.ErrorE(err, "Failed to update status phase", "phase", phase)
	}
}

func (r *ModelReleaseReconciler) updateStatusSuccess(ctx context.Context, hr *helmv1alpha1.ModelRelease, rel interface{}) error {
	updated := hr.DeepCopy()
	updated.Status.Phase = helmv1alpha1.PhaseRunning
	updated.Status.ReleaseName = hr.GetReleaseName()
	updated.Status.ReleaseStatus = "deployed"
	updated.Status.ObservedGeneration = hr.Generation
	updated.Status.LastAttemptedGeneration = hr.Generation
	updated.Status.RetryCount = 0
	updated.Status.LastFailureMessage = ""
	now := metav1.Now()
	updated.Status.LastAppliedTime = &now

	if release, ok := rel.(*helmrelease.Release); ok {
		updated.Status.Revision = release.Version
		updated.Status.ReleaseVersion = release.Version
		updated.Status.LastTargetRevision = fmt.Sprintf("%d", release.Version)
		if release.Info != nil {
			updated.Status.Notes = release.Info.Notes
		}
	}

	if hr.IsPodMonitorEnabled() {
		updated.Status.PodMonitorReady = true
	}

	if err := r.Status().Update(ctx, updated); err != nil {
		return err
	}
	return nil
}

func (r *ModelReleaseReconciler) updateStatusFailed(ctx context.Context, hr *helmv1alpha1.ModelRelease, message string) {
	updated := hr.DeepCopy()
	updated.Status.Phase = helmv1alpha1.PhaseFailed
	updated.Status.LastAttemptedGeneration = hr.Generation
	updated.Status.RetryCount = hr.Status.RetryCount + 1
	updated.Status.LastFailureMessage = message
	if err := r.Status().Update(ctx, updated); err != nil {
		inferguardlog.ErrorE(err, "Failed to update status")
	}
}

func (r *ModelReleaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&helmv1alpha1.ModelRelease{}).
		Complete(r)
}

// crStatusUpdater implements podwatch.StatusUpdater to write pod status back to the CR
type crStatusUpdater struct {
	client      client.Client
	crNamespace string
	crName      string
}

func (u *crStatusUpdater) UpdatePodStatus(ctx context.Context, releaseName string, podInfo podwatch.PodInfo, eventType podwatch.PodEventType) error {
	hr := &helmv1alpha1.ModelRelease{}
	key := types.NamespacedName{Namespace: u.crNamespace, Name: u.crName}
	if err := u.client.Get(ctx, key, hr); err != nil {
		if apierrors.IsNotFound(err) {
			inferguardlog.Info("ModelRelease not found, skipping status update",
				"namespace", u.crNamespace, "name", u.crName)
			return nil
		}
		return fmt.Errorf("failed to get ModelRelease: %w", err)
	}

	updated := hr.DeepCopy()

	podStatus := helmv1alpha1.PodRuntimeStatus{
		Namespace:             podInfo.Namespace,
		Name:                  podInfo.Name,
		UID:                   podInfo.UID,
		Phase:                 podInfo.Phase,
		NodeName:              podInfo.NodeName,
		PodIP:                 podInfo.PodIP,
		Ready:                 podInfo.Ready,
		Restart:               podInfo.Restart,
		OOMKilled:             podInfo.OOMKilled,
		LastTerminationReason: podInfo.LastTerminationReason,
		Reason:                podInfo.Reason,
		Message:               podInfo.Message,
	}

	if eventType == podwatch.PodEventDeleted {
		filtered := make([]helmv1alpha1.PodRuntimeStatus, 0, len(updated.Status.PodStatuses))
		for _, ps := range updated.Status.PodStatuses {
			if ps.Namespace == podInfo.Namespace && ps.Name == podInfo.Name {
				continue
			}
			filtered = append(filtered, ps)
		}
		updated.Status.PodStatuses = filtered
	} else {
		found := false
		for i, ps := range updated.Status.PodStatuses {
			if ps.Namespace == podInfo.Namespace && ps.Name == podInfo.Name {
				updated.Status.PodStatuses[i] = podStatus
				found = true
				break
			}
		}
		if !found {
			updated.Status.PodStatuses = append(updated.Status.PodStatuses, podStatus)
		}
	}

	if err := u.client.Status().Update(ctx, updated); err != nil {
		return fmt.Errorf("failed to update CR pod statuses: %w", err)
	}

	return nil
}


